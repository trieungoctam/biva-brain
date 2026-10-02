package mcpserver

import (
	"context"
	"testing"
)

// S5.1.1: platform tools — lead mới vào được; list_operators/promotion/propose_l1_change.
func TestPlatformMgmtTools(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/platform/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	isErr, out, _ := call(t, s, "list_operators", map[string]any{})
	if isErr {
		t.Fatal("list_operators lỗi")
	}
	ops := out["operators"].([]any)
	if len(ops) < 2 {
		t.Fatalf("operators = %v", ops)
	}
	first := ops[0].(map[string]any)
	for _, k := range []string{"id", "items_active", "bots", "open_reviews", "published_releases"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("thiếu %s trong %v", k, first)
		}
	}

	// Promotion: seed 1 review PROMOTE open + 1 family candidate.
	if _, err := f.pool.Exec(ctx, `INSERT INTO logic_families (id, capability, name, members,
		promote_candidate) VALUES ('fare.hookers', 'fare', 'Họ fare', $1::text[], true)`,
		[]string{f.opA, f.opB, "cx"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM logic_families`) })

	isErr, promo, _ := call(t, s, "list_promotion_candidates", map[string]any{})
	if isErr {
		t.Fatal("promotion lỗi")
	}
	cands := promo["candidates"].([]any)
	if len(cands) == 0 {
		t.Fatalf("candidates = %v", cands)
	}
	kinds := map[string]bool{}
	for _, c := range cands {
		kinds[c.(map[string]any)["kind"].(string)] = true
	}
	if !kinds["logic"] {
		t.Fatalf("thiếu ứng viên logic: %v", cands)
	}

	// propose_l1_change: preview → confirm.
	isErr, prev, _ := call(t, s, "propose_l1_change", map[string]any{
		"topic": "fare", "text": "Thông thường phụ thu Tết theo ngày đi, không theo ngày đặt."})
	if isErr || len(prev["next_actions"].([]any)) != 2 {
		t.Fatalf("preview = %+v", prev)
	}
	na := prev["next_actions"].([]any)[1].(string)
	token := na[len("đồng ý thì gọi lại với confirm_token="):]
	if i := indexStr(token, " (hết hạn"); i > 0 {
		token = token[:i]
	}
	if token == "" {
		t.Fatalf("không tách được token từ %q", na)
	}
	isErr, done, _ := call(t, s, "propose_l1_change", map[string]any{
		"topic": "fare", "text": "Thông thường phụ thu Tết theo ngày đi, không theo ngày đặt.",
		"confirm_token": token})
	if isErr {
		t.Fatalf("confirm lỗi: %+v", done)
	}
	if done["review_id"] == "" || done["item_id"] == "" {
		t.Fatalf("done = %+v", done)
	}
	var layer int
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT layer::int, status FROM items WHERE id = $1::uuid`,
		done["item_id"].(string)).Scan(&layer, &status); err != nil {
		t.Fatal(err)
	}
	if layer != 1 || status != "pending" {
		t.Fatalf("L1 pending = %d %s", layer, status)
	}
}

func indexStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
