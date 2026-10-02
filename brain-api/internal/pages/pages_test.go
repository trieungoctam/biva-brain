package pages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/pack"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func bySlug(ps []Page) map[string]Page {
	m := make(map[string]Page, len(ps))
	for _, p := range ps {
		m[p.Slug] = p
	}
	return m
}

func TestRenderPages(t *testing.T) {
	p := pack.Pack{
		Operator: "phuongnam", AsOf: "2026-10-01", KnowledgeVersion: "3.7",
		Rules: []pack.Entry{{ID: "r1", Layer: "L0", Locked: true, Topic: "fare",
			Text: "Không xác nhận giá khi chưa có ngày đi."}},
		Policies: []pack.Entry{
			{ID: "p2", Layer: "L2", Topic: "pets", Text: "Không nhận chó mèo.", Overrides: []string{"p1"}},
			{ID: "p3", Layer: "L1", Label: "thông lệ chung", Topic: "kid", Text: "Trẻ em dưới 6 tuổi ngồi chung miễn phí."},
		},
		Lessons:     []pack.Entry{{ID: "l1", Layer: "L2", Kind: "lesson", Topic: "other", Text: "Không nhầm bến Miền Đông mới/cũ."}},
		DataSummary: []pack.DataTopic{{Topic: "fare", Title: "Giá vé", Count: 6, Updated: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), Lookup: "get_fare"}},
		Gaps: []pack.Gap{
			{Topic: "luggage", Title: "Hành lý", Level: "required", Status: "missing"},
			{Topic: "pickup", Title: "Điểm đón", Level: "recommended", Status: "industry_default"},
		},
	}
	tmpl := kb.Template{Capabilities: []kb.Capability{
		{ID: "fare", Level: "required", Title: "Tính giá", Modes: []string{"config", "hook", "custom"}},
	}}
	ps := Render(p, tmpl)
	if len(ps) != 5 || len(Slugs()) != 5 {
		t.Fatalf("phải có đúng 5 trang: %d", len(ps))
	}
	m := bySlug(ps)
	for slug, pg := range m {
		if !strings.Contains(pg.Markdown, "# phuongnam — "+pg.Title) || !strings.Contains(pg.Markdown, "version 3.7") {
			t.Fatalf("%s thiếu tiêu đề/version:\n%s", slug, pg.Markdown)
		}
	}
	if md := m["tong-quan"].Markdown; !strings.Contains(md, "- Chính sách: 2 mục, trong đó 1 mục khác thông lệ ngành") ||
		!strings.Contains(md, "Quy tắc bắt buộc (locked): 1") ||
		!strings.Contains(md, "fare (Giá vé): 6 mục, cập nhật 2026-09-30 — get_fare") {
		t.Fatalf("tong-quan sai:\n%s", md)
	}
	if md := m["chinh-sach"].Markdown; !strings.Contains(md, "## Quy tắc bắt buộc") || !strings.Contains(md, "[[r1]]") ||
		!strings.Contains(md, "[L1 · thông lệ chung] kid") || !strings.Contains(md, "[[l1]]") {
		t.Fatalf("chinh-sach sai:\n%s", md)
	}
	if md := m["khac-thong-le"].Markdown; !strings.Contains(md, "[[p2]]") || !strings.Contains(md, "thay thông lệ [[p1]]") ||
		strings.Contains(md, "[[p3]]") {
		t.Fatalf("khac-thong-le sai:\n%s", md)
	}
	if md := m["con-thieu"].Markdown; !strings.Contains(md, "[bắt buộc] Hành lý (luggage) — chưa có") ||
		!strings.Contains(md, "[khuyến nghị] Điểm đón (pickup) — đang dùng thông lệ chung, cần nhà xe xác nhận") ||
		!strings.Contains(md, "generate_questions") {
		t.Fatalf("con-thieu sai:\n%s", md)
	}
	if md := m["logic"].Markdown; !strings.Contains(md, "[bắt buộc] fare — Tính giá (config · hook · custom)") ||
		!strings.Contains(md, "Hồ sơ logic của nhà xe: chưa có") {
		t.Fatalf("logic sai:\n%s", md)
	}
}

