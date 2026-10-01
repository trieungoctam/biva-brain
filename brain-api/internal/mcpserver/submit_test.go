package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSubmitKnowledgeAndListKnowledge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Lỗi đầu vào trả ngay, theo vị trí, để AI sửa.
	isErr, _, text := call(t, s, "submit_knowledge", map[string]any{"source": "image", "items": []map[string]any{
		{"kind": "data", "topic": "karaoke", "key": "karaoke.co", "text": "Có karaoke trên xe"},
		{"kind": "data", "topic": "fare", "key": "fare", "text": "Giá 300k", "valid_from": "1/11/2026"},
	}})
	if !isErr || !strings.Contains(text, "items[0]") || !strings.Contains(text, "items[1]: key phải nêu chủ thể") ||
		!strings.Contains(text, "valid_from phải dạng YYYY-MM-DD") {
		t.Fatalf("lỗi đầu vào: %v %s", isErr, text)
	}

	items := []map[string]any{{"kind": "data", "topic": "fare", "key": "fare.Sài Gòn - Vũng Tàu.16 chỗ",
		"text": "Xe 16 chỗ SG–VT 180.000đ", "facts": map[string]string{"gia_ve": "180000"}, "valid_from": "2026-11-01"}}
	isErr, out, text := call(t, s, "submit_knowledge", map[string]any{"source": "image", "items": items,
		"source_excerpt": "[ảnh bảng giá] 16 chỗ 180k từ 1/11"})
	if isErr {
		t.Fatal(text)
	}
	opID := out["operation_id"].(string)

	// Payload Go ghi phải khớp hợp đồng mà ai-worker đọc (contracts/schemas/jobs/ingest.schema.json).
	var payload map[string]any
	if err := f.pool.QueryRow(ctx, `SELECT payload FROM operations WHERE id = $1 AND kind = 'ingest'`, opID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	sch, err := c.Compile(filepath.Join("..", "..", "..", "contracts", "schemas", "jobs", "ingest.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(roundTrip(t, payload)); err != nil {
		t.Fatalf("payload sai hợp đồng: %v", err)
	}
	if payload["submitted_by"] != "user:"+f.builder || payload["content"] != "[ảnh bảng giá] 16 chỗ 180k từ 1/11" {
		t.Fatalf("payload = %v", payload)
	}
	// Gửi lại y hệt → cùng job.
	if _, again, _ := call(t, s, "submit_knowledge", map[string]any{"source": "image", "items": items,
		"source_excerpt": "[ảnh bảng giá] 16 chỗ 180k từ 1/11"}); again["operation_id"] != opID {
		t.Fatalf("gửi lại tạo job mới: %v", again)
	}

	// list_knowledge: chỉ item active của đúng nhà xe.
	f.pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status)
		VALUES (2, $1, 'data', 'fare', 'fare.sg_vt.16_cho', 'Xe 16 chỗ 170.000đ', '{"facts": {"gia_ve": "170000"}}', 'active'),
		       (2, $2, 'data', 'fare', 'fare.khac', 'Nhà xe khác', NULL, 'active')`, f.opA, f.opB)
	_, kn, _ := call(t, s, "list_knowledge", map[string]any{"topic": "fare"})
	got := kn["items"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["key"] != "fare.sg_vt.16_cho" ||
		got[0].(map[string]any)["facts"].(map[string]any)["gia_ve"] != "170000" {
		t.Fatalf("list_knowledge = %v", kn)
	}
}
