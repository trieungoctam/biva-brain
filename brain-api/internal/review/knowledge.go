package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

// Item đang dùng của nhà xe (để AI dùng lại key khi gửi cập nhật).
type Item struct {
	ID        string            `json:"id"`
	Key       string            `json:"key"`
	Topic     string            `json:"topic"`
	Kind      string            `json:"kind"`
	Text      string            `json:"text"`
	Facts     map[string]string `json:"facts,omitempty"`
	ValidFrom *time.Time        `json:"valid_from,omitempty"`
	ValidTo   *time.Time        `json:"valid_to,omitempty"`
}

// ListActive: item active (L2) của nhà xe, lọc theo topic nếu có.
func ListActive(ctx context.Context, db *pgxpool.Pool, operatorID, topic string, limit int) ([]Item, error) {
	if limit <= 0 || limit > 500 {
		limit = 300
	}
	rows, err := db.Query(ctx, `SELECT id::text, key, topic, kind, text, value, valid_from, valid_to FROM items
		WHERE operator_id = $1 AND layer = 2 AND status = 'active' AND ($2 = '' OR topic = $2)
		ORDER BY topic, key, valid_from NULLS FIRST LIMIT $3`, operatorID, topic, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Item{}
	for rows.Next() {
		var it Item
		var key *string
		var value []byte
		if err := rows.Scan(&it.ID, &key, &it.Topic, &it.Kind, &it.Text, &value, &it.ValidFrom, &it.ValidTo); err != nil {
			return nil, err
		}
		it.Key = deref(key)
		var v struct {
			Facts map[string]string `json:"facts"`
		}
		if json.Unmarshal(value, &v) == nil {
			it.Facts = v.Facts
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SubmittedItem: một item AI phía builder đã trích (khớp $defs/item của contracts/schemas/jobs/ingest).
type SubmittedItem struct {
	Action    string            `json:"action,omitempty" jsonschema:"upsert (mặc định) | remove"`
	Kind      string            `json:"kind" jsonschema:"data | policy | lesson | persona"`
	Topic     string            `json:"topic" jsonschema:"topic thuộc template ngành (xem resource/tool get_bot_spec), hoặc other"`
	Key       string            `json:"key" jsonschema:"chủ thể của item, dạng <topic>.<phần>; dùng lại key đã có (list_knowledge) khi nói cùng chủ thể"`
	Text      string            `json:"text" jsonschema:"một ý, câu tiếng Việt đầy đủ, tự hiểu được"`
	Facts     map[string]string `json:"facts,omitempty" jsonschema:"giá trị chính, vd {gia_ve: 320000, gio_xuat_ben: 23:00}"`
	ValidFrom string            `json:"valid_from,omitempty" jsonschema:"YYYY-MM-DD, chỉ khi nguồn nêu rõ ngày bắt đầu"`
	ValidTo   string            `json:"valid_to,omitempty" jsonschema:"YYYY-MM-DD, ngày cuối còn hiệu lực"`
}

var Sources = []string{"zalo", "excel", "image", "call", "chat", "form", "console", "other"}

// CheckSubmitted kiểm tra đồng bộ trước khi enqueue (để AI sửa ngay, không phải chờ job lỗi).
// Trả về payload items đã làm sạch (dạng contracts) hoặc danh sách lỗi theo vị trí.
func CheckSubmitted(items []SubmittedItem, topics []string) ([]map[string]any, error) {
	if len(items) == 0 || len(items) > 200 {
		return nil, errors.New("items phải có 1–200 phần tử")
	}
	var problems []string
	out := make([]map[string]any, 0, len(items))
	for i, it := range items {
		at := fmt.Sprintf("items[%d]", i)
		action := it.Action
		if action == "" {
			action = "upsert"
		}
		if action != "upsert" && action != "remove" {
			problems = append(problems, at+": action phải là upsert | remove")
		}
		if !slices.Contains([]string{"data", "policy", "lesson", "persona"}, it.Kind) {
			problems = append(problems, at+": kind phải là data | policy | lesson | persona")
		}
		if !slices.Contains(topics, it.Topic) && it.Topic != "other" {
			problems = append(problems, fmt.Sprintf("%s: topic %q không có trong template", at, it.Topic))
		}
		if textnorm.NormalizeKey(it.Topic, it.Key) == it.Topic {
			problems = append(problems, at+": key phải nêu chủ thể (vd fare.sai_gon_da_lat.giuong_nam)")
		}
		if n := len([]rune(strings.TrimSpace(it.Text))); n < 5 || n > 2000 {
			problems = append(problems, at+": text phải 5–2000 ký tự")
		}
		for name, v := range map[string]string{"valid_from": it.ValidFrom, "valid_to": it.ValidTo} {
			if v != "" {
				if _, err := time.Parse("2006-01-02", v); err != nil {
					problems = append(problems, fmt.Sprintf("%s: %s phải dạng YYYY-MM-DD", at, name))
				}
			}
		}
		m := map[string]any{"action": action, "kind": it.Kind, "topic": it.Topic, "key": it.Key,
			"text": strings.TrimSpace(it.Text)}
		if len(it.Facts) > 0 {
			m["facts"] = it.Facts
		}
		if it.ValidFrom != "" {
			m["valid_from"] = it.ValidFrom
		}
		if it.ValidTo != "" {
			m["valid_to"] = it.ValidTo
		}
		out = append(out, m)
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return out, nil
}
