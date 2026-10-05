package recall

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/entity"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

// fakeEmbedder: vector one-hot theo từ khoá đầu tiên khớp trong câu — đủ để kiểm nhánh semantic không cần TEI.
type fakeEmbedder struct {
	axes map[string]int
	err  error
}

func (f fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	v := make([]float32, Dim)
	folded := textnorm.Fold(text)
	for word, axis := range f.axes {
		if strings.Contains(folded, word) {
			v[axis] = 1
			return v, nil
		}
	}
	v[Dim-1] = 1
	return v, nil
}

func axis(i int) string {
	v := make([]float32, Dim)
	v[i] = 1
	return pgvector(v)
}

type env struct {
	pool     *pgxpool.Pool
	op, opB  string
	tag      string // từ riêng của test để lọc khỏi dữ liệu L0/L1 có sẵn trong DB test
	day      func(s string) time.Time
	insertIt func(layer int, op any, kind, topic, key, text string, extra map[string]any) string
	t        *testing.T
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	e := &env{pool: pool, op: "rc" + sfx, opB: "rcb" + sfx, tag: "zq" + sfx, t: t}
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'Phương Nam'), ($2, 'B')`, e.op, e.opB); err != nil {
		t.Fatal(err)
	}
	var global []string
	e.day = func(s string) time.Time {
		d, _ := time.ParseInLocation("2006-01-02", s, time.FixedZone("ICT", 7*3600))
		return d
	}
	e.insertIt = func(layer int, op any, kind, topic, key, text string, extra map[string]any) string {
		t.Helper()
		text = text + " " + e.tag
		var keyArg any
		if key != "" {
			keyArg = key
		}
		var id string
		err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status, locked,
				valid_from, valid_to, search_text, embedding, proof_count, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', $8, $9, $10, $11, $12::vector, $13, $14) RETURNING id::text`,
			layer, op, kind, topic, keyArg, text, extra["value"], extra["locked"] == true, extra["valid_from"],
			extra["valid_to"], textnorm.SearchText(topic+" "+key+" "+text), extra["embedding"],
			orInt(extra["proof"], 1), orMap(extra["metadata"])).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		if layer <= 1 {
			global = append(global, id)
		}
		return id
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, global)
		pool.Exec(ctx, `DELETE FROM operators WHERE id IN ($1, $2)`, e.op, e.opB)
	})
	return e
}

func orInt(v any, def int) int {
	if i, ok := v.(int); ok {
		return i
	}
	return def
}

func orMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func ids(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

func byID(hits []Hit, id string) *Hit {
	for i := range hits {
		if hits[i].ID == id {
			return &hits[i]
		}
	}
	return nil
}

func TestRecallKeywordScopeAndLabels(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	luggage := e.insertIt(2, e.op, "policy", "luggage", "luggage.mien_phi", "Mỗi khách được mang 20kg hành lý miễn phí", nil)
	other := e.insertIt(2, e.opB, "policy", "luggage", "luggage.mien_phi", "Nhà xe B: 30kg hành lý miễn phí", nil)
	l1 := e.insertIt(1, nil, "policy", "luggage", "", "Thông thường hành lý quá 20kg tính thêm phí",
		map[string]any{"metadata": map[string]any{"label": "thông lệ chung", "source": "L1/xe-khach/rules.yaml"}})
	l0 := e.insertIt(0, nil, "policy", "other", "", "Không bịa thông tin hành lý khi chưa có dữ liệu",
		map[string]any{"locked": true})
	pets := e.insertIt(2, e.op, "policy", "pets", "pets.cho_meo", "Không nhận chó mèo", nil)

	r := &Recaller{DB: e.pool}
	res, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "hanh ly mien phi " + e.tag})
	if err != nil {
		t.Fatal(err)
	}
	if byID(res.Hits, other) != nil {
		t.Fatal("lộ item của nhà xe khác")
	}
	if len(res.Degraded) == 0 || !strings.Contains(res.Degraded[0], "semantic") {
		t.Fatalf("thiếu TEI phải báo degraded: %v", res.Degraded)
	}
	h := byID(res.Hits, luggage)
	if h == nil || res.Hits[0].ID != luggage {
		t.Fatalf("item nhà xe khớp nhất phải đứng đầu: %v", ids(res.Hits))
	}
	if h.Layer != "L2" || h.Label != "nhà xe" || h.Facts != nil || h.Arms[0] != "keyword" {
		t.Fatalf("hit L2 = %+v", h)
	}
	if g := byID(res.Hits, l1); g == nil || g.Layer != "L1" || g.Label != "thông lệ chung" ||
		g.Source == nil || g.Source.KBFile != "L1/xe-khach/rules.yaml" {
		t.Fatalf("hit L1 = %+v", g)
	}
	if g := byID(res.Hits, l0); g == nil || g.Label != "quy tắc nền tảng (bắt buộc)" || !g.Locked {
		t.Fatalf("hit L0 = %+v", g)
	}

	// Lọc topic / kind.
	res, _ = r.Recall(ctx, Query{OperatorID: e.op, Text: "hanh ly " + e.tag, Topics: []string{"pets"}})
	if byID(res.Hits, luggage) != nil || byID(res.Hits, pets) == nil {
		t.Fatalf("lọc topic pets: %v", ids(res.Hits))
	}
	// Chỉ có topics → liệt kê theo topic.
	res, _ = r.Recall(ctx, Query{OperatorID: e.op, Topics: []string{"pets"}})
	if byID(res.Hits, pets) == nil || res.Hits[0].Arms[0] != "browse" {
		t.Fatalf("browse pets: %v", ids(res.Hits))
	}
	if _, err := r.Recall(ctx, Query{OperatorID: e.op}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("query rỗng: %v", err)
	}
}

func TestRecallValidity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	endOct := e.day("2026-11-01").Add(-time.Microsecond)
	old := e.insertIt(2, e.op, "data", "fare", "fare.sg_dl", "Giá vé Sài Gòn Đà Lạt 300.000đ",
		map[string]any{"valid_to": endOct})
	neu := e.insertIt(2, e.op, "data", "fare", "fare.sg_dl", "Giá vé Sài Gòn Đà Lạt 320.000đ",
		map[string]any{"valid_from": e.day("2026-11-01")})
	gone := e.insertIt(2, e.op, "data", "fare", "fare.tet", "Giá vé Tết 450.000đ",
		map[string]any{"valid_from": e.day("2026-01-20"), "valid_to": e.day("2026-02-10")})

	r := &Recaller{DB: e.pool}
	q := func(day string) []string {
		start := e.day(day)
		res, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "gia ve " + e.tag, ValidAt: start,
			ValidUntil: start.Add(24*time.Hour - time.Microsecond)})
		if err != nil {
			t.Fatal(err)
		}
		return only(ids(res.Hits), old, neu, gone) // DB test có thể có L0/L1 nói về giá vé (kb sync)
	}
	if got := q("2026-10-31"); len(got) != 1 || got[0] != old {
		t.Fatalf("31/10 phải ra giá cũ: %v", got)
	}
	if got := q("2026-11-01"); len(got) != 1 || got[0] != neu {
		t.Fatalf("01/11 phải ra giá mới: %v", got)
	}
	if got := q("2026-02-01"); len(got) != 2 || !contains(got, gone) || !contains(got, old) {
		t.Fatalf("01/02 phải có giá Tết + giá cũ: %v", got)
	}
}

