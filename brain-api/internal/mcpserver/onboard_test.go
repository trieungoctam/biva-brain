package mcpserver

import (
	"strings"
	"testing"
)

func TestOnboardTools(t *testing.T) {
	f := setup(t)
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, cov, _ := call(t, s, "get_coverage", map[string]any{})
	if cov["required_total"].(float64) != 1 || cov["required_covered"].(float64) != 0 {
		t.Fatalf("coverage = %v", cov)
	}
	_, q, _ := call(t, s, "generate_questions", map[string]any{})
	if len(q["questions"].([]any)) == 0 || !strings.Contains(q["message"].(string), "Giá vé?") {
		t.Fatalf("questions = %v", q)
	}
	isErr, form, text := call(t, s, "create_form", map[string]any{})
	if isErr || !strings.Contains(form["url"].(string), "/f/") || !strings.Contains(form["message"].(string), form["url"].(string)) {
		t.Fatalf("create_form = %v %s", form, text)
	}
	_, st, _ := call(t, s, "get_form", map[string]any{"form_id": form["form_id"]})
	if st["status"] != "open" || len(st["questions"].([]any)) == 0 {
		t.Fatalf("get_form = %v", st)
	}
	if isErr, _, _ := call(t, s, "create_form", map[string]any{"questions": []map[string]string{{"topic": "fare", "question": " "}}}); !isErr {
		t.Fatal("câu hỏi trống phải lỗi")
	}
}
