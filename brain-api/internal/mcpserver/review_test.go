package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) (bool, map[string]any, string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return res.IsError, out, text
}

func TestIngestTool(t *testing.T) {
	f := setup(t)
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	isErr, out, text := call(t, s, "ingest", map[string]any{"content": "Giá vé SG-ĐL giường nằm 300k"})
	if isErr {
		t.Fatal(text)
	}
	opID, _ := out["operation_id"].(string)
	var kind, submitted, content string
	f.pool.QueryRow(context.Background(), `SELECT kind, payload->>'submitted_by', payload->>'content' FROM operations WHERE id = $1`, opID).
		Scan(&kind, &submitted, &content)
	if kind != "ingest" || submitted != "user:"+f.builder || content != "Giá vé SG-ĐL giường nằm 300k" {
		t.Fatalf("job = %s / %s / %q", kind, submitted, content)
	}
	// Gửi lại đúng nội dung → cùng job.
	if _, again, _ := call(t, s, "ingest", map[string]any{"content": "  Giá vé SG-ĐL giường nằm 300k  "}); again["operation_id"] != opID || again["duplicate"] != true {
		t.Fatalf("gửi lại: %v", again)
	}
	if isErr, _, _ := call(t, s, "ingest", map[string]any{"content": "x", "source": "fax"}); !isErr {
		t.Fatal("source lạ lẽ ra bị từ chối")
	}
}

func TestProposeAndApplyWithConfirmToken(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Đề xuất: topic ngoài template bị từ chối, lỗi đọc được để AI sửa.
	if isErr, _, text := call(t, s, "propose_item", map[string]any{"action": "upsert", "kind": "data", "topic": "karaoke",
		"key": "x", "text": "Có karaoke trên xe", "reason": "nhà xe nói"}); !isErr || !strings.Contains(text, "template") {
		t.Fatalf("topic lạ: %v %s", isErr, text)
	}
	isErr, out, text := call(t, s, "propose_item", map[string]any{"action": "upsert", "kind": "data", "topic": "fare",
		"key": "fare.Sài Gòn - Đà Lạt.Giường nằm", "text": "Giá giường nằm SG–ĐL 300.000đ", "facts": map[string]string{"gia_ve": "300000"},
		"reason": "builder gọi xác nhận với nhà xe 1/10"})
	if isErr {
		t.Fatal(text)
	}
	rv := out["review"].(map[string]any)
	reviewID := rv["id"].(string)
	if rv["key"] != "fare.sai_gon_da_lat.giuong_nam" || rv["change_kind"] != "NEW" || rv["risk"] != "high" {
		t.Fatalf("review = %v", rv)
	}

	// Hàng đợi + chi tiết.
	_, lst, _ := call(t, s, "list_review_queue", map[string]any{})
	if items := lst["items"].([]any); len(items) != 1 {
		t.Fatalf("queue = %v", lst)
	}
	_, det, _ := call(t, s, "get_review_item", map[string]any{"review_id": reviewID})
	if det["effect"] == "" || det["after"].(map[string]any)["text"] != "Giá giường nằm SG–ĐL 300.000đ" {
		t.Fatalf("detail = %v", det)
	}

	// Bước 1: preview, CHƯA thực thi.
	_, pv, _ := call(t, s, "apply_review", map[string]any{"review_id": reviewID, "decision": "approve"})
	token, _ := pv["confirm_token"].(string)
	if pv["executed"] != false || !strings.HasPrefix(token, "ct_") {
		t.Fatalf("preview = %v", pv)
	}
	var status string
	f.pool.QueryRow(ctx, `SELECT status FROM review_items WHERE id = $1`, reviewID).Scan(&status)
	if status != "open" {
		t.Fatal("preview không được thực thi")
	}

	// Token không dùng được cho quyết định khác (reject) hay review khác.
	if isErr, _, _ := call(t, s, "apply_review", map[string]any{"review_id": reviewID, "decision": "reject",
		"reason": "x", "confirm_token": token}); !isErr {
		t.Fatal("token của approve không được dùng để reject")
	}

	// Bước 2: xác nhận.
	isErr, done, text := call(t, s, "apply_review", map[string]any{"review_id": reviewID, "decision": "approve", "confirm_token": token})
	if isErr || done["executed"] != true || done["outcome"].(map[string]any)["status"] != "applied" {
		t.Fatalf("apply: %v %v %s", isErr, done, text)
	}
	var itemStatus, actor string
	f.pool.QueryRow(ctx, `SELECT i.status FROM review_items r JOIN items i ON i.id = r.item_id WHERE r.id = $1`, reviewID).Scan(&itemStatus)
	f.pool.QueryRow(ctx, `SELECT decided_by FROM review_items WHERE id = $1`, reviewID).Scan(&actor)
	if itemStatus != "active" || actor != "user:"+f.builder {
		t.Fatalf("item=%s decided_by=%s", itemStatus, actor)
	}
	// Token dùng một lần.
	if isErr, _, _ := call(t, s, "apply_review", map[string]any{"review_id": reviewID, "decision": "approve", "confirm_token": token}); !isErr {
		t.Fatal("token không được dùng lại")
	}

	// Đề xuất đổi giá → CHANGE; reject cần reason.
	_, out2, _ := call(t, s, "propose_item", map[string]any{"action": "upsert", "kind": "data", "topic": "fare",
		"key": "fare.sai_gon_da_lat.giuong_nam", "text": "Giá giường nằm 320.000đ", "valid_from": "2026-11-01", "reason": "tin Zalo"})
	rv2 := out2["review"].(map[string]any)
	if rv2["change_kind"] != "CHANGE" {
		t.Fatalf("lần hai = %v", rv2)
	}
	if isErr, _, _ := call(t, s, "apply_review", map[string]any{"review_id": rv2["id"], "decision": "reject"}); !isErr {
		t.Fatal("reject thiếu reason phải lỗi")
	}
	_, pv2, _ := call(t, s, "apply_review", map[string]any{"review_id": rv2["id"], "decision": "reject", "reason": "nhà xe nhắn nhầm"})
	_, done2, _ := call(t, s, "apply_review", map[string]any{"review_id": rv2["id"], "decision": "reject",
		"reason": "nhà xe nhắn nhầm", "confirm_token": pv2["confirm_token"]})
	if done2["outcome"].(map[string]any)["status"] != "rejected" {
		t.Fatalf("reject = %v", done2)
	}
}

func TestReviewIsScopedToOperator(t *testing.T) {
	f := setup(t)
	// Lead tạo đề xuất ở nhà xe B; builder (chỉ có A) không thấy được qua endpoint A.
	sb, err := connect(t, f.url+"/mcp/operator/"+f.opB+"/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	_, out, _ := call(t, sb, "propose_item", map[string]any{"action": "upsert", "kind": "policy", "topic": "pets",
		"key": "pets.dieu_kien", "text": "Thú cưng để trong lồng", "reason": "test"})
	idB := out["review"].(map[string]any)["id"].(string)

	sa, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer sa.Close()
	if isErr, _, _ := call(t, sa, "get_review_item", map[string]any{"review_id": idB}); !isErr {
		t.Fatal("review của nhà xe khác phải bị ẩn")
	}
	if isErr, _, _ := call(t, sa, "apply_review", map[string]any{"review_id": idB, "decision": "approve"}); !isErr {
		t.Fatal("không được duyệt review của nhà xe khác")
	}
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}
