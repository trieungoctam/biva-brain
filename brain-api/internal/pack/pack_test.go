package pack

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

type env struct {
	pool               *pgxpool.Pool
	op                 string
	tFare, tPets, tKid string // topic riêng của test (DB test có sẵn L0/L1 từ kb sync)
	global             []string
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	e := &env{pool: testdb.Pool(t), op: "pk" + sfx, tFare: "fare" + sfx, tPets: "pets" + sfx, tKid: "kid" + sfx}
	if _, err := e.pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'P')`, e.op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, e.global)
		e.pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, e.op)
	})
	return e
}

func (e *env) insert(t *testing.T, layer int, kind, topic, key, text string, locked bool, value string) string {
	t.Helper()
	var op, val any
	if layer == 2 {
		op = e.op
	}
	if value != "" {
		val = value
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO items (layer, operator_id, kind, topic, key, text, value,
			status, locked, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, 'active', $8, $9) RETURNING id::text`, layer, op, kind, topic, key, text,
		val, locked, map[string]any{"label": map[bool]string{true: "", false: "thông lệ chung"}[locked || layer != 1]}).
		Scan(&id); err != nil {
		t.Fatal(err)
	}
	if layer <= 1 {
		e.global = append(e.global, id)
	}
	return id
}

func ids(es []Entry) map[string]Entry {
	m := map[string]Entry{}
	for _, e := range es {
		m[e.ID] = e
	}
	return m
}

func TestPackMergeLayers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	sfx := e.op
	locked := e.insert(t, 0, "policy", "other", "l0.t"+sfx, "Không bịa thông tin.", true, "")
	petsDefault := e.insert(t, 1, "policy", e.tPets, "l1.default.pets"+sfx, "Thường nhận thú cưng nhỏ trong lồng.", false, "")
	kidDefault := e.insert(t, 1, "policy", e.tKid, "l1.default.kid"+sfx, "Trẻ dưới 6 tuổi thường miễn vé.", false, "")
	lesson := e.insert(t, 1, "lesson", e.tPets, "l1.lesson"+sfx, "Đừng hứa chở chó lớn.", false, "")
	pets := e.insert(t, 2, "policy", e.tPets, e.tPets+".cho_meo", "Không nhận chó mèo.", false, "")
	persona := e.insert(t, 2, "persona", "other", "persona.xung_ho", "Xưng 'nhà xe', gọi khách 'anh/chị'.", false, "")
	e.insert(t, 2, "data", e.tFare, e.tFare+".sg_dl", "Sài Gòn → Đà Lạt 320.000đ", false, `{"facts": {"gia_ve": "320000"}}`)
	e.insert(t, 2, "data", e.tFare, e.tFare+".sg_nt", "Sài Gòn → Nha Trang 280.000đ", false, "")

	b := &Builder{DB: e.pool, Topics: []TopicSpec{{ID: e.tFare, Title: "Giá vé", Required: true},
		{ID: "sched" + sfx, Title: "Lịch chạy", Required: true}, {ID: e.tPets, Title: "Thú cưng"},
		{ID: e.tKid, Title: "Trẻ em"}}}
	p, err := b.Build(ctx, Query{OperatorID: e.op})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ids(p.Rules)[locked]; !ok {
		t.Fatal("thiếu rule locked")
	}
	pol := ids(p.Policies)
	if _, ok := pol[petsDefault]; ok {
		t.Fatal("thông lệ L1 của topic nhà xe đã có phải bị thay")
	}
	if got := pol[pets]; got.Layer != "L2" || len(got.Overrides) != 1 || got.Overrides[0] != petsDefault {
		t.Fatalf("item L2 phải ghi overrides: %+v", got)
	}
	if got := pol[kidDefault]; got.Label != "thông lệ chung" {
		t.Fatalf("thông lệ L1 khi nhà xe chưa có phải mang nhãn: %+v", got)
	}
	if _, ok := ids(p.Lessons)[lesson]; !ok {
		t.Fatal("thiếu lesson")
	}
	if _, ok := ids(p.Persona)[persona]; !ok {
		t.Fatal("thiếu persona")
	}
	var fare *DataTopic
	for i := range p.DataSummary {
		if p.DataSummary[i].Topic == e.tFare {
			fare = &p.DataSummary[i]
		}
	}
	if fare == nil || fare.Count != 2 || fare.Title != "Giá vé" || !strings.Contains(fare.Lookup, "query_data") || fare.Samples[0].ID == "" {
		t.Fatalf("data_summary = %+v", p.DataSummary)
	}
	gaps := map[string]Gap{}
	for _, g := range p.Gaps {
		gaps[g.Topic] = g
	}
	if gaps["sched"+sfx].Status != "missing" || gaps["sched"+sfx].Level != "required" || gaps[e.tKid].Status != "industry_default" {
		t.Fatalf("gaps = %+v", p.Gaps)
	}
	if _, ok := gaps[e.tFare]; ok {
		t.Fatal("topic có data không phải gap")
	}
	if p.Gaps[0].Level != "required" || p.Cached || p.KnowledgeVersion == "" {
		t.Fatalf("pack = %+v", p)
	}

	// Ngân sách rất nhỏ: rule locked vẫn đủ, các phần khác bị cắt và báo omitted.
	small, _ := b.Build(ctx, Query{OperatorID: e.op, Budget: 50})
	if _, ok := ids(small.Rules)[locked]; !ok || len(small.Policies) != 0 || small.Omitted["policies"] == 0 ||
		!strings.Contains(strings.Join(small.NextActions, " "), "recall_knowledge") {
		t.Fatalf("ngân sách nhỏ: %+v", small)
	}
}

func TestPackCacheInvalidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	b := &Builder{DB: e.pool, Topics: []TopicSpec{{ID: e.tPets, Title: "Thú cưng"}}}
	pets := e.insert(t, 2, "policy", e.tPets, e.tPets+".cho_meo", "Không nhận chó mèo.", false, "")

	p1, _ := b.Build(ctx, Query{OperatorID: e.op})
	p2, _ := b.Build(ctx, Query{OperatorID: e.op})
	if p1.Cached || !p2.Cached || p1.KnowledgeVersion != p2.KnowledgeVersion {
		t.Fatalf("lần 2 phải trúng cache: %v %v", p1.Cached, p2.Cached)
	}

	// Job index chỉ ghi search_text/embedding, tăng proof_count → không đổi version, vẫn trúng cache.
	e.pool.Exec(ctx, `UPDATE items SET search_text = 'x', proof_count = proof_count + 1 WHERE id = $1`, pets)
	if p, _ := b.Build(ctx, Query{OperatorID: e.op}); !p.Cached {
		t.Fatal("index/proof_count không được làm mất cache")
	}

	// Sửa nội dung → version mới → dựng lại.
	e.pool.Exec(ctx, `UPDATE items SET text = 'Chỉ nhận mèo trong lồng.' WHERE id = $1`, pets)
	p3, _ := b.Build(ctx, Query{OperatorID: e.op})
	if p3.Cached || p3.KnowledgeVersion == p1.KnowledgeVersion || ids(p3.Policies)[pets].Text != "Chỉ nhận mèo trong lồng." {
		t.Fatalf("đổi tri thức phải dựng lại: %+v", p3)
	}
	// Item pending (đề xuất) không đổi pack.
	e.pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status) VALUES (2, $1, 'policy', $2, 'x', 'pending')`,
		e.op, e.tPets)
	if p, _ := b.Build(ctx, Query{OperatorID: e.op}); !p.Cached {
		t.Fatal("item pending không được làm mất cache")
	}
	// Superseded → mất khỏi pack.
	e.pool.Exec(ctx, `UPDATE items SET status = 'superseded' WHERE id = $1`, pets)
	if p, _ := b.Build(ctx, Query{OperatorID: e.op}); p.Cached {
		t.Fatal("superseded phải làm mất cache")
	} else if _, ok := ids(p.Policies)[pets]; ok {
		t.Fatalf("superseded phải rời pack: %+v", p.Policies)
	}
	// Ngày khác → key khác.
	if p, _ := b.Build(ctx, Query{OperatorID: e.op, Day: time.Date(2030, 1, 1, 0, 0, 0, 0, vn)}); p.Cached || p.AsOf != "2030-01-01" {
		t.Fatalf("as_of khác: %+v", p)
	}
	if _, err := b.Build(ctx, Query{OperatorID: e.op, Purpose: "xyz"}); err == nil {
		t.Fatal("purpose sai phải lỗi")
	}
}

// AC S1.4.2: get_knowledge_pack p95 < 800 ms (không cache) với nhà xe có 600 item active + L0/L1.
func TestPackLatency(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status)
		SELECT 2, $1, CASE WHEN g % 2 = 0 THEN 'data' ELSE 'policy' END, 'topic' || (g % 16), 'k.' || g,
			'Nội dung tri thức số ' || g || ' của nhà xe, đủ dài để giống thật một chút.',
			'{"facts": {"gia_ve": "320000"}}', 'active'
		FROM generate_series(1, 600) g`, e.op); err != nil {
		t.Fatal(err)
	}
	var took []time.Duration
	for i := 0; i < 20; i++ {
		b := &Builder{DB: e.pool} // Builder mới: không cache
		start := time.Now()
		if _, err := b.Build(ctx, Query{OperatorID: e.op, Budget: MaxBudget}); err != nil {
			t.Fatal(err)
		}
		took = append(took, time.Since(start))
	}
	slices.Sort(took)
	p95 := took[len(took)*95/100]
	t.Logf("pack p50=%v p95=%v", took[len(took)/2], p95)
	if p95 > 800*time.Millisecond {
		t.Fatalf("p95 = %v", p95)
	}
}