func only(xs []string, keep ...string) []string {
	var out []string
	for _, x := range xs {
		if contains(keep, x) {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestRecallSemanticRRFAndDegrade(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// Item không chung từ nào với query nhưng gần nghĩa (cùng trục vector) → chỉ nhánh semantic tìm ra.
	pets := e.insertIt(2, e.op, "policy", "pets", "pets.cho_meo", "Không nhận chó mèo lên xe",
		map[string]any{"embedding": axis(1)})
	// Cả hai nhánh khớp → RRF cộng điểm, đứng trên.
	both := e.insertIt(2, e.op, "policy", "pets", "pets.thu_cung", "Thú cưng nhỏ để trong lồng được mang theo",
		map[string]any{"embedding": axis(1)})
	// Vector trực giao (cosine 0) → dưới ngưỡng, nhánh semantic bỏ.
	far := e.insertIt(2, e.op, "policy", "luggage", "luggage.cong_kenh", "Hàng cồng kềnh báo trước",
		map[string]any{"embedding": axis(2)})

	emb := fakeEmbedder{axes: map[string]int{"thu cung": 1}}
	r := &Recaller{DB: e.pool, Embedder: emb}
	res, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "thú cưng"})
	if err != nil {
		t.Fatal(err)
	}
	got := only(ids(res.Hits), pets, both, far)
	if len(got) < 2 || got[0] != both || !contains(got, pets) || contains(got, far) {
		t.Fatalf("semantic + RRF: %v", got)
	}
	if h := byID(res.Hits, both); len(h.Arms) != 2 {
		t.Fatalf("hit khớp 2 nhánh: %v", h.Arms)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("degraded: %v", res.Degraded)
	}

	// TEI lỗi → vẫn có kết quả keyword, báo degraded.
	r.Embedder = fakeEmbedder{err: errors.New("TEI down")}
	res, err = r.Recall(ctx, Query{OperatorID: e.op, Text: "thú cưng"})
	if err != nil {
		t.Fatal(err)
	}
	// Lọc theo item của test: DB test có thể có sẵn L1 nói về thú cưng (kb sync).
	if got := only(ids(res.Hits), pets, both, far); len(got) != 1 || got[0] != both || len(res.Degraded) != 1 {
		t.Fatalf("degrade: %v %v", got, res.Degraded)
	}
}

func TestRecallBudgetAndBoost(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		e.insertIt(2, e.op, "policy", "other", fmt.Sprintf("other.quy_dinh_%d", i),
			strings.Repeat("Quy định chung về đón trả khách dọc đường. ", 10), nil)
	}
	r := &Recaller{DB: e.pool}
	res, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "don tra khach " + e.tag, MaxTokens: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Total < 30 || len(res.Hits) == 0 || len(res.Hits) >= 30 {
		t.Fatalf("cắt theo token: hits=%d total=%d truncated=%v", len(res.Hits), res.Total, res.Truncated)
	}
	// Một hit vượt ngân sách vẫn được trả (ít nhất 1).
	res, _ = r.Recall(ctx, Query{OperatorID: e.op, Text: "don tra khach " + e.tag, MaxTokens: 1})
	if len(res.Hits) != 1 {
		t.Fatalf("tối thiểu 1 hit: %d", len(res.Hits))
	}

	// Boost: cùng thứ hạng, tầng nhà xe (L2) xếp trên thông lệ (L1).
	now := time.Now()
	l2 := boost(Hit{Layer: "L2", proof: 1, createdAt: now}, now)
	l1 := boost(Hit{Layer: "L1", proof: 1, createdAt: now}, now)
	proven := boost(Hit{Layer: "L2", proof: 5, createdAt: now}, now)
	oldOne := boost(Hit{Layer: "L2", proof: 1, createdAt: now.AddDate(-2, 0, 0)}, now)
	if !(proven > l2 && l2 > l1 && l2 > oldOne) {
		t.Fatalf("boost: proven=%v l2=%v l1=%v old=%v", proven, l2, l1, oldOne)
	}
}

// Khoảng hiệu lực NỬA MỞ [valid_from, valid_to): đúng mốc chuyển (valid_to cũ = valid_from mới)
// chỉ bản MỚI có hiệu lực — bản cũ hết ngay tại mốc, không trả cả hai giá trong cùng ngày.
func TestQueryDataHalfOpen(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	switchDay := e.day("2026-11-01")
	oldFare := e.insertIt(2, e.op, "data", "fare", "fare.sg_dl", "SG→ĐL 320.000đ",
		map[string]any{"value": []byte(`{"facts": {"gia_ve": "320000"}}`),
			"valid_from": e.day("2026-01-01"), "valid_to": switchDay})
	newFare := e.insertIt(2, e.op, "data", "fare", "fare.sg_dl", "SG→ĐL 350.000đ",
		map[string]any{"value": []byte(`{"facts": {"gia_ve": "350000"}}`),
			"valid_from": switchDay})

	for _, tc := range []struct {
		day  string
		want string // id bản phải thấy; "" = không thấy bản nào
	}{
		{"2026-10-31", oldFare},
		{"2026-11-01", newFare}, // mốc chuyển: chỉ bản mới
		{"2026-12-01", newFare},
	} {
		res, err := QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Topics: []string{"fare"}, At: e.day(tc.day)})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 || res.Rows[0].ID != tc.want {
			t.Fatalf("ngày %s: muốn %s, được %+v", tc.day, tc.want, res.Rows)
		}
	}
}

