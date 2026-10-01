// Package validate kiểm tra tĩnh artifact của bot (M1, S1.5.2) — không gọi LLM, chạy đồng bộ (< 300 ms).
// Thiết kế: docs/mcp.md §4. Lỗi luôn có mã, dòng, item liên quan để AI tự sửa.
//
//	UNCITED            khối (đoạn / mục danh sách) mang thông tin nhưng không có [[id]]
//	STALE_CITATION     trích dẫn item không còn active (superseded / expired / retracted...)
//	MISSING_LOCKED     system_prompt thiếu quy tắc bắt buộc (L0/L1 locked)
//	UNLABELED_DEFAULT  dùng thông lệ L1 mà câu không nói rõ là thông lệ
//	HARDCODED_DATA     ghi cứng giá tiền / giờ chạy thay vì hướng bot gọi tool
//	COVERAGE           system_prompt chưa phủ mục bắt buộc của template mà nhà xe đã có tri thức
//
// Lỗi (errors) làm artifact `invalid`; cảnh báo (warnings) không chặn. NO_CAPABILITY (M2), CONTRADICTION (M3, LLM).
package validate

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/artifact"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

type Issue struct {
	Code         string `json:"code"`
	Line         int    `json:"line,omitempty"`
	Item         string `json:"item,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	Topic        string `json:"topic,omitempty"`
	Excerpt      string `json:"excerpt,omitempty"`
	Message      string `json:"message"`
}

type Report struct {
	ArtifactID  string    `json:"artifact_id"`
	Kind        string    `json:"kind"`
	Version     int       `json:"version"`
	Valid       bool      `json:"valid"`
	Status      string    `json:"status"`
	Errors      []Issue   `json:"errors"`
	Warnings    []Issue   `json:"warnings"`
	CheckedAt   time.Time `json:"checked_at"`
	NextActions []string  `json:"next_actions"`
}

type TopicSpec struct {
	ID       string
	Title    string
	Required bool
}

type itemInfo struct {
	status, topic, supersededBy string
	layer                       int
	locked                      bool
	validFrom, validTo          *time.Time
}

var (
	// Tiền (bắt buộc có đơn vị): 320.000đ, 320,000 VND, 320k, 1,2 triệu, 300 nghìn. Giờ: 22:00, 22h, 22h30,
	// 7 giờ tối. Sau đơn vị không được là chữ cái (để "5 đêm", "20kg", "2 trẻ" không bị bắt).
	moneyRe = regexp.MustCompile(`(?i)\b\d+(?:[.,]\d+)*\s*(?:đồng|đ|₫|vn[dđ]|k|nghìn|ngàn|triệu|tr)(?:$|[^\p{L}])`)
	clockRe = regexp.MustCompile(`(?i)\b(?:[01]?\d|2[0-3])\s*[:h]\s*[0-5]\d\b|\b(?:[01]?\d|2[0-3])h(?:$|[^\p{L}\d])|\b(?:[01]?\d|2[0-3])\s*giờ\s*(?:sáng|chiều|tối|đêm|trưa)`)
	listRe  = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s+`)
	// Câu nói rõ là thông lệ (đã chuẩn hoá không dấu).
	defaultPhrases = []string{"thong le", "thuong", "nhieu nha xe", "xac nhan lai", "chua xac nhan", "tham khao"}
)

// block: một đoạn văn hoặc một mục danh sách (có thể nhiều dòng) — trích dẫn ở dòng nào trong khối cũng tính.
type block struct {
	start, end int // dòng 1-based
	text       string
	heading    string // tiêu đề gần nhất phía trên
}

func blocks(content string) []block {
	var out []block
	var cur *block
	heading := ""
	inCode := false
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for i, line := range strings.Split(content, "\n") {
		n := i + 1
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flush()
			inCode = !inCode
			continue
		}
		if inCode {
			continue // code/tool schema trong tool_spec: không kiểm câu
		}
		switch {
		case trimmed == "" || trimmed == "---":
			flush()
		case strings.HasPrefix(trimmed, "#"):
			flush()
			heading = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		case listRe.MatchString(line) || cur == nil:
			flush()
			cur = &block{start: n, end: n, text: trimmed, heading: heading}
		default:
			cur.end = n
			cur.text += " " + trimmed
		}
	}
	flush()
	return out
}

