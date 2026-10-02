// Package pages dựng và lưu Operator Profile pages (M2, S2.1.4 refresh_pages): 5 trang markdown cho mỗi
// nhà xe — Tổng quan · Chính sách · Khác thông lệ · Còn thiếu · Logic — lắp từ knowledge pack.
//
// Trang lưu trong bảng operator_pages kèm knowledge_version đã dựng. Job refresh_pages (scheduler leader,
// mỗi phút) dựng lại cho nhà xe có version tri thức đổi (trigger 000010 tăng version khi item active đổi).
// Resource MCP biva://operator/{id}/pages/<slug>.md đọc từ bảng; thiếu hoặc lệch version thì dựng tại chỗ —
// nội dung đọc được luôn là bản đúng version tri thức hiện tại.
package pages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/pack"
)

// ErrUnknownSlug: slug trang không thuộc bộ 5 trang của Operator Profile.
var ErrUnknownSlug = errors.New("slug không hợp lệ (tong-quan, chinh-sach, khac-thong-le, con-thieu, logic)")

type Page struct {
	Slug     string
	Title    string
	Markdown string
}

var catalog = []Page{
	{Slug: "tong-quan", Title: "Tổng quan"},
	{Slug: "chinh-sach", Title: "Chính sách"},
	{Slug: "khac-thong-le", Title: "Khác thông lệ"},
	{Slug: "con-thieu", Title: "Còn thiếu"},
	{Slug: "logic", Title: "Logic"},
}

// Slugs trả về slug của 5 trang, theo thứ tự catalog.
func Slugs() []string {
	ss := make([]string, len(catalog))
	for i, p := range catalog {
		ss[i] = p.Slug
	}
	return ss
}

// Title trả về tên hiển thị của slug (vd "tong-quan" → "Tổng quan").
func Title(slug string) (string, bool) {
	for _, p := range catalog {
		if p.Slug == slug {
			return p.Title, true
		}
	}
	return "", false
}

// Render dựng 5 trang từ knowledge pack và template ngành (thuần, không đụng DB).
func Render(p pack.Pack, tmpl kb.Template) []Page {
	ps := make([]Page, len(catalog))
	for i, c := range catalog {
		ps[i] = Page{Slug: c.Slug, Title: c.Title}
		switch c.Slug {
		case "tong-quan":
			ps[i].Markdown = overviewMD(p)
		case "chinh-sach":
			ps[i].Markdown = policyMD(p)
		case "khac-thong-le":
			ps[i].Markdown = nonStandardMD(p)
		case "con-thieu":
			ps[i].Markdown = gapsMD(p)
		case "logic":
			ps[i].Markdown = logicMD(p, tmpl)
		}
	}
	return ps
}

// Refresh (job refresh_pages): dựng lại trang cho mọi nhà xe có version tri thức khác bản đã lưu.
// Nhà xe dựng lỗi được bỏ qua (log, thử lại lượt sau) để không chặn các nhà xe khác. Trả số nhà xe đã dựng.
func Refresh(ctx context.Context, db *pgxpool.Pool, b *pack.Builder, tmpl kb.Template) (int, error) {
	// Tri thức có hiệu lực theo NGÀY (giá Tết, chính sách từ 01/11…): item active từ mai không
	// đổi knowledge_version hôm nay — sang ngày mai phải dựng lại trang dù version trùng.
	// Vì thế outdated = version khác HOẶC trang cuối dựng trước 00:00 giờ Việt Nam hôm nay.
	rows, err := db.Query(ctx, `
		SELECT o.id, concat(COALESCE(g.version, 0), '.', COALESCE(v.version, 0))
		FROM operators o
		LEFT JOIN knowledge_versions g ON g.scope = '*'
		LEFT JOIN knowledge_versions v ON v.scope = o.id
		WHERE concat(COALESCE(g.version, 0), '.', COALESCE(v.version, 0)) <>
		      COALESCE((SELECT MAX(knowledge_version) FROM operator_pages p WHERE p.operator_id = o.id), '')
		   OR COALESCE((SELECT MAX(p.refreshed_at) FROM operator_pages p WHERE p.operator_id = o.id), to_timestamp(0))
		      < date_trunc('day', now() AT TIME ZONE 'Asia/Ho_Chi_Minh') AT TIME ZONE 'Asia/Ho_Chi_Minh'`)
	if err != nil {
		return 0, err
	}
	type outdated struct {
		id      string
		version string
	}
	var todo []outdated
	for rows.Next() {
		var o outdated
		if err := rows.Scan(&o.id, &o.version); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, o)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	n := 0
	for _, o := range todo {
		p, err := b.Build(ctx, pack.Query{OperatorID: o.id, Budget: pack.MaxBudget})
		if err != nil {
			slog.Warn("refresh_pages: dựng pack lỗi, thử lại lượt sau", "operator", o.id, "err", err)
			continue
		}
		if err := store(ctx, db, o.id, Render(p, tmpl), p.KnowledgeVersion); err != nil {
			return n, fmt.Errorf("lưu trang %s: %w", o.id, err)
		}
		n++
	}
	return n, nil
}