func TestRenderEmptyPack(t *testing.T) {
	m := bySlug(Render(pack.Pack{Operator: "x", AsOf: "2026-10-01", KnowledgeVersion: "0.0"}, kb.Template{}))
	if md := m["khac-thong-le"].Markdown; !strings.Contains(md, "Chưa có mục nào khác thông lệ") {
		t.Fatalf("khac-thong-le rỗng sai:\n%s", md)
	}
	if md := m["con-thieu"].Markdown; !strings.Contains(md, "Đủ mọi mục của template ngành.") {
		t.Fatalf("con-thieu rỗng sai:\n%s", md)
	}
	if md := m["chinh-sach"].Markdown; !strings.Contains(md, "Chưa có chính sách nào đang hiệu lực.") {
		t.Fatalf("chinh-sach rỗng sai:\n%s", md)
	}
}

func TestRefreshAndRead(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op, op2 := "pg"+sfx, "qg"+sfx
	tPets, tLug := "pets"+sfx, "lug"+sfx
	for _, id := range []string{op, op2} {
		if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'P')`, id); err != nil {
			t.Fatal(err)
		}
	}
	var l1, l2 string
	if err := pool.QueryRow(ctx, `INSERT INTO items (layer, kind, topic, key, text, status, metadata)
			VALUES (1, 'policy', $1, $1 || '.q', 'Không nhận chó mèo trên xe.', 'active', $2) RETURNING id::text`,
		tPets, map[string]any{"label": "thông lệ chung"}).Scan(&l1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
			VALUES (2, $1, 'policy', $2, $2 || '.rieng', 'Nhận chó nhỏ có lồng.', 'active') RETURNING id::text`,
		op, tPets).Scan(&l2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, []string{l1, l2})
		pool.Exec(ctx, `DELETE FROM operators WHERE id IN ($1, $2)`, op, op2) // cascade operator_pages
	})

	specs := []pack.TopicSpec{{ID: tPets, Title: "Thú nuôi", Required: true}, {ID: tLug, Title: "Hành lý", Required: true}}
	builder := &pack.Builder{DB: pool, Topics: specs}

	// Refresh lần đầu: 5 trang đúng version tri thức hiện tại.
	n, err := Refresh(ctx, pool, builder, kb.Template{})
	if err != nil || n < 1 {
		t.Fatalf("Refresh = %d, %v", n, err)
	}
	ver, err := pack.Version(ctx, pool, op)
	if err != nil {
		t.Fatal(err)
	}
	var got int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_pages WHERE operator_id = $1 AND knowledge_version = $2`,
		op, ver).Scan(&got); err != nil || got != 5 {
		t.Fatalf("operator_pages = %d rows (version %s), err=%v", got, ver, err)
	}
	var chinh, khac string
	if err := pool.QueryRow(ctx, `SELECT markdown FROM operator_pages WHERE operator_id = $1 AND slug = 'chinh-sach'`, op).Scan(&chinh); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chinh, "Nhận chó nhỏ có lồng.") || !strings.Contains(chinh, "[["+l2+"]]") {
		t.Fatalf("chinh-sach thiếu chính sách L2:\n%s", chinh)
	}
	if err := pool.QueryRow(ctx, `SELECT markdown FROM operator_pages WHERE operator_id = $1 AND slug = 'khac-thong-le'`, op).Scan(&khac); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(khac, "thay thông lệ [["+l1+"]]") {
		t.Fatalf("khac-thong-le thiếu thông lệ bị thay:\n%s", khac)
	}

	// Đổi tri thức → trigger tăng version → Refresh dựng lại, nội dung mới.
	if _, err := pool.Exec(ctx, `UPDATE items SET text = 'Không nhận thú nuôi.' WHERE id = $1`, l2); err != nil {
		t.Fatal(err)
	}
	if n, err := Refresh(ctx, pool, builder, kb.Template{}); err != nil || n < 1 {
		t.Fatalf("Refresh sau đổi tri thức = %d, %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT markdown FROM operator_pages WHERE operator_id = $1 AND slug = 'chinh-sach'`, op).Scan(&chinh); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chinh, "Không nhận thú nuôi.") {
		t.Fatalf("chinh-sach chưa dựng lại:\n%s", chinh)
	}
	ver2, _ := pack.Version(ctx, pool, op)
	if ver2 == ver {
		t.Fatalf("version tri thức không đổi: %s", ver2)
	}

	// Read: nhà xe chưa có trang → dựng tại chỗ đủ 5 trang.
	md, err := Read(ctx, pool, builder, kb.Template{}, op2, "tong-quan")
	if err != nil || !strings.Contains(md, "# "+op2+" — Tổng quan") {
		t.Fatalf("Read lạnh = %v\n%s", err, md)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_pages WHERE operator_id = $1`, op2).Scan(&got); err != nil || got != 5 {
		t.Fatalf("Read lạnh phải dựng đủ 5 trang, được %d (err=%v)", got, err)
	}
	// Read: bản lưu đúng version → dùng nguyên, không dựng lại.
	if _, err := pool.Exec(ctx, `UPDATE operator_pages SET markdown = 'BAN CU' WHERE operator_id = $1 AND slug = 'tong-quan'`, op2); err != nil {
		t.Fatal(err)
	}
	if md, err = Read(ctx, pool, builder, kb.Template{}, op2, "tong-quan"); err != nil || md != "BAN CU" {
		t.Fatalf("Read phải dùng bản lưu đúng version: %q %v", md, err)
	}
	// Read: slug lạ.
	if _, err := Read(ctx, pool, builder, kb.Template{}, op2, "khong-co"); !errors.Is(err, ErrUnknownSlug) {
		t.Fatalf("slug lạ phải ErrUnknownSlug, được %v", err)
	}
}

// Item có hiệu lực theo NGÀY (giá Tết, chính sách từ 01/11) không bump knowledge_version —
// trang dựng từ hôm trước (giờ VN) vẫn phải được coi outdated và dựng lại.
func TestRefreshRedoesPagesAfterVNDayChange(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op := "pd" + sfx
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'P')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	if _, err := pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
		VALUES (2, $1, 'policy', 'fare', 'fare.x', 'Giá cơ sở 320k', 'active')`, op); err != nil {
		t.Fatal(err)
	}
	builder := &pack.Builder{DB: pool, Topics: []pack.TopicSpec{{ID: "fare", Title: "Giá vé", Required: true}}}

	// Dựng trang bình thường, rồi đẩy refreshed_at về hôm trước (giờ VN).
	if _, err := Refresh(ctx, pool, builder, kb.Template{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operator_pages SET refreshed_at = now() - interval '25 hours'
		WHERE operator_id = $1`, op); err != nil {
		t.Fatal(err)
	}
	// Version tri thức KHÔNG đổi — Refresh vẫn phải chọn nhà xe này (điều kiện ngày VN).
	rows, err := pool.Query(ctx, `
		SELECT o.id FROM operators o
		LEFT JOIN knowledge_versions g ON g.scope = '*'
		LEFT JOIN knowledge_versions v ON v.scope = o.id
		WHERE concat(COALESCE(g.version, 0), '.', COALESCE(v.version, 0)) <>
		      COALESCE((SELECT MAX(knowledge_version) FROM operator_pages p WHERE p.operator_id = o.id), '')
		   OR COALESCE((SELECT MAX(p.refreshed_at) FROM operator_pages p WHERE p.operator_id = o.id), to_timestamp(0))
		      < date_trunc('day', now() AT TIME ZONE 'Asia/Ho_Chi_Minh') AT TIME ZONE 'Asia/Ho_Chi_Minh'`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	found := false
	for _, id := range ids {
		if id == op {
			found = true
		}
	}
	if !found {
		t.Fatal("nhà xe có trang dựng hôm trước phải nằm trong danh outdated (điều kiện ngày VN)")
	}
	// Read cũng không dùng cache cũ: sameVNDay sai → rebuild.
	if _, err := pool.Exec(ctx, `UPDATE operator_pages SET refreshed_at = now() - interval '25 hours'
		WHERE operator_id = $1`, op); err != nil {
		t.Fatal(err)
	}
	md, err := Read(ctx, pool, builder, kb.Template{}, op, "chinh-sach")
	if err != nil {
		t.Fatal(err)
	}
	if md == "" {
		t.Fatal("Read phải dựng lại trang thay vì trả rỗng")
	}
	// sameVNDay: hai mốc trong cùng ngày VN (dù khác múi gốc) phải bằng nhau.
	vnA := time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC) // = 01/11 06:30 sáng giờ VN
	vnB := time.Date(2026, 11, 1, 2, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	if !sameVNDay(vnA, vnB) {
		t.Fatal("hai mốc sáng sớm 01/11 theo giờ VN phải cùng ngày VN")
	}
	if !sameVNDay(vnB, vnB.Add(time.Hour)) {
		t.Fatal("02:00 → 03:00 cùng ngày VN")
	}
	vnC := vnB.Add(26 * time.Hour) // sang 02/11
	if sameVNDay(vnB, vnC) {
		t.Fatal("mốc cách 26 tiếng phải khác ngày VN")
	}
}