// (r72 finding) "7 giờ sáng" phải sinh token "07" để khớp dữ liệu "07:30".
func TestTsQueryHourPad(t *testing.T) {
	got := tsQuery("Xe 7 giờ sáng thứ 2 4 6 chạy tuyến nào?")
	if !strings.Contains(got, "07") {
		t.Fatalf("thiếu arm 07: %q", got)
	}
	got2 := tsQuery("có xe 21h không")
	if !strings.Contains(got2, "21") {
		t.Fatalf("giờ 2 chữ số giữ nguyên: %q", got2)
	}
	got3 := tsQuery("giá vé 320000")
	for _, arm := range strings.Split(got3, " | ") {
		if arm == "03" || arm == "32" { // token ĐÚNG, không phải substring của "320000"
			t.Fatalf("số không theo giờ không được pad thành token: %q", got3)
		}
	}
}

func TestQueryData(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	val := func(f string) map[string]any { return map[string]any{"value": []byte(f)} }
	dl := e.insertIt(2, e.op, "data", "fare", "fare.sai_gon_da_lat.giuong_nam", "Sài Gòn → Đà Lạt giường nằm 320.000đ",
		val(`{"facts": {"diem_di": "Sài Gòn", "diem_den": "Đà Lạt", "gia_ve": "320000"}}`))
	nt := e.insertIt(2, e.op, "data", "fare", "fare.sai_gon_nha_trang", "Sài Gòn → Nha Trang 280.000đ",
		val(`{"facts": {"diem_di": "Sài Gòn", "diem_den": "Nha Trang", "gia_ve": 280000}}`))
	tet := e.insertIt(2, e.op, "data", "fare", "fare.sai_gon_da_lat.tet", "Giá Tết Sài Gòn → Đà Lạt 450.000đ",
		map[string]any{"value": []byte(`{"facts": {"diem_den": "Đà Lạt", "gia_ve": "450000"}}`),
			"valid_from": time.Now().AddDate(0, 2, 0)})
	e.insertIt(2, e.op, "policy", "fare", "fare.tre_em", "Trẻ em dưới 5 tuổi miễn phí", nil)
	e.insertIt(2, e.opB, "data", "fare", "fare.sai_gon_da_lat", "Nhà xe B Sài Gòn Đà Lạt 999.000đ", nil)

	res, err := QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Topics: []string{"fare"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || len(res.Upcoming) != 0 {
		t.Fatalf("chỉ data đang hiệu lực của nhà xe: %+v", res)
	}
	if r := res.Rows[1]; r.ID != nt || r.Facts["gia_ve"] != "280000" {
		t.Fatalf("fact số giữ dạng JSON: %+v", r)
	}
	res, _ = QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Match: "da lat giường", IncludeUpcoming: true})
	if len(res.Rows) != 1 || res.Rows[0].ID != dl || len(res.Upcoming) != 0 {
		t.Fatalf("match: %+v", res)
	}
	res, _ = QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Facts: map[string]string{"diem den": "da lat"},
		IncludeUpcoming: true})
	if len(res.Rows) != 1 || len(res.Upcoming) != 1 || res.Upcoming[0].ID != tet {
		t.Fatalf("facts + upcoming: %+v", res)
	}
	res, _ = QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Facts: map[string]string{"diem_den": "da la"}})
	if len(res.Rows) != 0 {
		t.Fatalf("facts khớp trọn từ, không khớp nửa cụm: %+v", res)
	}
}