// Read trả về markdown của một trang. Đúng version tri thức hiện tại: bản lưu còn hiệu lực thì dùng,
// thiếu hoặc đã cũ thì dựng lại tại chỗ từ knowledge pack rồi lưu.
func Read(ctx context.Context, db *pgxpool.Pool, b *pack.Builder, tmpl kb.Template, operatorID, slug string) (string, error) {
	known := false
	for _, p := range catalog {
		if p.Slug == slug {
			known = true
			break
		}
	}
	if !known {
		return "", ErrUnknownSlug
	}
	if pageMD, ver, at, ok, err := load(ctx, db, operatorID, slug); err != nil {
		return "", err
	} else if ok {
		cur, err := pack.Version(ctx, db, operatorID)
		if err != nil {
			return "", err
		}
		// Cùng version nhưng dựng từ hôm trước (giờ VN) vẫn phải dựng lại: item có thể vừa có
		// hiệu lực theo ngày mà không bump version.
		if ver == cur && sameVNDay(at, time.Now()) {
			return pageMD, nil
		}
	}
	p, err := b.Build(ctx, pack.Query{OperatorID: operatorID, Budget: pack.MaxBudget})
	if err != nil {
		return "", err
	}
	ps := Render(p, tmpl)
	if err := store(ctx, db, operatorID, ps, p.KnowledgeVersion); err != nil {
		return "", err
	}
	for _, pg := range ps {
		if pg.Slug == slug {
			return pg.Markdown, nil
		}
	}
	return "", ErrUnknownSlug
}

// load đọc một trang đã lưu; ok = false khi chưa có trang nào của slug.
func load(ctx context.Context, db *pgxpool.Pool, operatorID, slug string) (md, version string, at time.Time, ok bool, err error) {
	err = db.QueryRow(ctx, `SELECT markdown, knowledge_version, refreshed_at
		FROM operator_pages WHERE operator_id = $1 AND slug = $2`,
		operatorID, slug).Scan(&md, &version, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", time.Time{}, false, nil
	}
	if err != nil {
		return "", "", time.Time{}, false, err
	}
	return md, version, at, true, nil
}

// sameVNDay: hai mốc thời gian có nằm trong cùng một ngày lịch theo giờ Việt Nam.
func sameVNDay(a, b time.Time) bool {
	vn := time.FixedZone("ICT", 7*3600)
	ay, am, ad := a.In(vn).Date()
	by, bm, bd := b.In(vn).Date()
	return ay == by && am == bm && ad == bd
}

