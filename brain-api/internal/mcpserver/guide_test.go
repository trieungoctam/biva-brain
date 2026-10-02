package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/pages"
)

func TestGuideResourcesPromptsAndValidate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !strings.Contains(s.InitializeResult().Instructions, "build_bot") {
		t.Fatalf("instructions = %q", s.InitializeResult().Instructions)
	}

	res, err := s.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	uris := map[string]bool{}
	for _, r := range res.Resources {
		uris[r.URI] = true
	}
	profile := "biva://operator/" + f.opA + "/profile"
	want := []string{"biva://guides/citation", "biva://guides/workflow", "biva://industry/template", profile}
	for _, slug := range pages.Slugs() {
		want = append(want, "biva://operator/"+f.opA+"/pages/"+slug+".md")
	}
	for _, u := range want {
		if !uris[u] {
			t.Fatalf("thiếu resource %s: %v", u, uris)
		}
	}
	read := func(uri string) string {
		r, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatal(err)
		}
		return r.Contents[0].Text
	}
	if !strings.Contains(read("biva://guides/citation"), "MISSING_LOCKED") ||
		!strings.Contains(read("biva://industry/template"), "| fare | Giá vé | required |") ||
		!strings.Contains(read(profile), "# Nhà xe "+f.opA) {
		t.Fatal("nội dung resource sai")
	}

	// Trang pages/<slug>.md: dựng tại chỗ khi chưa có, lưu đủ 5 trang vào operator_pages.
	tongquan := read("biva://operator/" + f.opA + "/pages/tong-quan.md")
	if !strings.Contains(tongquan, "# "+f.opA+" — Tổng quan") {
		t.Fatalf("tong-quan = %s", tongquan)
	}
	var got int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM operator_pages WHERE operator_id = $1`, f.opA).Scan(&got); err != nil || got != 5 {
		t.Fatalf("operator_pages = %d (err=%v), muốn 5", got, err)
	}

	pr, err := s.ListPrompts(ctx, nil)
	if err != nil || len(pr.Prompts) != 6 {
		t.Fatalf("prompts = %v %v", pr, err)
	}
	gp, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "process_update",
		Arguments: map[string]string{"content": "Từ 1/11 giá lên 350k", "source": "zalo"}})
	if err != nil {
		t.Fatal(err)
	}
	text := gp.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "submit_knowledge") || !strings.Contains(text, "<noi_dung_nha_xe>\nTừ 1/11 giá lên 350k") {
		t.Fatalf("process_update = %s", text)
	}
	if gp, err = s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "build_bot"}); err != nil ||
		!strings.Contains(gp.Messages[0].Content.(*mcp.TextContent).Text, `channel="zalo"`) {
		t.Fatalf("build_bot: %v", err)
	}

	// validate_artifact: chưa có → lỗi; có lỗi ghi cứng giá → invalid, có dòng.
	if isErr, _, _ := call(t, s, "validate_artifact", map[string]any{"kind": "faq"}); !isErr {
		t.Fatal("artifact chưa có phải lỗi")
	}
	var pets string
	f.pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
		VALUES (2, $1, 'policy', 'pets', 'Không nhận chó mèo', 'active') RETURNING id::text`, f.opA).Scan(&pets)
	call(t, s, "save_artifact", map[string]any{"kind": "faq",
		"content": "### Giá?\nGiá vé là 320.000đ ạ. [[" + pets + "]]"})
	_, rep, _ := call(t, s, "validate_artifact", map[string]any{"kind": "faq"})
	errs := rep["errors"].([]any)
	if rep["valid"] != false || len(errs) != 1 || errs[0].(map[string]any)["code"] != "HARDCODED_DATA" ||
		errs[0].(map[string]any)["line"].(float64) != 2 {
		t.Fatalf("validate = %v", rep)
	}
	_, lst, _ := call(t, s, "list_artifacts", map[string]any{})
	if lst["artifacts"].([]any)[0].(map[string]any)["status"] != "invalid" {
		t.Fatalf("status phải invalid: %v", lst)
	}

	// Nhà xe bỏ chính sách → faq stale ngay khi commit; list_stale chỉ đúng dòng.
	if _, st, _ := call(t, s, "list_stale", map[string]any{}); len(st["artifacts"].([]any)) != 0 {
		t.Fatalf("chưa có gì stale: %v", st)
	}
	f.pool.Exec(ctx, `UPDATE items SET status = 'retracted' WHERE id = $1`, pets)
	_, st, _ := call(t, s, "list_stale", map[string]any{})
	arts := st["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("list_stale = %v", st)
	}
	reason := arts[0].(map[string]any)["reasons"].([]any)[0].(map[string]any)
	if reason["line"].(float64) != 2 || reason["reason"] != "retracted" || reason["instruction"] == "" {
		t.Fatalf("reason = %v", reason)
	}
	if gp, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "refresh_bot"}); err != nil ||
		!strings.Contains(gp.Messages[0].Content.(*mcp.TextContent).Text, "list_stale") {
		t.Fatalf("refresh_bot: %v", err)
	}
}

// 4 resource còn lại của mục 6: lessons, modules, L0, artifact theo bot.
func TestCatalogResources(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`,
		f.opA+":zalo", f.opA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content,
		content_hash, status, author) VALUES ($1, $2, 'faq', 1, 'Nội dung FAQ [[x]]', 'h', 'valid', 't')`,
		f.opA+":zalo", f.opA); err != nil {
		t.Fatal(err)
	}

	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	read := func(uri string) string {
		r, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		return r.Contents[0].Text
	}
	if md := read("biva://platform/rules"); !strings.Contains(md, "LOCKED") {
		t.Fatalf("L0 thiếu rule locked:\n%s", md[:200])
	}
	if md := read("biva://industry/lessons"); !strings.Contains(md, "add_lesson") {
		t.Fatalf("lessons thiếu hướng dẫn:\n%s", md[:200])
	}
	if md := read("biva://logic/modules"); !strings.Contains(md, "get_logic_module") {
		t.Fatalf("modules thiếu hướng dẫn:\n%s", md[:200])
	}
	md := read("biva://operator/" + f.opA + "/bots/" + f.opA + ":zalo/artifacts/faq")
	if !strings.Contains(md, "faq v1 (valid)") || !strings.Contains(md, "Nội dung FAQ") {
		t.Fatalf("artifact = %s", md[:200])
	}
	// URI nhà xe khác → từ chối.
	if _, err := s.ReadResource(ctx, &mcp.ReadResourceParams{
		URI: "biva://operator/" + f.opB + "/bots/x/artifacts/faq"}); err == nil {
		t.Fatal("đọc artifact nhà xe khác phải lỗi")
	}
}
