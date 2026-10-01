// Package review: hàng đợi duyệt tri thức của nhà xe (review_items) cho MCP.
//
// Apply/reject đi qua SQL apply_review / reject_review (migration 000006) — cùng logic với ai-worker.
// Đề xuất từ AI (Propose) không bao giờ tự apply: luôn vào hàng đợi chờ người duyệt.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

var ErrNotFound = errors.New("không tìm thấy review trong phạm vi nhà xe này")

// HighRiskTopics: giống biva_worker.ingest.diff.HIGH_RISK_TOPICS.
var HighRiskTopics = []string{"fare", "schedule", "cancellation", "payment"}

type Summary struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Topic      string    `json:"topic"`
	ChangeKind string    `json:"change_kind"`
	Risk       string    `json:"risk"`
	Status     string    `json:"status"`
	Proposed   string    `json:"proposed_text,omitempty"`
	Current    string    `json:"current_text,omitempty"`
	ProposedBy string    `json:"proposed_by"`
	CreatedAt  time.Time `json:"created_at"`
}

type Source struct {
	DocumentID string    `json:"document_id"`
	Channel    string    `json:"channel"`
	ReceivedAt time.Time `json:"received_at"`
	Excerpt    string    `json:"excerpt"`
}

type Detail struct {
	Summary
	Before     map[string]any `json:"before,omitempty"`
	After      map[string]any `json:"after,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	DecidedBy  string         `json:"decided_by,omitempty"`
	Source     *Source        `json:"source,omitempty"`
	OpenOnKey  int            `json:"other_open_reviews_same_key"`
	EffectNote string         `json:"effect"`
}

const summaryCols = `r.id::text, r.key, r.topic, r.change_kind, r.risk, r.status,
	COALESCE(r.after->>'text', ''), COALESCE(r.before->>'text', ''), r.proposed_by, r.created_at`

func scanSummary(row pgx.Row, s *Summary) error {
	return row.Scan(&s.ID, &s.Key, &s.Topic, &s.ChangeKind, &s.Risk, &s.Status, &s.Proposed, &s.Current,
		&s.ProposedBy, &s.CreatedAt)
}

// List: rủi ro cao trước, cũ trước. status rỗng = open.
func List(ctx context.Context, db *pgxpool.Pool, operatorID, status, risk string, limit int) ([]Summary, error) {
	if status == "" {
		status = "open"
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.Query(ctx, `SELECT `+summaryCols+` FROM review_items r
		WHERE r.operator_id = $1 AND r.status = $2 AND ($3 = '' OR r.risk = $3)
		ORDER BY (r.risk = 'high') DESC, r.created_at LIMIT $4`, operatorID, status, risk, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err := scanSummary(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func Get(ctx context.Context, db *pgxpool.Pool, operatorID, id string) (Detail, error) {
	var d Detail
	var before, after []byte
	var reason, decided, docID, channel, content *string
	var received *time.Time
	err := db.QueryRow(ctx, `SELECT `+summaryCols+`, r.before, r.after, r.reason, r.decided_by,
			d.id::text, d.source, d.received_at, d.content,
			(SELECT count(*) FROM review_items o WHERE o.operator_id = r.operator_id AND o.key = r.key
			   AND o.status = 'open' AND o.id <> r.id)
		FROM review_items r LEFT JOIN documents d ON d.id = r.document_id
		WHERE r.operator_id = $1 AND r.id = $2`, operatorID, id,
	).Scan(&d.ID, &d.Key, &d.Topic, &d.ChangeKind, &d.Risk, &d.Status, &d.Proposed, &d.Current, &d.ProposedBy,
		&d.CreatedAt, &before, &after, &reason, &decided, &docID, &channel, &received, &content, &d.OpenOnKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	_ = json.Unmarshal(before, &d.Before)
	_ = json.Unmarshal(after, &d.After)
	d.Reason, d.DecidedBy = deref(reason), deref(decided)
	if docID != nil {
		d.Source = &Source{DocumentID: *docID, Channel: deref(channel), Excerpt: excerpt(deref(content), 2000)}
		if received != nil {
			d.Source.ReceivedAt = *received
		}
	}
	d.EffectNote = effect(d)
	return d, nil
}

func effect(d Detail) string {
	switch d.ChangeKind {
	case "NEW":
		return "thêm item mới cho key " + d.Key
	case "CHANGE", "CONFLICT":
		if vf, _ := d.After["valid_from"].(string); vf != "" {
			return fmt.Sprintf("bản mới có hiệu lực từ %s; nếu ngày đó ở tương lai, bản hiện tại vẫn dùng tới ngày đó", vf)
		}
		return "thay bản hiện tại của " + d.Key + " ngay khi duyệt (bản cũ chuyển superseded, trích dẫn tới bản cũ thành stale)"
	case "REMOVE":
		return "bỏ item " + d.Key + " khỏi tri thức đang dùng (retracted)"
	case "DUPLICATE":
		return "chỉ tăng số lần xác nhận của item hiện có"
	}
	return ""
}

type Outcome struct {
	Status string `json:"status"` // applied | stale | rejected
	ItemID string `json:"item_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Decide áp dụng hoặc từ chối một review đang mở của nhà xe; actor ghi vào audit_log (user:<id>).
func Decide(ctx context.Context, db *pgxpool.Pool, operatorID, id, decision, reason, actor string) (Outcome, error) {
	var owned bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM review_items WHERE id = $1 AND operator_id = $2)`,
		id, operatorID).Scan(&owned); err != nil {
		return Outcome{}, err
	}
	if !owned {
		return Outcome{}, ErrNotFound
	}
	switch decision {
	case "approve":
		var raw []byte
		if err := db.QueryRow(ctx, `SELECT apply_review($1, $2)`, id, actor).Scan(&raw); err != nil {
			return Outcome{}, err
		}
		var out struct {
			Status string  `json:"status"`
			ItemID *string `json:"item_id"`
			Reason string  `json:"reason"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return Outcome{}, err
		}
		if out.Status == "applied" {
			// E3.2: tri thức đổi → gom lại observation của scope này (job tự no-op khi không có gì mới).
			if _, _, err := queue.Enqueue(ctx, db, queue.Job{Kind: "consolidate",
				OperatorID: operatorID, IdempotencyKey: "consolidate:" + operatorID + ":" + id}); err != nil {
				slog.Warn("enqueue consolidate lỗi", "err", err)
			}
		}
		return Outcome{Status: out.Status, ItemID: deref(out.ItemID), Reason: out.Reason}, nil
	case "reject":
		if _, err := db.Exec(ctx, `SELECT reject_review($1, $2, NULLIF($3, ''))`, id, actor, reason); err != nil {
			return Outcome{}, err
		}
		return Outcome{Status: "rejected", Reason: reason}, nil
	}
	return Outcome{}, fmt.Errorf("decision phải là approve hoặc reject")
}

