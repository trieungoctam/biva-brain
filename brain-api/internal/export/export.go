// Package export lắp snapshot của bot và xuất ra định dạng trung lập (M2, S2.4.1). Thiết kế: docs/mcp.md §4 "Export".
//
// Snapshot chỉ nhận bản mới nhất của mỗi artifact khi bản đó `valid` (không draft/invalid/stale); thiếu artifact
// bắt buộc → từ chối, nói rõ cần làm gì. Trích dẫn [[id]] bị bỏ khỏi nội dung xuất (bot không in UUID cho khách);
// json giữ danh sách trích dẫn để truy vết.
package export

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/artifact"
)

var Formats = []string{"json", "markdown", "faq_csv"}

// NotReadyError: snapshot chưa lắp được — danh sách artifact cần xử lý.
type NotReadyError struct{ Problems []string }

func (e *NotReadyError) Error() string {
	return "chưa xuất được bot: " + strings.Join(e.Problems, "; ")
}

type ArtifactOut struct {
	Version   int                 `json:"version"`
	Content   string              `json:"content"`
	Citations []artifact.Citation `json:"citations"`
}

type Definition struct {
	BotID            string                 `json:"bot_id"`
	Operator         string                 `json:"operator"`
	Channel          string                 `json:"channel"`
	SnapshotVersion  int                    `json:"snapshot_version"`
	KnowledgeVersion string                 `json:"knowledge_version,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	Artifacts        map[string]ArtifactOut `json:"artifacts"`
}

type Result struct {
	SnapshotID      string         `json:"snapshot_id"`
	SnapshotVersion int            `json:"snapshot_version"`
	Reused          bool           `json:"reused"` // cùng bộ version artifact với snapshot trước
	Artifacts       map[string]int `json:"artifact_versions"`
	Format          string         `json:"format"`
	Filename        string         `json:"filename"`
	Bytes           int            `json:"bytes"`
	Content         string         `json:"content"`
}

var citeRe = regexp.MustCompile(`[ \t]*\[\[[^\[\]]*\]\]`)

// StripCitations bỏ [[...]] (và khoảng trắng trước nó).
func StripCitations(s string) string {
	lines := strings.Split(citeRe.ReplaceAllString(s, ""), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}

type Input struct {
	OperatorID       string
	Channel          string
	Format           string
	Required         []string // artifact bắt buộc (template ngành)
	Actor            string
	KnowledgeVersion string
}

func Export(ctx context.Context, db *pgxpool.Pool, in Input) (Result, error) {
	var res Result
	if in.Channel == "" {
		in.Channel = "zalo"
	}
	if in.Format == "" {
		in.Format = "json"
	}
	if !slices.Contains(Formats, in.Format) {
		return res, &NotReadyError{[]string{"format phải là: " + strings.Join(Formats, ", ")}}
	}
	botID := artifact.BotID(in.OperatorID, in.Channel)

	tx, err := db.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bots WHERE id = $1 AND operator_id = $2)`, botID,
		in.OperatorID).Scan(&exists); err != nil {
		return res, err
	}
	if !exists {
		return res, &NotReadyError{[]string{"bot kênh " + in.Channel + " chưa có artifact nào (build_bot)"}}
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM bots WHERE id = $1 FOR UPDATE`, botID); err != nil {
		return res, err
	}

	type latest struct {
		id, status, content string
		version             int
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (kind) kind, id::text, version, status, content FROM bot_artifacts
		WHERE bot_id = $1 ORDER BY kind, version DESC`, botID)
	if err != nil {
		return res, err
	}
	arts := map[string]latest{}
	for rows.Next() {
		var kind string
		var l latest
		if err := rows.Scan(&kind, &l.id, &l.version, &l.status, &l.content); err != nil {
			rows.Close()
			return res, err
		}
		arts[kind] = l
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	var problems []string
	for _, k := range in.Required {
		if _, ok := arts[k]; !ok {
			problems = append(problems, k+": chưa có (bắt buộc) — viết rồi save_artifact")
		}
	}
	for _, k := range artifact.Kinds {
		l, ok := arts[k]
		if !ok || l.status == "valid" || l.status == "published" {
			continue
		}
		hint := map[string]string{
			"draft":   "chưa kiểm — validate_artifact",
			"invalid": "còn lỗi — sửa theo validate_artifact",
			"stale":   "tri thức đã đổi — list_stale rồi refresh_bot",
		}[l.status]
		problems = append(problems, fmt.Sprintf("%s v%d: %s", k, l.version, hint))
	}
	if len(problems) > 0 {
		return res, &NotReadyError{problems}
	}

	versions := map[string]int{}
	for k, l := range arts {
		versions[k] = l.version
	}
	vjson, _ := json.Marshal(versions)

	var snapID string
	var snapVersion int
	var defRaw []byte
	err = tx.QueryRow(ctx, `SELECT id::text, version, definition FROM snapshots
		WHERE bot_id = $1 AND artifact_versions = $2::jsonb ORDER BY version DESC LIMIT 1`, botID, string(vjson)).
		Scan(&snapID, &snapVersion, &defRaw)
	var def Definition
	switch {
	case err == nil:
		res.Reused = true
		if err := json.Unmarshal(defRaw, &def); err != nil {
			return res, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM snapshots WHERE bot_id = $1`, botID).
			Scan(&snapVersion); err != nil {
			return res, err
		}
		def = Definition{BotID: botID, Operator: in.OperatorID, Channel: in.Channel, SnapshotVersion: snapVersion,
			KnowledgeVersion: in.KnowledgeVersion, CreatedAt: time.Now().UTC(), Artifacts: map[string]ArtifactOut{}}
		for k, l := range arts {
			cites, _ := artifact.ParseCitations(l.content)
			if cites == nil {
				cites = []artifact.Citation{}
			}
			def.Artifacts[k] = ArtifactOut{Version: l.version, Content: StripCitations(l.content), Citations: cites}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO snapshots (bot_id, operator_id, version, artifact_versions,
				knowledge_version, definition, created_by)
			VALUES ($1, $2, $3, $4::jsonb, NULLIF($5, ''), $6, $7) RETURNING id::text`,
			botID, in.OperatorID, snapVersion, string(vjson), in.KnowledgeVersion, def, in.Actor).Scan(&snapID); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_log (actor, action, target, payload) VALUES ($1, 'snapshot.assemble', $2, $3)`,
			in.Actor, "snapshot:"+snapID, map[string]any{"operator_id": in.OperatorID, "bot_id": botID,
				"version": snapVersion, "artifacts": versions}); err != nil {
			return res, err
		}
	default:
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}

	res.SnapshotID, res.SnapshotVersion, res.Artifacts, res.Format = snapID, snapVersion, versions, in.Format
	base := fmt.Sprintf("%s-%s-v%d", in.OperatorID, in.Channel, snapVersion)
	switch in.Format {
	case "json":
		b, _ := json.MarshalIndent(def, "", "  ")
		res.Content, res.Filename = string(b), base+".json"
	case "markdown":
		res.Content, res.Filename = Markdown(def), base+".md"
	case "faq_csv":
		res.Content, res.Filename = FAQCSV(def.Artifacts["faq"].Content), base+"-faq.csv"
	}
	res.Bytes = len(res.Content)
	return res, nil
}

// Markdown: system prompt ghép sẵn để dán vào runtime — system_prompt, persona, flows, fallbacks, tool_spec.
func Markdown(def Definition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- BIVA Brain · %s · snapshot v%d · tri thức %s -->\n", def.BotID, def.SnapshotVersion,
		def.KnowledgeVersion)
	titles := []struct{ kind, title string }{{"system_prompt", ""}, {"persona", "Phong cách"}, {"flows", "Kịch bản"},
		{"fallbacks", "Khi thiếu thông tin"}, {"tool_spec", "Công cụ"}}
	for _, t := range titles {
		a, ok := def.Artifacts[t.kind]
		if !ok {
			continue
		}
		if t.title != "" {
			fmt.Fprintf(&b, "\n# %s\n\n", t.title)
		}
		b.WriteString(strings.TrimSpace(a.Content) + "\n")
	}
	return b.String()
}

var (
	qPrefixRe = regexp.MustCompile(`(?i)^(hỏi|q)\s*[:.]\s*`)
	aPrefixRe = regexp.MustCompile(`(?i)^(đáp|trả lời|a)\s*[:.]\s*`)
)

// FAQCSV: cặp hỏi–đáp từ faq markdown. Câu hỏi = tiêu đề (#…), dòng "Hỏi:"/"Q:", hoặc dòng kết thúc bằng "?";
// câu trả lời = các dòng sau đó tới câu hỏi tiếp theo.
func FAQCSV(faq string) string {
	type qa struct{ q, a string }
	var pairs []qa
	var cur *qa
	for _, line := range strings.Split(faq, "\n") {
		t := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(line, "**", ""), "__", ""))
		if t == "" || t == "---" {
			continue
		}
		heading := strings.HasPrefix(t, "#")
		q := strings.TrimSpace(strings.TrimLeft(t, "#"))
		q = strings.Trim(q, "* ")
		isQ := (heading && strings.HasSuffix(q, "?")) || qPrefixRe.MatchString(q) ||
			(!heading && strings.HasSuffix(q, "?") && (cur == nil || cur.a != ""))
		switch {
		case isQ:
			pairs = append(pairs, qa{q: qPrefixRe.ReplaceAllString(q, "")})
			cur = &pairs[len(pairs)-1]
		case heading:
			cur = nil // tiêu đề nhóm
		case cur != nil:
			a := aPrefixRe.ReplaceAllString(strings.TrimLeft(t, "-*> "), "")
			if cur.a != "" {
				cur.a += " "
			}
			cur.a += a
		}
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"question", "answer"})
	for _, p := range pairs {
		if p.a != "" {
			w.Write([]string{p.q, p.a})
		}
	}
	w.Flush()
	return buf.String()
}
