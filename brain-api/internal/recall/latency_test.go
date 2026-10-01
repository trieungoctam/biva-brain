package recall

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

type randEmbedder struct{ r *rand.Rand }

func (e randEmbedder) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, Dim)
	for i := range v {
		v[i] = e.r.Float32()
	}
	return v, nil
}

// AC của S1.3.1 (recall) và S1.3.2 (query_data): p95 < 150 ms trên dữ liệu cỡ 3 pilot (3 nhà xe × 300 item có embedding + L0/L1).
// Đo cả 2 nhánh (semantic với vector ngẫu nhiên, keyword) + nạp chi tiết + cắt token; không tính thời gian TEI.
func TestRecallLatencyP95(t *testing.T) {
	if testing.Short() {
		t.Skip("bỏ qua đo latency khi -short")
	}
	e := setup(t)
	ctx := context.Background()
	ops := []string{e.op, e.opB, e.op + "c"}
	if _, err := e.pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'C')`, ops[2]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.pool.Exec(context.Background(), `DELETE FROM operators WHERE id = $1`, ops[2]) })
	words := []string{"gia ve", "giuong nam", "da lat", "nha trang", "don khach", "hanh ly", "tre em", "thu cung",
		"huy ve", "thanh toan", "chuyen khoan", "ben xe", "mien dong", "trung chuyen", "xe limousine", "tet"}
	for _, op := range ops {
		_, err := e.pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, search_text, embedding, status)
			SELECT 2, $1, 'data', 'fare', 'fare.k' || g, 'Quy định số ' || g || ' về ' || ($2::text[])[1 + g % 16],
				'quy dinh so ' || g || ' ve ' || ($2::text[])[1 + g % 16],
				(SELECT array_agg(random())::real[] FROM generate_series(1, 1024) x WHERE g > 0)::vector, 'active'
			FROM generate_series(1, 300) g`, op, words)
		if err != nil {
			t.Fatal(err)
		}
	}
	e.pool.Exec(ctx, `ANALYZE items`)

	r := &Recaller{DB: e.pool, Embedder: randEmbedder{rand.New(rand.NewSource(1))}}
	var took []time.Duration
	for i := 0; i < 60; i++ {
		start := time.Now()
		res, err := r.Recall(ctx, Query{OperatorID: ops[i%3], Text: fmt.Sprintf("%s %s", words[i%16], words[(i+5)%16])})
		if err != nil {
			t.Fatal(err)
		}
		if i >= 10 { // bỏ 10 lần đầu (khởi động kết nối, cache)
			took = append(took, time.Since(start))
		}
		if len(res.Hits) == 0 {
			t.Fatalf("query %d không có kết quả", i)
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	p95 := took[len(took)*95/100]
	t.Logf("recall p50=%v p95=%v (n=%d)", took[len(took)/2], p95, len(took))
	if p95 > 150*time.Millisecond {
		t.Fatalf("p95 = %v > 150ms", p95)
	}

	// AC S1.3.2: query_data p95 < 15 ms (300 item data mỗi nhà xe, lọc từ khoá trong Go).
	took = took[:0]
	for i := 0; i < 60; i++ {
		start := time.Now()
		res, err := QueryData(ctx, e.pool, DataQuery{OperatorID: ops[i%3], Topics: []string{"fare"}, Match: words[i%16]})
		if err != nil {
			t.Fatal(err)
		}
		if i >= 10 {
			took = append(took, time.Since(start))
		}
		if len(res.Rows) == 0 {
			t.Fatalf("query_data %d không có kết quả", i)
		}
	}
	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	p95 = took[len(took)*95/100]
	t.Logf("query_data p50=%v p95=%v (n=%d)", took[len(took)/2], p95, len(took))
	if p95 > 15*time.Millisecond {
		t.Fatalf("query_data p95 = %v > 15ms", p95)
	}
}