func TestEntityAliases(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ents, err := entity.New([]entity.Entity{
		{ID: "hcm", Type: "city", Name: "TP.HCM", Aliases: []string{"Sài Gòn", "SG"}},
		{ID: "da_lat", Type: "city", Name: "Đà Lạt"},
		{ID: "giuong_nam", Type: "vehicle_type", Name: "giường nằm", Aliases: []string{"sleeper"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	val := func(f string) map[string]any { return map[string]any{"value": []byte(f)} }
	dl := e.insertIt(2, e.op, "data", "fare", "fare.sai_gon_da_lat.giuong_nam", "Sài Gòn → Đà Lạt giường nằm 320.000đ",
		val(`{"facts": {"diem_di": "Sài Gòn", "diem_den": "Đà Lạt"}}`))
	e.insertIt(2, e.op, "data", "fare", "fare.ha_noi_sa_pa", "Hà Nội → Sa Pa 350.000đ", nil)

	// query_data: "SG" = "Sài Gòn", "sleeper" = "giường nằm"; facts theo thực thể.
	res, err := QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Match: "SG đà lạt sleeper", Entities: ents})
	if err != nil || len(res.Rows) != 1 || res.Rows[0].ID != dl {
		t.Fatalf("match alias: %+v %v", res, err)
	}
	if res, _ := QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Match: "SG đà lạt"}); len(res.Rows) != 0 {
		t.Fatalf("không có từ điển thì 'SG' không khớp: %+v", res)
	}
	res, _ = QueryData(ctx, e.pool, DataQuery{OperatorID: e.op, Facts: map[string]string{"diem_di": "TP.HCM"}, Entities: ents})
	if len(res.Rows) != 1 || res.Rows[0].ID != dl {
		t.Fatalf("facts alias: %+v", res)
	}

	// recall: query "SG" tìm ra item viết "Sài Gòn" (nhánh keyword mở rộng alias).
	r := &Recaller{DB: e.pool, Entities: ents}
	rec, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "giá vé SG", Topics: []string{"fare"}})
	if err != nil || byID(rec.Hits, dl) == nil || rec.Hits[0].ID != dl {
		t.Fatalf("recall alias: %v %v", ids(rec.Hits), err)
	}
}

// S3.1.1 graph arm + S3.1.2 temporal arm.
func TestRecallGraphAndTemporalArms(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tet, err := time.ParseInLocation("2006-01-02", "2027-02-04", time.FixedZone("ICT", 7*3600))
	if err != nil {
		t.Fatal(err)
	}

	// Hai entity L1 (không dấu test) + liên kết item_entities cho item tuyến.
	sgID := e.insertEntity("sgx", "Sài Gòn", []string{"SG"})
	dlID := e.insertEntity("dlx", "Đà Lạt", nil)
	route := e.insertIt(2, e.op, "data", "route", "route.sg_dl",
		"Chuyến Sài Gòn đi Đà Lạt khởi hành 21:30 mỗi tối", nil)
	for _, eid := range []string{sgID, dlID} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO item_entities (item_id, entity_id)
			VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, route, eid); err != nil {
			t.Fatal(err)
		}
	}
	// Temporal: giá thường quanh năm và giá Tết (cửa sổ hẹp chứa 04/02/2027).
	base := e.insertIt(2, e.op, "data", "fare", "fare.sg_dl",
		"Giá tuyến Sài Gòn Đà Lạt giường nằm 320000", map[string]any{
			"valid_from": e.day("2026-01-01"), "valid_to": e.day("2027-12-31")})
	tetFare := e.insertIt(2, e.op, "data", "fare", "fare.sg_dl.tet",
		"Giá Tết tuyến Sài Gòn Đà Lạt giường nằm 450000", map[string]any{
			"valid_from": e.day("2027-02-01"), "valid_to": e.day("2027-02-10")})

	resolver, err := entity.New([]entity.Entity{
		{ID: "sgx", Type: "city", Name: "Sài Gòn", Aliases: []string{"SG"}},
		{ID: "dlx", Type: "city", Name: "Đà Lạt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &Recaller{DB: e.pool, Entities: resolver}

	// Graph: query nhắc thực thể → item tuyến (keyword yếu vì text khác) vẫn được trả qua arm graph.
	res, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "SG Đà Lạt " + e.tag})
	if err != nil {
		t.Fatal(err)
	}
	if h := byID(res.Hits, route); h == nil || !contains(h.Arms, "graph") {
		t.Fatalf("graph arm phải tìm ra item tuyến: %+v", res.Hits)
	}

	// Temporal: hỏi giá ngày 04/02/2027 → giá Tết có phiếu temporal và đứng trên giá thường.
	res, err = r.Recall(ctx, Query{OperatorID: e.op, Text: "giá giường nằm " + e.tag,
		ValidAt: tet, ValidUntil: tet.Add(24*time.Hour - time.Microsecond)})
	if err != nil {
		t.Fatal(err)
	}
	tetHit := byID(res.Hits, tetFare)
	baseHit := byID(res.Hits, base)
	if tetHit == nil || !contains(tetHit.Arms, "temporal") {
		t.Fatalf("giá Tết phải được tìm ra qua temporal arm: %+v", res.Hits)
	}
	if baseHit != nil && tetHit.Score <= baseHit.Score {
		t.Fatalf("giá Tết phải trên giá thường: tet=%v base=%v arms=%v",
			tetHit.Score, baseHit.Score, tetHit.Arms)
	}
}

