// Package coverage: độ phủ tri thức của nhà xe theo template ngành (M2, S2.1.1) và bộ câu hỏi gửi nhà xe
// (S2.1.2). Không dùng LLM: câu hỏi lấy từ template; mục ít nhà xe khai riêng (đa số theo thông lệ) → câu
// "xác nhận nhanh" dựa trên thông lệ L1, mục nhiều nhà xe khác nhau → câu hỏi mở.
package coverage

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
)

type Section struct {
	Topic       string   `json:"topic"`
	Title       string   `json:"title"`
	Level       string   `json:"level"`  // required | recommended
	Status      string   `json:"status"` // covered | ambiguous | industry_default | missing
	Items       int      `json:"items"`  // item active của nhà xe
	OpenReviews int      `json:"open_reviews"`
	Defaults    []string `json:"industry_defaults,omitempty"` // id thông lệ L1 đang dùng thay
	Facts       []string `json:"facts"`                       // thông tin template mong có (checklist)
}

type Coverage struct {
	Operator      string    `json:"operator"`
	RequiredTotal int       `json:"required_total"`
	RequiredDone  int       `json:"required_covered"`
	Percent       int       `json:"required_percent"`
	Sections      []Section `json:"sections"`
	NextActions   []string  `json:"next_actions"`
}

type sectionData struct {
	own, reviews, conflicts int
	defaults                []string
	defaultText             []string
}

