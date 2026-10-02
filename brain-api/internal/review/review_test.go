package review

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

// Sửa giá ĐANG ÁP DỤNG khi đã có bản lên lịch cho tương lai: target phải là bản hiện tại
// (khoảng chứa thời điểm đề xuất), không phải bản valid_from lớn nhất.
func TestProposeTargetsIntervalContainingNow(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	op := "rv" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'R')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })

	// Bản hiện tại: hiệu lực tới 01/11.
	var cur, fut string
	if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status,
		valid_from, valid_to) VALUES (2, $1, 'data', 'fare', 'fare.sg_dl', 'Giá 320k', '{}', 'active',
		now() - interval '10 days', now() + interval '30 days') RETURNING id::text`,
		op).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	// Bản tương lai: từ 01/11 (chưa tới — nhưng active vì khoảng của nó bắt đầu sau).
	if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status,
		valid_from, valid_to) VALUES (2, $1, 'data', 'fare', 'fare.sg_dl', 'Giá Tết 450k', '{}', 'active',
		now() + interval '30 days', now() + interval '60 days') RETURNING id::text`,
		op).Scan(&fut); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, []string{cur, fut})
	})

	s, err := Propose(ctx, pool, op, Proposal{
		Kind: "data", Topic: "fare", Key: "fare.sg_dl", Action: "upsert",
		Text: "Giá 350k từ hôm nay", Reason: "tăng giá xăng",
	}, []string{"fare"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM review_items WHERE id = $1::uuid`, s.ID) })
	if s.ChangeKind != "CHANGE" {
		t.Fatalf("change_kind = %s, muốn CHANGE", s.ChangeKind)
	}
	// Target phải là bản đang áp dụng (cur), không phải bản tương lai (fut).
	var target string
	if err := pool.QueryRow(ctx,
		`SELECT target_item_id::text FROM review_items WHERE id = $1::uuid`, s.ID).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != cur {
		t.Fatalf("target phải là bản đang áp dụng %s, được %s (bản tương lai %s)", cur, target, fut)
	}
}