// insertEntity: entity L1 của kb test (ext_id) — trả uuid.
func (e *env) insertEntity(extID, name string, aliases []string) string {
	e.t.Helper()
	if aliases == nil {
		aliases = []string{}
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO entities (ext_id, layer, entity_type, name, name_norm, aliases)
		VALUES ($1, 1, 'city', $2, $3, $4)
		ON CONFLICT (ext_id) DO UPDATE SET name = EXCLUDED.name RETURNING id::text`,
		extID, name, textnorm.Fold(name), aliases).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// S3.1.3: rerank đổi thứ tự phần đầu; quá ngân sách → giữ RRF + degraded.
type fakeReranker struct {
	order []int
	delay time.Duration
}

func (f *fakeReranker) Rerank(ctx context.Context, query string, docs []string) ([]int, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
			return f.order, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.order, nil
}

func TestRecallRerank(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.insertIt(2, e.op, "policy", "fare"+e.tag, "k1", "Nội dung A về giá vé", nil)
	b := e.insertIt(2, e.op, "policy", "fare"+e.tag, "k2", "Nội dung B về giá vé", nil)
	r := &Recaller{DB: e.pool}

	// Thứ tự RRF gốc.
	base, err := r.Recall(ctx, Query{OperatorID: e.op, Text: "giá vé " + e.tag})
	if err != nil || len(base.Hits) < 2 {
		t.Fatalf("base = %+v %v", base.Hits, err)
	}
	first := base.Hits[0].ID

	// Reranker đảo 2 vị trí đầu → thứ tự mới, Reranked = true.
	r2 := &Recaller{DB: e.pool, Reranker: &fakeReranker{order: []int{1, 0}}}
	res, err := r2.Recall(ctx, Query{OperatorID: e.op, Text: "giá vé " + e.tag})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reranked || res.Hits[0].ID == first {
		t.Fatalf("rerank phải đổi thứ tự: %+v", ids(res.Hits))
	}
	// Không mất hit.
	if len(res.Hits) != len(base.Hits) {
		t.Fatalf("mất hit sau rerank: %d vs %d", len(res.Hits), len(base.Hits))
	}
	_ = a
	_ = b

	// Reranker chậm hơn 80ms → timeout: giữ RRF, báo degraded.
	r3 := &Recaller{DB: e.pool, Reranker: &fakeReranker{order: []int{1, 0}, delay: 300 * time.Millisecond}}
	res, err = r3.Recall(ctx, Query{OperatorID: e.op, Text: "giá vé " + e.tag})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reranked || res.Hits[0].ID != first || len(res.Degraded) == 0 {
		t.Fatalf("timeout phải giữ RRF: reranked=%v degraded=%v", res.Reranked, res.Degraded)
	}
}
