package mcpserver

import (
	"context"
	"strings"
	"testing"
)

func TestBuildTools(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var pets string
	if err := f.pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
		VALUES (2, $1, 'policy', 'pets', 'pets.cho_meo', 'Không nhận chó mèo', 'active') RETURNING id::text`, f.opA).Scan(&pets); err != nil {
		t.Fatal(err)
	}

	// get_bot_spec: artifact bắt buộc chưa có; pets đã có tri thức nhà xe, fare còn thiếu (kèm câu hỏi).
	isErr, spec, text := call(t, s, "get_bot_spec", map[string]any{})
	if isErr {
		t.Fatal(text)
	}
	arts := spec["artifacts"].([]any)
	secs := map[string]map[string]any{}
	for _, x := range spec["sections"].([]any) {
		m := x.(map[string]any)
		secs[m["topic"].(string)] = m
	}
	if spec["bot_id"] != f.opA+":zalo" || len(arts) != 3 || arts[0].(map[string]any)["status"] != "missing" ||
		secs["pets"]["coverage"] != "operator" || secs["fare"]["coverage"] == "operator" ||
		len(secs["fare"]["questions"].([]any)) != 1 || spec["citation_guide"] == nil || spec["locked_rules"] == nil {
		t.Fatalf("spec = %v", spec)
	}

	// get_knowledge_pack: có item nhà xe; lần 2 trúng cache.
	_, p1, _ := call(t, s, "get_knowledge_pack", map[string]any{"purpose": "faq"})
	found := false
	for _, x := range p1["policies"].([]any) {
		if x.(map[string]any)["id"] == pets {
			found = true
		}
	}
	if !found || p1["cached"] != false || p1["purpose"] != "faq" {
		t.Fatalf("pack = %v", p1)
	}
	if _, p2, _ := call(t, s, "get_knowledge_pack", map[string]any{"purpose": "faq"}); p2["cached"] != true {
		t.Fatal("lần 2 phải trúng cache")
	}
	if isErr, _, text := call(t, s, "get_knowledge_pack", map[string]any{"purpose": "x"}); !isErr || !strings.Contains(text, "purpose") {
		t.Fatalf("purpose sai: %s", text)
	}

	// save_artifact → get_artifact → list_artifacts.
	isErr, saved, text := call(t, s, "save_artifact", map[string]any{"kind": "faq",
		"content": "### Có chở chó mèo không?\nNhà xe không nhận chó mèo. [[" + pets + "]]"})
	if isErr || saved["version"].(float64) != 1 || !strings.Contains(strings.Join(toStrings(saved["next_actions"]), " "), "system_prompt") {
		t.Fatalf("save = %v %s", saved, text)
	}
	if isErr, _, text := call(t, s, "save_artifact", map[string]any{"kind": "faq", "content": "x [[it_1]]"}); !isErr ||
		!strings.Contains(text, "dòng 1") {
		t.Fatalf("trích dẫn sai: %s", text)
	}
	_, got, _ := call(t, s, "get_artifact", map[string]any{"kind": "faq"})
	if got["version"].(float64) != 1 || len(got["citations"].([]any)) != 1 || got["author"] != "user:"+f.builder {
		t.Fatalf("get_artifact = %v", got)
	}
	_, lst, _ := call(t, s, "list_artifacts", map[string]any{})
	if len(lst["artifacts"].([]any)) != 1 || strings.Join(toStrings(lst["missing_required"]), ",") != "system_prompt" {
		t.Fatalf("list = %v", lst)
	}
	// Nhà xe B không thấy artifact của A.
	sb, err := connect(t, f.url+"/mcp/operator/"+f.opB+"/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if isErr, _, _ := call(t, sb, "get_artifact", map[string]any{"kind": "faq"}); !isErr {
		t.Fatal("nhà xe B đọc được artifact của A")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
