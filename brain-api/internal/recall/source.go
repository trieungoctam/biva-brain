package recall

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("không tìm thấy item trong phạm vi này")

// maxContent: document dài (bảng Excel dán vào) chỉ trả phần đầu để không làm tràn context của AI.
const maxContent = 6000

type Document struct {
	ID          string     `json:"id"`
	Channel     string     `json:"channel"`
	ReceivedAt  time.Time  `json:"received_at"`
	SubmittedBy string     `json:"submitted_by,omitempty"`
	Content     string     `json:"content"`
	Truncated   bool       `json:"truncated,omitempty"`
	Via         string     `json:"via,omitempty"`       // vai trò với item: origin (tạo ra item) | confirm (nhắc lại)
	ReviewID    string     `json:"review_id,omitempty"` // review đã đưa item/nguồn này vào
	ProposedBy  string     `json:"proposed_by,omitempty"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

type SourceResult struct {
	Item struct {
		ID           string     `json:"id"`
		Layer        string     `json:"layer"`
		Status       string     `json:"status"`
		Topic        string     `json:"topic"`
		Key          string     `json:"key,omitempty"`
		Text         string     `json:"text"`
		SupersededBy string     `json:"superseded_by,omitempty"`
		ProofCount   int        `json:"proof_count"`
		CreatedAt    time.Time  `json:"created_at"`
		ValidFrom    *time.Time `json:"valid_from,omitempty"`
		ValidTo      *time.Time `json:"valid_to,omitempty"`
	} `json:"item"`
	KBFile    string     `json:"kb_file,omitempty"` // L0/L1: tri thức nền nằm trong repo kb/
	Reason    string     `json:"reason,omitempty"`  // lý do/nguồn ghi khi đề xuất (propose_item) nếu không có document
	Documents []Document `json:"documents"`
}

// GetSource: nguồn gốc của một item (kể cả item đã superseded — để truy vết vì sao tri thức đổi).
// Item của nhà xe khác → ErrNotFound (không để lộ là có tồn tại).
func GetSource(ctx context.Context, db *pgxpool.Pool, operatorID, itemID string) (SourceResult, error) {
	var res SourceResult
	var layer int16
	var opID, key, superseded, docID *string
	var meta struct {
		Source string `json:"source"`
	}
	it := &res.Item
	err := db.QueryRow(ctx, `SELECT id::text, layer, operator_id, status, topic, key, text, superseded_by::text,
			proof_count, created_at, valid_from, valid_to, document_id::text, metadata
		FROM items WHERE id = $1::uuid`, itemID).Scan(&it.ID, &layer, &opID, &it.Status, &it.Topic, &key, &it.Text,
		&superseded, &it.ProofCount, &it.CreatedAt, &it.ValidFrom, &it.ValidTo, &docID, &meta)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, ErrNotFound
	}
	if err != nil {
		return res, err
	}
	if layer >= 2 && (opID == nil || *opID != operatorID) {
		return res, ErrNotFound
	}
	it.Layer, _ = layerLabel(int(layer), false, "")
	it.Key, it.SupersededBy = deref(key), deref(superseded)
	res.KBFile = meta.Source
	res.Documents = []Document{}
	if layer < 2 {
		return res, nil
	}

	// Review đã tạo ra item (origin) và các lần nhà xe nhắc lại đúng nội dung (DUPLICATE → confirm).
	rows, err := db.Query(ctx, `SELECT r.id::text, r.reason, r.proposed_by, r.decided_by, r.decided_at,
			CASE WHEN r.item_id = $1::uuid THEN 'origin' ELSE 'confirm' END,
			d.id::text, d.source, d.received_at, d.submitted_by, d.content
		FROM review_items r LEFT JOIN documents d ON d.id = r.document_id
		WHERE r.operator_id = $2 AND r.status = 'applied'
			AND (r.item_id = $1::uuid OR (r.target_item_id = $1::uuid AND r.change_kind = 'DUPLICATE'))
		ORDER BY r.created_at LIMIT 20`, itemID, operatorID)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var reviewID, via string
		var reason, proposedBy, decidedBy, dID, channel, submittedBy, content *string
		var decidedAt, received *time.Time
		if err := rows.Scan(&reviewID, &reason, &proposedBy, &decidedBy, &decidedAt, &via,
			&dID, &channel, &received, &submittedBy, &content); err != nil {
			return res, err
		}
		if via == "origin" && res.Reason == "" {
			res.Reason = deref(reason)
		}
		if dID == nil {
			continue
		}
		seen[*dID] = true
		res.Documents = append(res.Documents, newDocument(*dID, deref(channel), *received, deref(submittedBy),
			deref(content), via, reviewID, deref(proposedBy), deref(decidedBy), decidedAt))
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	// Item tạo không qua review (vd seed) nhưng có document_id.
	if docID != nil && !seen[*docID] {
		var d Document
		var submittedBy *string
		if err := db.QueryRow(ctx, `SELECT source, received_at, submitted_by, content FROM documents WHERE id = $1::uuid`,
			*docID).Scan(&d.Channel, &d.ReceivedAt, &submittedBy, &d.Content); err != nil {
			return res, err
		}
		res.Documents = append([]Document{newDocument(*docID, d.Channel, d.ReceivedAt, deref(submittedBy), d.Content,
			"origin", "", "", "", nil)}, res.Documents...)
	}
	return res, nil
}

func newDocument(id, channel string, received time.Time, submittedBy, content, via, reviewID, proposedBy,
	decidedBy string, decidedAt *time.Time) Document {
	d := Document{ID: id, Channel: channel, ReceivedAt: received, SubmittedBy: submittedBy, Via: via,
		ReviewID: reviewID, ProposedBy: proposedBy, DecidedBy: decidedBy, DecidedAt: decidedAt}
	if utf8.RuneCountInString(content) > maxContent {
		content = string([]rune(content)[:maxContent])
		d.Truncated = true
	}
	d.Content = content
	return d
}