// informative: mức "mang thông tin" của khối — 2: chắc chắn (có chữ số, hoặc ≥ 12 từ) → lỗi nếu thiếu trích dẫn;
// 1: có thể (6–11 từ, vd câu dẫn/chuyển hướng ngắn) → cảnh báo; 0: không (câu hỏi FAQ, nhãn, câu rất ngắn).
func informative(b block) int {
	t := citeStrip(b.text)
	t = strings.TrimSpace(listRe.ReplaceAllString(t, ""))
	low := strings.ToLower(t)
	if strings.HasSuffix(t, "?") || strings.HasPrefix(low, "hỏi:") || strings.HasPrefix(low, "q:") {
		return 0
	}
	words := len(strings.Fields(t))
	switch {
	case words >= 12 || digitRe.MatchString(t):
		return 2
	case words >= 6:
		return 1
	}
	return 0
}

var digitRe = regexp.MustCompile(`\d`)

var citeAnyRe = regexp.MustCompile(`\[\[[^\[\]]*\]\]`)

func citeStrip(s string) string { return citeAnyRe.ReplaceAllString(s, "") }

func excerpt(s string) string {
	s = strings.TrimSpace(citeStrip(s))
	if r := []rune(s); len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}

// Validate kiểm một version artifact và ghi kết quả (status valid/invalid + validation) vào bot_artifacts.
func Validate(ctx context.Context, db *pgxpool.Pool, operatorID string, a artifact.Artifact, topics []TopicSpec) (Report, error) {
	rep := Report{ArtifactID: a.ID, Kind: a.Kind, Version: a.Version, Errors: []Issue{}, Warnings: []Issue{},
		CheckedAt: time.Now().UTC(), NextActions: []string{}}

	// Thông tin item được trích dẫn.
	ids := []string{}
	for _, c := range a.Citations {
		if !slices.Contains(ids, c.ItemID) {
			ids = append(ids, c.ItemID)
		}
	}
	items := map[string]itemInfo{}
	if len(ids) > 0 {
		rows, err := db.Query(ctx, `SELECT id::text, status, topic, COALESCE(superseded_by::text, ''), layer, locked,
				valid_from, valid_to
			FROM items WHERE id = ANY($1::uuid[])`, ids)
		if err != nil {
			return rep, err
		}
		for rows.Next() {
			var id string
			var it itemInfo
			if err := rows.Scan(&id, &it.status, &it.topic, &it.supersededBy, &it.layer, &it.locked, &it.validFrom,
				&it.validTo); err != nil {
				rows.Close()
				return rep, err
			}
			items[id] = it
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
	}
	citesByLine := map[int][]string{}
	for _, c := range a.Citations {
		citesByLine[c.Line] = append(citesByLine[c.Line], c.ItemID)
	}

	now := time.Now()
	// STALE_CITATION: theo từng trích dẫn.
	staleSeen := map[string]bool{}
	for _, c := range a.Citations {
		it, ok := items[c.ItemID]
		key := c.ItemID + "@" + itoa(c.Line)
		if staleSeen[key] {
			continue
		}
		staleSeen[key] = true
		switch {
		case !ok:
			rep.Errors = append(rep.Errors, Issue{Code: "STALE_CITATION", Line: c.Line, Item: c.ItemID,
				Message: "item không còn tồn tại"})
		case it.status != "active":
			msg := "item đã " + it.status + "; tìm bản thay thế bằng recall_knowledge"
			if it.supersededBy != "" {
				msg = "item đã được thay; trích dẫn [[" + it.supersededBy + "]] và cập nhật nội dung theo bản mới"
			}
			rep.Errors = append(rep.Errors, Issue{Code: "STALE_CITATION", Line: c.Line, Item: c.ItemID,
				SupersededBy: it.supersededBy, Message: msg})
		case it.validTo != nil && it.validTo.Before(now):
			rep.Errors = append(rep.Errors, Issue{Code: "STALE_CITATION", Line: c.Line, Item: c.ItemID,
				Message: "item đã hết hiệu lực (valid_to " + it.validTo.Format("2006-01-02") + ")"})
		case it.validFrom != nil && it.validFrom.After(now):
			rep.Warnings = append(rep.Warnings, Issue{Code: "STALE_CITATION", Line: c.Line, Item: c.ItemID,
				Message: "item chưa có hiệu lực (từ " + it.validFrom.Format("2006-01-02") + "): câu phải nói rõ áp dụng từ ngày đó"})
		}
	}

	// Thông lệ L1 được trích nhưng nhà xe nay đã có tri thức riêng cho topic đó (L2 thay L1) → STALE_CITATION.
	var defaultTopics []string
	for _, it := range items {
		if it.layer == 1 && !it.locked && it.status == "active" && !slices.Contains(defaultTopics, it.topic) {
			defaultTopics = append(defaultTopics, it.topic)
		}
	}
	overriddenBy := map[string][]string{}
	if len(defaultTopics) > 0 {
		rows, err := db.Query(ctx, `SELECT topic, array_agg(id::text ORDER BY key, id) FROM items
			WHERE operator_id = $1 AND layer = 2 AND status = 'active' AND kind <> 'data' AND topic = ANY($2)
			GROUP BY topic`, operatorID, defaultTopics)
		if err != nil {
			return rep, err
		}
		for rows.Next() {
			var topic string
			var ids []string
			if err := rows.Scan(&topic, &ids); err != nil {
				rows.Close()
				return rep, err
			}
			overriddenBy[topic] = ids
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
	}
	for _, c := range a.Citations {
		it := items[c.ItemID]
		if it.layer != 1 || it.locked || it.status != "active" || len(overriddenBy[it.topic]) == 0 {
			continue
		}
		own := overriddenBy[it.topic]
		refs := make([]string, len(own))
		for i, id := range own {
			refs[i] = "[[" + id + "]]"
		}
		rep.Errors = append(rep.Errors, Issue{Code: "STALE_CITATION", Line: c.Line, Item: c.ItemID, Topic: it.topic,
			SupersededBy: own[0], Message: "nhà xe đã có tri thức riêng cho topic " + it.topic + " (" +
				strings.Join(refs, ", ") + "): thay thông lệ chung bằng thông tin của nhà xe"})
	}

	// Kiểm theo khối.
	for _, b := range blocks(a.Content) {
		var cited []string
		for n := b.start; n <= b.end; n++ {
			cited = append(cited, citesByLine[n]...)
		}
		if len(cited) == 0 && a.Kind != "tool_spec" {
			switch informative(b) {
			case 2:
				rep.Errors = append(rep.Errors, Issue{Code: "UNCITED", Line: b.start, Excerpt: excerpt(b.text),
					Message: "câu mang thông tin nhưng không có trích dẫn [[id]]; nếu không có item nào làm nguồn thì bỏ câu này"})
			case 1:
				rep.Warnings = append(rep.Warnings, Issue{Code: "UNCITED", Line: b.start, Excerpt: excerpt(b.text),
					Message: "nếu câu này nêu thông tin của nhà xe/ngành thì thêm [[id]]"})
			}
		}
		folded := textnorm.Fold(citeStrip(b.text) + " " + b.heading)
		for _, id := range cited {
			it, ok := items[id]
			if ok && it.layer == 1 && !it.locked && !containsAny(folded, defaultPhrases) {
				rep.Errors = append(rep.Errors, Issue{Code: "UNLABELED_DEFAULT", Line: b.start, Item: id,
					Excerpt: excerpt(b.text), Message: "đây là thông lệ chung của ngành (nhà xe chưa xác nhận): câu " +
						"phải nói rõ, vd 'thông thường…, anh/chị vui lòng xác nhận lại với nhà xe'"})
			}
		}
		text := citeStrip(b.text)
		if m := strings.TrimRight(moneyRe.FindString(text), " ,.;:)"); m != "" {
			rep.Errors = append(rep.Errors, Issue{Code: "HARDCODED_DATA", Line: b.start, Excerpt: strings.TrimSpace(m),
				Message: "không ghi cứng giá tiền: hướng bot gọi tool tra giá (khai báo trong tool_spec)"})
		} else if m := strings.TrimRight(clockRe.FindString(text), " ,.;:)"); m != "" {
			rep.Errors = append(rep.Errors, Issue{Code: "HARDCODED_DATA", Line: b.start, Excerpt: strings.TrimSpace(m),
				Message: "không ghi cứng giờ chạy: hướng bot gọi tool tra lịch (khai báo trong tool_spec)"})
		}
	}

	if a.Kind == "system_prompt" {
		if err := checkSystemPrompt(ctx, db, operatorID, a, items, topics, &rep); err != nil {
			return rep, err
		}
	}

	rep.Valid = len(rep.Errors) == 0
	rep.Status = map[bool]string{true: "valid", false: "invalid"}[rep.Valid]
	if rep.Valid {
		rep.NextActions = append(rep.NextActions, "list_artifacts — viết tiếp artifact còn thiếu")
	} else {
		rep.NextActions = append(rep.NextActions, "sửa từng lỗi theo dòng (get_artifact để xem nội dung)",
			"save_artifact(base_version="+itoa(a.Version)+")", "validate_artifact lại")
	}
	stored := map[string]any{"valid": rep.Valid, "errors": rep.Errors, "warnings": rep.Warnings,
		"checked_at": rep.CheckedAt}
	_, err := db.Exec(ctx, `UPDATE bot_artifacts SET status = $2, validation = $3, updated_at = now()
		WHERE id = $1 AND status IN ('draft', 'valid', 'invalid')`, a.ID, rep.Status, stored)
	return rep, err
}

func checkSystemPrompt(ctx context.Context, db *pgxpool.Pool, operatorID string, a artifact.Artifact,
	items map[string]itemInfo, topics []TopicSpec, rep *Report) error {
	cited := map[string]bool{}
	citedTopics := map[string]bool{}
	for _, c := range a.Citations {
		cited[c.ItemID] = true
		if it, ok := items[c.ItemID]; ok && it.status == "active" {
			citedTopics[it.topic] = true
		}
	}
	// MISSING_LOCKED.
	rows, err := db.Query(ctx, `SELECT id::text, COALESCE(key, ''), text FROM items
		WHERE status = 'active' AND layer <= 1 AND locked ORDER BY layer, key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, key, text string
		if err := rows.Scan(&id, &key, &text); err != nil {
			rows.Close()
			return err
		}
		if !cited[id] {
			rep.Errors = append(rep.Errors, Issue{Code: "MISSING_LOCKED", Item: id, Excerpt: excerpt(text),
				Message: "thiếu quy tắc bắt buộc " + key + ": thêm vào system_prompt kèm [[" + id + "]]"})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// COVERAGE: mục bắt buộc mà nhà xe đã có tri thức phải được system_prompt hoặc faq dùng tới.
	// Mục nhà xe chưa có tri thức → cảnh báo: cần fallbacks.
	var faqTopics []string
	if err := db.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT i.topic), '{}') FROM bot_artifacts f
		JOIN artifact_citations c ON c.artifact_id = f.id JOIN items i ON i.id = c.item_id
		WHERE f.id = (SELECT id FROM bot_artifacts WHERE bot_id = $1 AND kind = 'faq' ORDER BY version DESC LIMIT 1)
			AND i.status = 'active'`, a.BotID).Scan(&faqTopics); err != nil {
		return err
	}
	for _, t := range faqTopics {
		citedTopics[t] = true
	}
	own := map[string]bool{}
	var ownTopics []string
	if err := db.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT topic), '{}') FROM items
		WHERE operator_id = $1 AND layer = 2 AND status = 'active'`, operatorID).Scan(&ownTopics); err != nil {
		return err
	}
	for _, t := range ownTopics {
		own[t] = true
	}
	var hasFallbacks bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bot_artifacts WHERE bot_id = $1 AND kind = 'fallbacks')`,
		a.BotID).Scan(&hasFallbacks); err != nil {
		return err
	}
	for _, t := range topics {
		if !t.Required || citedTopics[t.ID] {
			continue
		}
		if own[t.ID] {
			rep.Errors = append(rep.Errors, Issue{Code: "COVERAGE", Topic: t.ID, Message: "chưa phủ mục bắt buộc '" +
				t.Title + "' dù nhà xe đã có tri thức: recall_knowledge(topics=[" + t.ID + "]) rồi thêm vào system_prompt/faq"})
		} else if !hasFallbacks {
			rep.Warnings = append(rep.Warnings, Issue{Code: "COVERAGE", Topic: t.ID, Message: "nhà xe chưa có tri thức " +
				"cho mục bắt buộc '" + t.Title + "': viết fallbacks (nói chưa có thông tin, chuyển nhân viên) và hỏi nhà xe"})
		}
	}
	return nil
}

func containsAny(s string, subs []string) bool {
	s = " " + s + " "
	for _, sub := range subs {
		if strings.Contains(s, " "+sub+" ") {
			return true
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
