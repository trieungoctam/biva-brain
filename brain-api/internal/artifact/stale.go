package artifact

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StaleReason: một chỗ trong artifact bị tri thức mới làm lỗi thời (ghi bởi trigger migration 000011).
type StaleReason struct {
	Line         int    `json:"line,omitempty"`
	Item         string `json:"item"`
	Reason       string `json:"reason"` // superseded | expired | retracted | overridden_default | new_locked_rule
	SupersededBy string `json:"superseded_by,omitempty"`
	LineText     string `json:"line_text,omitempty"` // nội dung dòng hiện tại trong artifact
	ItemText     string `json:"item_text,omitempty"` // tri thức cũ được trích
	NewText      string `json:"new_text,omitempty"`  // tri thức thay thế (nếu có)
	Instruction  string `json:"instruction"`
}

type StaleArtifact struct {
	BotID   string        `json:"bot_id"`
	Channel string        `json:"channel"`
	Kind    string        `json:"kind"`
	Version int           `json:"version"`
	StaleAt *time.Time    `json:"stale_at,omitempty"`
	Reasons []StaleReason `json:"reasons"`
}

var instructions = map[string]string{
	"superseded":         "sửa đúng dòng này theo new_text và trích dẫn [[superseded_by]]",
	"expired":            "tri thức đã hết hiệu lực: bỏ hoặc thay bằng tri thức hiện hành (recall_knowledge)",
	"retracted":          "nhà xe đã bỏ thông tin này: xoá câu, hoặc chuyển sang fallbacks nếu khách vẫn có thể hỏi",
	"overridden_default": "nhà xe đã có chính sách riêng (new_text): thay thông lệ chung bằng chính sách này, trích [[superseded_by]]",
	"new_locked_rule":    "quy tắc bắt buộc mới (item_text): thêm vào system_prompt kèm [[item]]",
}

// ListStale: bản mới nhất đang stale của các artifact của nhà xe, kèm từng chỗ cần sửa.
func ListStale(ctx context.Context, db *pgxpool.Pool, operatorID, channel string) ([]StaleArtifact, error) {
	rows, err := db.Query(ctx, `SELECT a.bot_id, b.channel, a.kind, a.version, a.stale_at, a.stale_reasons, a.content
		FROM bot_artifacts a JOIN bots b ON b.id = a.bot_id
		WHERE a.operator_id = $1 AND a.status = 'stale' AND ($2 = '' OR b.channel = $2) AND artifact_is_latest(a.id)
		ORDER BY a.bot_id, a.kind`, operatorID, channel)
	if err != nil {
		return nil, err
	}
	type raw struct {
		StaleArtifact
		reasons []byte
		content string
	}
	var list []raw
	itemIDs := map[string]bool{}
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.BotID, &r.Channel, &r.Kind, &r.Version, &r.StaleAt, &r.reasons, &r.content); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(r.reasons, &r.Reasons); err != nil {
			rows.Close()
			return nil, err
		}
		for _, sr := range r.Reasons {
			itemIDs[sr.Item] = true
			if sr.SupersededBy != "" {
				itemIDs[sr.SupersededBy] = true
			}
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	texts := map[string]string{}
	if len(itemIDs) > 0 {
		ids := make([]string, 0, len(itemIDs))
		for id := range itemIDs {
			ids = append(ids, id)
		}
		trows, err := db.Query(ctx, `SELECT id::text, text FROM items WHERE id = ANY($1::uuid[])`, ids)
		if err != nil {
			return nil, err
		}
		for trows.Next() {
			var id, text string
			if err := trows.Scan(&id, &text); err != nil {
				trows.Close()
				return nil, err
			}
			texts[id] = text
		}
		trows.Close()
		if err := trows.Err(); err != nil {
			return nil, err
		}
	}
	out := []StaleArtifact{}
	for _, r := range list {
		lines := strings.Split(r.content, "\n")
		for i := range r.Reasons {
			sr := &r.Reasons[i]
			if sr.Line > 0 && sr.Line <= len(lines) {
				sr.LineText = strings.TrimSpace(lines[sr.Line-1])
			}
			sr.ItemText, sr.NewText, sr.Instruction = texts[sr.Item], texts[sr.SupersededBy], instructions[sr.Reason]
		}
		out = append(out, r.StaleArtifact)
	}
	return out, nil
}