// store upsert cả 5 trang của một nhà xe trong một transaction (các trang luôn cùng knowledge_version).
func store(ctx context.Context, db *pgxpool.Pool, operatorID string, ps []Page, version string) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, p := range ps {
		if _, err := tx.Exec(ctx, `INSERT INTO operator_pages (operator_id, slug, title, markdown, knowledge_version, refreshed_at)
			VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (operator_id, slug) DO UPDATE SET
				title = EXCLUDED.title, markdown = EXCLUDED.markdown,
				knowledge_version = EXCLUDED.knowledge_version, refreshed_at = now()`,
			operatorID, p.Slug, p.Title, p.Markdown, version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func header(p pack.Pack, title string) string {
	return fmt.Sprintf("# %s — %s\n\nTri thức hiệu lực ngày %s (version %s).\n", p.Operator, title, p.AsOf, p.KnowledgeVersion)
}

// entryLine: "- [L2 · thông lệ chung] pets: Không nhận chó mèo (hiệu lực 01/11 → 31/12) [[id]]".
func entryLine(e pack.Entry) string {
	label := e.Layer
	if e.Label != "" {
		label += " · " + e.Label
	}
	eff := ""
	switch {
	case e.ValidFrom != nil && e.ValidTo != nil:
		eff = fmt.Sprintf(" (hiệu lực %s → %s)", e.ValidFrom.Format("2006-01-02"), e.ValidTo.Format("2006-01-02"))
	case e.ValidFrom != nil:
		eff = fmt.Sprintf(" (hiệu lực từ %s)", e.ValidFrom.Format("2006-01-02"))
	case e.ValidTo != nil:
		eff = fmt.Sprintf(" (hiệu lực tới %s)", e.ValidTo.Format("2006-01-02"))
	}
	return fmt.Sprintf("- [%s] %s: %s%s [[%s]]\n", label, e.Topic, e.Text, eff, e.ID)
}

func section(b *strings.Builder, title string, es []pack.Entry) {
	if len(es) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n\n", title)
	for _, e := range es {
		b.WriteString(entryLine(e))
	}
}

func overviewMD(p pack.Pack) string {
	var b strings.Builder
	b.WriteString(header(p, "Tổng quan"))
	nOver := 0
	for _, e := range p.Policies {
		if len(e.Overrides) > 0 {
			nOver++
		}
	}
	fmt.Fprintf(&b, "\n- Chính sách: %d mục, trong đó %d mục khác thông lệ ngành\n", len(p.Policies), nOver)
	fmt.Fprintf(&b, "- Quy tắc bắt buộc (locked): %d\n", len(p.Rules))
	fmt.Fprintf(&b, "- Bài học: %d\n", len(p.Lessons))
	fmt.Fprintf(&b, "- Còn thiếu theo template: %d mục — xem trang Còn thiếu\n", len(p.Gaps))
	section(&b, "Persona", p.Persona)
	if len(p.DataSummary) > 0 {
		b.WriteString("\n## Data vận hành (con số cụ thể bot tra bằng tool)\n\n")
		for _, d := range p.DataSummary {
			title := d.Topic
			if d.Title != "" {
				title = d.Title
			}
			fmt.Fprintf(&b, "- %s (%s): %d mục, cập nhật %s — %s\n",
				d.Topic, title, d.Count, d.Updated.Format("2006-01-02"), d.Lookup)
		}
	}
	return b.String()
}

func policyMD(p pack.Pack) string {
	var b strings.Builder
	b.WriteString(header(p, "Chính sách"))
	section(&b, "Quy tắc bắt buộc", p.Rules)
	section(&b, "Chính sách theo chủ đề", p.Policies)
	section(&b, "Bài học", p.Lessons)
	if len(p.Rules) == 0 && len(p.Policies) == 0 && len(p.Lessons) == 0 {
		b.WriteString("\nChưa có chính sách nào đang hiệu lực.\n")
	}
	return b.String()
}

func nonStandardMD(p pack.Pack) string {
	var b strings.Builder
	b.WriteString(header(p, "Khác thông lệ ngành"))
	b.WriteString("\nNhững mục nhà xe tự quy định, thay thông lệ chung của ngành:\n\n")
	n := 0
	for _, e := range p.Policies {
		if len(e.Overrides) == 0 {
			continue
		}
		ids := make([]string, len(e.Overrides))
		for i, id := range e.Overrides {
			ids[i] = "[[" + id + "]]"
		}
		fmt.Fprintf(&b, "%s— thay thông lệ %s\n", entryLine(e), strings.Join(ids, " "))
		n++
	}
	if n == 0 {
		b.WriteString("Chưa có mục nào khác thông lệ — mọi chủ đề đang có tri thức đều theo thông lệ ngành (L1).\n")
	}
	return b.String()
}

func gapsMD(p pack.Pack) string {
	var b strings.Builder
	b.WriteString(header(p, "Còn thiếu"))
	if len(p.Gaps) == 0 {
		b.WriteString("\nĐủ mọi mục của template ngành.\n")
		return b.String()
	}
	b.WriteString("\nTheo template ngành (mục bắt buộc trước):\n\n")
	for _, g := range p.Gaps {
		level := "khuyến nghị"
		if g.Level == "required" {
			level = "bắt buộc"
		}
		status := "chưa có"
		if g.Status == "industry_default" {
			status = "đang dùng thông lệ chung, cần nhà xe xác nhận"
		}
		fmt.Fprintf(&b, "- [%s] %s (%s) — %s\n", level, g.Title, g.Topic, status)
	}
	b.WriteString("\nCâu hỏi cho mục còn thiếu: generate_questions, gửi nhà xe qua Zalo hoặc create_form.\n")
	return b.String()
}

func logicMD(p pack.Pack, tmpl kb.Template) string {
	var b strings.Builder
	b.WriteString(header(p, "Logic"))
	if len(tmpl.Capabilities) == 0 {
		b.WriteString("\nTemplate ngành chưa khai báo capability nào.\n")
	} else {
		b.WriteString("\nCapability theo template ngành:\n\n")
		for _, c := range tmpl.Capabilities {
			level := "khuyến nghị"
			if c.Level == "required" {
				level = "bắt buộc"
			}
			modes := ""
			if len(c.Modes) > 0 {
				modes = " (" + strings.Join(c.Modes, " · ") + ")"
			}
			fmt.Fprintf(&b, "- [%s] %s — %s%s\n", level, c.ID, c.Title, modes)
		}
	}
	b.WriteString("\nHồ sơ logic của nhà xe: chưa có.\n")
	return b.String()
}