type Proposal struct {
	Action    string            `json:"action"` // upsert | remove
	Kind      string            `json:"kind"`
	Topic     string            `json:"topic"`
	Key       string            `json:"key"`
	Text      string            `json:"text"`
	Facts     map[string]string `json:"facts,omitempty"`
	ValidFrom string            `json:"valid_from,omitempty"` // YYYY-MM-DD (giờ Việt Nam)
	ValidTo   string            `json:"valid_to,omitempty"`
	Reason    string            `json:"reason"`
}

var vn = time.FixedZone("ICT", 7*3600)

func parseDay(s string, endOfDay bool) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	d, err := time.ParseInLocation("2006-01-02", s, vn)
	if err != nil {
		return nil, fmt.Errorf("ngày %q phải dạng YYYY-MM-DD", s)
	}
	if endOfDay {
		d = d.Add(24*time.Hour - time.Microsecond)
	}
	return &d, nil
}

// Propose tạo review_item (không bao giờ tự apply). Trả về review id, change_kind, key đã chuẩn hoá.
func Propose(ctx context.Context, db *pgxpool.Pool, operatorID string, p Proposal, topics []string, actor string) (Summary, error) {
	var s Summary
	if !slices.Contains(topics, p.Topic) && p.Topic != "other" {
		return s, fmt.Errorf("topic %q không có trong template (dùng: %s, other)", p.Topic, strings.Join(topics, ", "))
	}
	if !slices.Contains([]string{"data", "policy", "lesson", "persona"}, p.Kind) {
		return s, errors.New("kind phải là data | policy | lesson | persona")
	}
	if p.Action != "upsert" && p.Action != "remove" {
		return s, errors.New("action phải là upsert hoặc remove")
	}
	if len(strings.TrimSpace(p.Text)) < 5 || strings.TrimSpace(p.Reason) == "" {
		return s, errors.New("cần text (≥ 5 ký tự) và reason (vì sao đề xuất, nguồn ở đâu)")
	}
	from, err := parseDay(p.ValidFrom, false)
	if err != nil {
		return s, err
	}
	to, err := parseDay(p.ValidTo, true)
	if err != nil {
		return s, err
	}
	key := textnorm.NormalizeKey(p.Topic, p.Key)

	tx, err := db.Begin(ctx)
	if err != nil {
		return s, err
	}
	defer tx.Rollback(ctx)

	var targetID, targetText *string
	var targetValue []byte
	err = tx.QueryRow(ctx, `SELECT id::text, text, value FROM items
		WHERE operator_id = $1 AND layer = 2 AND key = $2 AND status = 'active'
		ORDER BY valid_from DESC NULLS LAST LIMIT 1`, operatorID, key).Scan(&targetID, &targetText, &targetValue)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return s, err
	}

	kind := "NEW"
	switch {
	case p.Action == "remove" && targetID == nil:
		return s, fmt.Errorf("key %s chưa có item active để bỏ", key)
	case p.Action == "remove":
		kind = "REMOVE"
	case targetID != nil:
		kind = "CHANGE"
	}
	risk := "low"
	if kind != "NEW" || slices.Contains(HighRiskTopics, p.Topic) {
		risk = "high"
	}

	var value any
	if len(p.Facts) > 0 {
		value = map[string]any{"facts": p.Facts}
	}
	var itemID *string
	if kind != "REMOVE" {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status, valid_from, valid_to,
			                   mentioned_at, metadata, search_text)
			VALUES (2, $1, $2, $3, $4, $5, $6, 'pending', $7, $8, now(), $9, $10)
			RETURNING id::text`,
			operatorID, p.Kind, p.Topic, key, strings.TrimSpace(p.Text), value, from, to,
			map[string]any{"source": "propose_item", "proposed_by": actor},
			textnorm.ItemSearchText(p.Topic, key, strings.TrimSpace(p.Text))).Scan(&id)
		if err != nil {
			return s, err
		}
		itemID = &id
	}
	var before any
	if targetID != nil {
		b := map[string]any{"id": *targetID, "text": deref(targetText)}
		var v map[string]any
		if json.Unmarshal(targetValue, &v) == nil {
			if f, ok := v["facts"]; ok {
				b["facts"] = f
			}
		}
		before = b
	}
	after := map[string]any{"action": p.Action, "kind": p.Kind, "text": strings.TrimSpace(p.Text), "facts": p.Facts,
		"valid_from": nullable(p.ValidFrom), "valid_to": nullable(p.ValidTo)}
	err = tx.QueryRow(ctx, `
		INSERT INTO review_items (operator_id, key, topic, change_kind, risk, item_id, target_item_id, before, after,
		                          reason, proposed_by)
		VALUES ($1, $2, $3, $4, $5, $6::uuid, $7::uuid, $8, $9, $10, $11)
		RETURNING `+strings.ReplaceAll(summaryCols, "r.", ""),
		operatorID, key, p.Topic, kind, risk, itemID, targetID, before, after, p.Reason, actor,
	).Scan(&s.ID, &s.Key, &s.Topic, &s.ChangeKind, &s.Risk, &s.Status, &s.Proposed, &s.Current, &s.ProposedBy,
		&s.CreatedAt)
	if err != nil {
		return s, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log (actor, action, target, payload) VALUES ($1, 'review.propose', $2, $3)`,
		actor, "review:"+s.ID, map[string]any{"operator_id": operatorID, "key": key, "change_kind": kind}); err != nil {
		return s, err
	}
	if itemID != nil {
		// Embedding cho nhánh semantic của recall (search_text đã ghi ở trên); cùng transaction với item.
		if _, err := tx.Exec(ctx, `INSERT INTO operations (kind, operator_id, payload) VALUES ('index.items', $1, $2)`,
			operatorID, map[string]any{"item_ids": []string{*itemID}}); err != nil {
			return s, err
		}
	}
	return s, tx.Commit(ctx)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
