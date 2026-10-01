package coverage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestCoverageAndQuestions(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op, other := "cv"+sfx, "cvo"+sfx
	pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'C'), ($2, 'O')`, op, other)
	var global []string
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, global)
		pool.Exec(ctx, `DELETE FROM operators WHERE id IN ($1, $2)`, op, other)
	})
	// Topic riêng của test để không lẫn với L1 thật trong DB test.
	tFare, tSched, tKid, tPets := "fare"+sfx, "sched"+sfx, "kid"+sfx, "pets"+sfx
	ins := func(layer int, operator any, topic, text string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
			VALUES ($1, $2, 'policy', $3, $4, 'active') RETURNING id::text`, layer, operator, topic, text).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if layer == 1 {
			global = append(global, id)
		}
		return id
	}
	ins(2, op, tFare, "Giá theo bảng")
	kidDefault := ins(1, nil, tKid, "Thông thường trẻ dưới 6 tuổi miễn vé.")
	ins(1, nil, tPets, "Thường nhận thú cưng nhỏ trong lồng.")
	ins(2, other, tPets, "Nhà xe O không nhận thú cưng") // nhà xe khác quy định riêng pets → câu hỏi mở
	pool.Exec(ctx, `INSERT INTO review_items (operator_id, key, topic, change_kind, risk, item_id, proposed_by)
		VALUES ($1, $2, $3, 'CONFLICT', 'high', $4::uuid, 'system:ingest')`, op, tFare+".x", tFare,
		ins(2, op, tFare+"x", "pending giả"))
	pool.Exec(ctx, `UPDATE items SET status = 'pending' WHERE topic = $1`, tFare+"x")
	pool.Exec(ctx, `UPDATE review_items SET topic = $1 WHERE operator_id = $2`, tSched, op) // CONFLICT ở sched

	tpl := kb.Template{Sections: []kb.Section{
		{Topic: tFare, Level: "required", Title: "Giá vé"},
		{Topic: tSched, Level: "required", Title: "Lịch chạy", Questions: []string{"Mỗi tuyến chạy mấy giờ?"}},
		{Topic: tKid, Level: "recommended", Title: "Trẻ em", Questions: []string{"Trẻ em tính vé thế nào?"}},
		{Topic: tPets, Level: "recommended", Title: "Thú cưng", Questions: []string{"Có nhận thú cưng không?"}},
		{Topic: "x" + sfx, Level: "recommended", Title: "Gửi hàng", Questions: []string{"Có nhận gửi hàng?"}},
	}}
	cov, err := Get(ctx, pool, op, tpl)
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]string{}
	for _, s := range cov.Sections {
		st[s.Topic] = s.Status
	}
	if st[tFare] != "covered" || st[tSched] != "ambiguous" || st[tKid] != "industry_default" || st["x"+sfx] != "missing" ||
		cov.RequiredTotal != 2 || cov.RequiredDone != 1 || cov.Percent != 50 {
		t.Fatalf("coverage = %+v", cov)
	}

	q, err := Generate(ctx, pool, op, tpl, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	style := map[string]Question{}
	for _, x := range q.Questions {
		style[x.Topic] = x
	}
	// Bắt buộc trước; kid: ít nhà xe khai riêng → xác nhận nhanh theo thông lệ; pets: nhà xe khác quy định riêng
	// (tỉ lệ phụ thuộc số nhà xe trong DB test nên chỉ kiểm có câu hỏi); missing → câu hỏi mở từ template.
	if q.Questions[0].Topic != tSched || style[tSched].Style != "resolve" ||
		style[tKid].Style != "confirm" || style[tKid].Default != kidDefault ||
		!strings.Contains(style[tKid].Question, "trẻ dưới 6 tuổi miễn vé") ||
		style["x"+sfx].Style != "open" || style[tPets].Question == "" {
		t.Fatalf("questions = %+v", q.Questions)
	}
	if !strings.Contains(q.Message, "1. ") {
		t.Fatalf("message = %q", q.Message)
	}
	only, _ := Generate(ctx, pool, op, tpl, []string{tKid}, 10)
	if len(only.Questions) != 1 {
		t.Fatalf("lọc topic: %+v", only.Questions)
	}
}
