package recall

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGetSource(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	doc := func(content string) string {
		var id string
		if err := e.pool.QueryRow(ctx, `INSERT INTO documents (layer, operator_id, source, content, content_hash, submitted_by)
			VALUES (2, $1, 'zalo', $2, md5($2), 'user:bu') RETURNING id::text`, e.op, content).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	review := func(kind, itemCol, itemID, docID string) {
		_, err := e.pool.Exec(ctx, `INSERT INTO review_items (operator_id, key, topic, change_kind, risk, status, `+itemCol+`,
				document_id, reason, proposed_by, decided_by, decided_at)
			VALUES ($1, 'pets.cho_meo', 'pets', $2, 'low', 'applied', $3::uuid, $4::uuid, 'nhà xe nhắn Zalo',
				'system:ingest', 'user:bu', now())`, e.op, kind, itemID, docID)
		if err != nil {
			t.Fatal(err)
		}
	}
	d1 := doc("Từ 30/9 nhà xe không nhận chó mèo nữa nha " + e.tag)
	d2 := doc("Nhắc lại: không chở chó mèo " + e.tag + strings.Repeat(" x", maxContent))
	item := e.insertIt(2, e.op, "policy", "pets", "pets.cho_meo", "Không nhận chó mèo", nil)
	review("NEW", "item_id", item, d1)
	review("DUPLICATE", "target_item_id", item, d2)

	res, err := GetSource(ctx, e.pool, e.op, item)
	if err != nil {
		t.Fatal(err)
	}
	if res.Item.Layer != "L2" || res.Item.Status != "active" || res.Reason != "nhà xe nhắn Zalo" || len(res.Documents) != 2 {
		t.Fatalf("source = %+v", res)
	}
	o, c := res.Documents[0], res.Documents[1]
	if o.Via != "origin" || o.ID != d1 || o.Channel != "zalo" || o.SubmittedBy != "user:bu" || o.DecidedBy != "user:bu" ||
		c.Via != "confirm" || !c.Truncated || len([]rune(c.Content)) != maxContent {
		t.Fatalf("documents = %+v", res.Documents)
	}

	// Item của nhà xe khác: không lộ.
	if _, err := GetSource(ctx, e.pool, e.opB, item); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nhà xe khác: %v", err)
	}
	// L1: trỏ về file kb/.
	l1 := e.insertIt(1, nil, "policy", "pets", "", "Thường không nhận thú cưng lớn",
		map[string]any{"metadata": map[string]any{"source": "L1/xe-khach/rules.yaml"}})
	res, err = GetSource(ctx, e.pool, e.op, l1)
	if err != nil || res.KBFile != "L1/xe-khach/rules.yaml" || len(res.Documents) != 0 || res.Item.Layer != "L1" {
		t.Fatalf("L1 source = %+v %v", res, err)
	}
}

func TestGetOverview(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.insertIt(2, e.op, "data", "fare", "fare.sg_dl", "Giá 320.000đ", nil)
	e.insertIt(2, e.op, "policy", "pets", "pets.cho_meo", "Không nhận chó mèo", nil)
	e.insertIt(2, e.op, "policy", "karaoke", "karaoke.co", "Có karaoke", nil)
	e.pool.Exec(ctx, `INSERT INTO review_items (operator_id, key, topic, change_kind, risk, target_item_id, proposed_by)
		SELECT $1, key, topic, 'REMOVE', 'high', id, 'system:ingest' FROM items WHERE operator_id = $1 AND topic = 'pets'`, e.op)
	e.pool.Exec(ctx, `INSERT INTO operations (kind, operator_id, status, run_after) VALUES ('ingest', $1, 'queued', now() + interval '1 hour')`, e.op)

	ov, err := GetOverview(ctx, e.pool, e.op, []TopicSpec{
		{ID: "fare", Title: "Giá vé", Required: true}, {ID: "schedule", Title: "Lịch chạy", Required: true},
		{ID: "pets", Title: "Thú cưng"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := ov.Coverage
	if ov.Operator.Name != "Phương Nam" || ov.Items["active"] != 3 || ov.LastUpdate == nil ||
		len(c.Required) != 2 || c.Required[0].Active != 1 || len(c.MissingRequired) != 1 || c.MissingRequired[0] != "schedule" ||
		c.Recommended[0].Active != 1 || c.Other != 1 || ov.Reviews["high"] != 1 || ov.Reviews["low"] != 0 ||
		len(ov.Jobs) != 1 || ov.Jobs[0].Kind != "ingest" || ov.Jobs[0].Status != "queued" || len(ov.NextActions) != 3 {
		t.Fatalf("overview = %+v", ov)
	}
	if _, err := GetOverview(ctx, e.pool, "khong-co", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nhà xe không tồn tại: %v", err)
	}
}