func Get(ctx context.Context, db *pgxpool.Pool, operatorID string, t kb.Template) (Coverage, error) {
	cov := Coverage{Operator: operatorID, Sections: []Section{}, NextActions: []string{}}
	data := map[string]*sectionData{}
	get := func(topic string) *sectionData {
		if data[topic] == nil {
			data[topic] = &sectionData{}
		}
		return data[topic]
	}
	rows, err := db.Query(ctx, `SELECT topic, layer, id::text, text FROM items
		WHERE status = 'active' AND (layer = 2 AND operator_id = $1 OR layer = 1 AND NOT locked)
		ORDER BY topic, layer, key`, operatorID)
	if err != nil {
		return cov, err
	}
	for rows.Next() {
		var topic, id, text string
		var layer int
		if err := rows.Scan(&topic, &layer, &id, &text); err != nil {
			rows.Close()
			return cov, err
		}
		d := get(topic)
		if layer == 2 {
			d.own++
		} else {
			d.defaults = append(d.defaults, id)
			d.defaultText = append(d.defaultText, text)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cov, err
	}
	rows, err = db.Query(ctx, `SELECT topic, count(*), count(*) FILTER (WHERE change_kind = 'CONFLICT') FROM review_items
		WHERE operator_id = $1 AND status = 'open' GROUP BY topic`, operatorID)
	if err != nil {
		return cov, err
	}
	for rows.Next() {
		var topic string
		var n, c int
		if err := rows.Scan(&topic, &n, &c); err != nil {
			rows.Close()
			return cov, err
		}
		d := get(topic)
		d.reviews, d.conflicts = n, c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cov, err
	}

	for _, sec := range t.Sections {
		d := get(sec.Topic)
		s := Section{Topic: sec.Topic, Title: sec.Title, Level: sec.Level, Items: d.own, OpenReviews: d.reviews,
			Facts: sec.Facts}
		if s.Facts == nil {
			s.Facts = []string{}
		}
		switch {
		case d.conflicts > 0:
			s.Status = "ambiguous"
		case d.own > 0:
			s.Status = "covered"
		case len(d.defaults) > 0:
			s.Status, s.Defaults = "industry_default", d.defaults
		default:
			s.Status = "missing"
		}
		if sec.Level == "required" {
			cov.RequiredTotal++
			if s.Status == "covered" {
				cov.RequiredDone++
			}
		}
		cov.Sections = append(cov.Sections, s)
	}
	if cov.RequiredTotal > 0 {
		cov.Percent = cov.RequiredDone * 100 / cov.RequiredTotal
	}
	if cov.RequiredDone < cov.RequiredTotal {
		cov.NextActions = append(cov.NextActions, "generate_questions → gửi nhà xe (Zalo hoặc create_form)")
	}
	for _, s := range cov.Sections {
		if s.Status == "ambiguous" {
			cov.NextActions = append(cov.NextActions, "list_review_queue: còn CONFLICT cần builder chọn")
			break
		}
	}
	return cov, nil
}

type Question struct {
	Topic    string `json:"topic"`
	Title    string `json:"title"`
	Level    string `json:"level"`
	Style    string `json:"style"` // confirm (xác nhận nhanh thông lệ) | open | resolve (đang mâu thuẫn)
	Question string `json:"question"`
	Default  string `json:"default_item,omitempty"` // thông lệ L1 được hỏi xác nhận
	Reason   string `json:"reason"`
}

type Questions struct {
	Operator  string     `json:"operator"`
	Questions []Question `json:"questions"`
	Message   string     `json:"message"` // gộp sẵn để builder gửi nhà xe (Zalo)
}

// divergeThreshold: tỉ lệ nhà xe có tri thức riêng ở một topic; dưới mức này coi như đa số theo thông lệ.
const divergeThreshold = 0.3

// Generate: câu hỏi cho các mục chưa phủ (bắt buộc trước), tối đa max câu.
func Generate(ctx context.Context, db *pgxpool.Pool, operatorID string, t kb.Template, topics []string, max int) (Questions, error) {
	q := Questions{Operator: operatorID, Questions: []Question{}}
	if max <= 0 || max > 30 {
		max = 10
	}
	cov, err := Get(ctx, db, operatorID, t)
	if err != nil {
		return q, err
	}
	// Mức khác nhau giữa các nhà xe theo topic: bao nhiêu nhà xe (trừ nhà xe này) đã có tri thức riêng.
	var total int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM operators WHERE id <> $1 AND status <> 'offboarded'`,
		operatorID).Scan(&total); err != nil {
		return q, err
	}
	share := map[string]float64{}
	if total > 0 {
		rows, err := db.Query(ctx, `SELECT topic, count(DISTINCT operator_id) FROM items
			WHERE layer = 2 AND status = 'active' AND operator_id <> $1 AND kind <> 'data' GROUP BY topic`, operatorID)
		if err != nil {
			return q, err
		}
		for rows.Next() {
			var topic string
			var n int
			if err := rows.Scan(&topic, &n); err != nil {
				rows.Close()
				return q, err
			}
			share[topic] = float64(n) / float64(total)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return q, err
		}
	}
	questionsOf := map[string][]string{}
	for _, sec := range t.Sections {
		questionsOf[sec.Topic] = sec.Questions
	}
	defaultText := map[string]string{}
	if rows, err := db.Query(ctx, `SELECT id::text, text FROM items WHERE layer = 1 AND status = 'active' AND NOT locked`); err == nil {
		for rows.Next() {
			var id, text string
			if rows.Scan(&id, &text) == nil {
				defaultText[id] = text
			}
		}
		rows.Close()
	}

	want := map[string]bool{}
	for _, tp := range topics {
		want[tp] = true
	}
	secs := append([]Section{}, cov.Sections...)
	sort.SliceStable(secs, func(i, j int) bool { return secs[i].Level == "required" && secs[j].Level != "required" })
	for _, s := range secs {
		if (len(want) > 0 && !want[s.Topic]) || s.Status == "covered" {
			continue
		}
		switch s.Status {
		case "ambiguous":
			q.Questions = append(q.Questions, Question{Topic: s.Topic, Title: s.Title, Level: s.Level, Style: "resolve",
				Question: "Về " + strings.ToLower(s.Title) + ", nhà xe đang có hai thông tin khác nhau — nhà xe xác nhận " +
					"giúp cách áp dụng hiện tại?", Reason: "có đề xuất CONFLICT đang mở"})
		case "industry_default":
			if share[s.Topic] < divergeThreshold && len(s.Defaults) > 0 {
				for _, id := range s.Defaults {
					q.Questions = append(q.Questions, Question{Topic: s.Topic, Title: s.Title, Level: s.Level,
						Style: "confirm", Default: id, Question: "Nhà xe có áp dụng như sau không: \"" +
							strings.TrimSuffix(defaultText[id], ".") + "\"? Nếu khác, nhà xe áp dụng thế nào?",
						Reason: fmt.Sprintf("đang dùng thông lệ; %.0f%% nhà xe khác có quy định riêng", share[s.Topic]*100)})
				}
				continue
			}
			fallthrough
		default:
			for _, text := range questionsOf[s.Topic] {
				q.Questions = append(q.Questions, Question{Topic: s.Topic, Title: s.Title, Level: s.Level,
					Style: "open", Question: text, Reason: map[string]string{
						"missing":          "chưa có tri thức",
						"industry_default": fmt.Sprintf("đang dùng thông lệ; %.0f%% nhà xe khác quy định riêng", share[s.Topic]*100),
					}[s.Status]})
			}
		}
	}
	if len(q.Questions) > max {
		q.Questions = q.Questions[:max]
	}
	var b strings.Builder
	if len(q.Questions) > 0 {
		b.WriteString("Chào anh/chị, để bot trả lời khách đúng thông tin của nhà xe, nhờ anh/chị trả lời giúp mấy câu sau:\n")
		for i, x := range q.Questions {
			fmt.Fprintf(&b, "%d. %s\n", i+1, x.Question)
		}
		b.WriteString("Cảm ơn anh/chị!")
	}
	q.Message = b.String()
	return q, nil
}
