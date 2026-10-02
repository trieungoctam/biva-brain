package mcpserver

import (
	"context"
	"strings"
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

// S5.1.2 + S5.1.3: impact_of_change và run_regression_all.
func TestImpactAndRegression(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO logic_families (id, capability, name, centroid_features,
		members) VALUES ('fare.famx', 'fare', 'Họ fare', ARRAY['fare.holiday_surcharge'], ARRAY[$1])`,
		f.opA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM logic_families`) })

	s, err := connect(t, f.url+"/mcp/platform/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// impact theo feature: spec chứa feature + family chứa feature.
	if _, err := f.pool.Exec(ctx, `INSERT INTO logic_specs (operator_id, capability, features,
		rules_text, source_item_ids, created_by) VALUES ($1, 'fare',
		'[{"id":"fare.holiday_surcharge"}]'::jsonb, ARRAY['Tết'], '{}', 't')`, f.opA); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM logic_specs WHERE operator_id = $1`, f.opA) })

	isErr, imp, _ := call(t, s, "impact_of_change", map[string]any{"feature_id": "fare.holiday_surcharge"})
	if isErr {
		t.Fatal("impact lỗi")
	}
	kinds := map[string]int{}
	for _, i := range imp["impacted"].([]any) {
		kinds[i.(map[string]any)["kind"].(string)]++
	}
	if kinds["spec"] < 1 || kinds["family"] < 1 {
		t.Fatalf("impacted = %v", imp["impacted"])
	}
	// Truyền 2 tham số → lỗi.
	if isErr, _, _ = call(t, s, "impact_of_change", map[string]any{
		"feature_id": "fare.x", "module_id": "fare.standard"}); !isErr {
		t.Fatal("phải truyền đúng một tham số")
	}

	// run_regression_all: nhà xe có snapshot được enqueue bot.tests.
	if _, err := f.pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`,
		f.opA+":zalo", f.opA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO snapshots (bot_id, operator_id, version,
		artifact_versions, definition, created_by) VALUES ($1, $2, 1, '{}', '{}', 't')`,
		f.opA+":zalo", f.opA); err != nil {
		t.Fatal(err)
	}
	isErr, reg, _ := call(t, s, "run_regression_all", map[string]any{})
	if isErr {
		t.Fatal("regression lỗi")
	}
	ops := reg["operations"].([]any)
	if len(ops) < 1 {
		t.Fatalf("operations = %v", ops)
	}
	firstOp := ops[0].(map[string]any)
	if firstOp["operator"] != f.opA || firstOp["operation_id"] == "" {
		t.Fatalf("op = %v", firstOp)
	}
	var kind string
	if err := f.pool.QueryRow(ctx, `SELECT kind FROM operations WHERE id = $1::uuid`,
		firstOp["operation_id"].(string)).Scan(&kind); err != nil || kind != "bot.tests" {
		t.Fatalf("job = %s %v", kind, err)
	}
}

// approve_publish: preview danh sách chờ → duyệt → published.
func TestApprovePublish(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	bot := f.opA + ":zalo"
	if _, err := f.pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`,
		bot, f.opA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO snapshots (bot_id, operator_id, version,
		artifact_versions, definition, created_by) VALUES ($1, $2, 1, '{}', '{}', 't')`, bot, f.opA); err != nil {
		t.Fatal(err)
	}
	rel := func() string {
		var id string
		if err := f.pool.QueryRow(ctx, `INSERT INTO releases (operator_id, bot_channel, snapshot_id,
			snapshot_ver, stage, status, requested_by)
			SELECT $1, 'zalo', s.id, s.version, 'production', 'requested', 'ai:t' FROM snapshots s
			WHERE s.bot_id = $2 ORDER BY s.version DESC LIMIT 1 RETURNING id::text`, f.opA, bot).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM releases WHERE operator_id = $1`, f.opA) })

	s, err := connect(t, f.url+"/mcp/platform/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	relID := rel() // bản production requested duy nhất
	// Bước 1 (liệt kê, không phát token): thấy đúng bản.
	isErr, out, raw := call(t, s, "approve_publish", map[string]any{})
	if isErr {
		t.Fatalf("liệt kê lỗi: %v", raw)
	}
	if len(out["pending"].([]any)) != 1 {
		t.Fatalf("pending = %+v", out["pending"])
	}
	// Bước 2: preview đúng release_id → token gắn đúng bản đó.
	isErr, out, raw = call(t, s, "approve_publish", map[string]any{"release_id": relID})
	if isErr {
		t.Fatalf("preview lỗi: %v", raw)
	}
	na := out["next_actions"].([]any)[0].(string)
	i := indexStr(na, "confirm_token=")
	if i < 0 {
		t.Fatalf("không có token trong %q", na)
	}
	token := na[i+len("confirm_token="):]
	if j := indexStr(token, " (hết hạn"); j > 0 {
		token = token[:j]
	}
	// Duyệt đúng id → published.
	isErr, out, _ = call(t, s, "approve_publish", map[string]any{
		"release_id": relID, "confirm_token": token})
	if isErr || out["approved_release"] != relID {
		t.Fatalf("approve = %+v", out)
	}
	var status string
	f.pool.QueryRow(ctx, `SELECT status FROM releases WHERE id = $1::uuid`, relID).Scan(&status)
	if status != "published" {
		t.Fatalf("status = %s", status)
	}
	// Token dùng lại → bị từ chối (một lần).
	if isErr, _, _ = call(t, s, "approve_publish", map[string]any{
		"release_id": relID, "confirm_token": token}); !isErr {
		t.Fatal("token dùng lại phải bị từ chối")
	}
	// Id không còn requested → lỗi rõ.
	if isErr, _, _ = call(t, s, "approve_publish", map[string]any{
		"release_id": relID, "confirm_token": "ct_khongtontai"}); !isErr {
		t.Fatal("id đã published phải lỗi")
	}
}

// Token approve_publish gắn đúng release_id — duyệt release KHÁC bằng token của bản này phải bị chặn.
func TestApprovePublishTokenBindsRelease(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	bot := f.opA + ":zalo"
	if _, err := f.pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`,
		bot, f.opA); err != nil {
		t.Fatal(err)
	}
	mk := func() string {
		var id string
		if err := f.pool.QueryRow(ctx, `INSERT INTO snapshots (bot_id, operator_id, version,
			artifact_versions, definition, created_by) VALUES ($1, $2, (SELECT COALESCE(max(version),0)+1
			FROM snapshots WHERE bot_id=$1), '{}', '{}', 't') RETURNING id::text`, bot, f.opA).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var rel string
		if err := f.pool.QueryRow(ctx, `INSERT INTO releases (operator_id, bot_channel, snapshot_id,
			snapshot_ver, stage, status, requested_by)
			SELECT $1, 'zalo', s.id, s.version, 'production', 'requested', 'ai:t' FROM snapshots s
			WHERE s.id=$2::uuid RETURNING id::text`, f.opA, id).Scan(&rel); err != nil {
			t.Fatal(err)
		}
		return rel
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM releases WHERE operator_id = $1`, f.opA) })
	s, err := connect(t, f.url+"/mcp/platform/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r1, r2 := mk(), mk()
	// Bước 1 (liệt kê): không có token.
	isErr, out, _ := call(t, s, "approve_publish", map[string]any{})
	if isErr || len(out["pending"].([]any)) != 2 || strings.HasPrefix(out["next_actions"].([]any)[0].(string), "duyệt: gọi lại với release_id + confirm_token") {
		t.Fatalf("liệt kê = %+v", out)
	}
	// Bước 2: preview đúng r1 → token.
	isErr, out, _ = call(t, s, "approve_publish", map[string]any{"release_id": r1})
	if isErr || len(out["pending"].([]any)) != 1 {
		t.Fatalf("preview = %+v", out)
	}
	na := out["next_actions"].([]any)[0].(string)
	i := strings.Index(na, "confirm_token=")
	token := na[i+len("confirm_token="):]
	if j := strings.Index(token, " (hết hạn"); j > 0 {
		token = token[:j]
	}
	// Dùng token của r1 để duyệt r2 → PHẢI BỊ CHỐI (subject không khớp).
	if isErr, _, _ = call(t, s, "approve_publish", map[string]any{
		"release_id": r2, "confirm_token": token}); !isErr {
		t.Fatal("token của r1 không được duyệt r2")
	}
	// Duyệt đúng r1 → ok.
	isErr, out, _ = call(t, s, "approve_publish", map[string]any{
		"release_id": r1, "confirm_token": token})
	if isErr || out["approved_release"] != r1 {
		t.Fatalf("approve r1 = %+v", out)
	}
}

// propose_l1_change trên key L1 ĐANG CÓ: review phải gắn target_item_id và apply
// supersede bản cũ (trước đây apply đụng unique items_platform_key_active → không sửa được).
func TestProposeL1ChangeModifiesExistingKey(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := connect(t, f.url+"/mcp/platform/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(text string) (reviewID, itemID string) {
		isErr, prev, _ := call(t, s, "propose_l1_change", map[string]any{
			"topic": "luggage", "key": "hanh_ly.mien_phi", "text": text})
		if isErr {
			t.Fatalf("preview lỗi: %+v", prev)
		}
		na := prev["next_actions"].([]any)[1].(string)
		token := na[len("đồng ý thì gọi lại với confirm_token="):]
		if i := indexStr(token, " (hết hạn"); i > 0 {
			token = token[:i]
		}
		isErr, done, _ := call(t, s, "propose_l1_change", map[string]any{
			"topic": "luggage", "key": "hanh_ly.mien_phi", "text": text, "confirm_token": token})
		if isErr {
			t.Fatalf("confirm lỗi: %+v", done)
		}
		return done["review_id"].(string), done["item_id"].(string)
	}

	// Lần 1: thêm mới — apply → active.
	r1, it1 := mk("Hành lý miễn phí 20kg.")
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, []string{it1})
		f.pool.Exec(ctx, `DELETE FROM review_items WHERE id = $1::uuid`, r1)
	})
	// apply_review là tool endpoint nhà xe; L1 apply trực tiếp qua hàm SQL (chính là
	// thứ migration 000025 thay đổi).
	if _, err := f.pool.Exec(ctx, `SELECT apply_review($1::uuid, 'user:lead')`, r1); err != nil {
		t.Fatalf("apply lần 1: %v", err)
	}

	// Lần 2: SỬA cùng key — trước đây lỗi unique; giờ target = bản cũ, apply supersede.
	r2, it2 := mk("Hành lý miễn phí 25kg.")
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, []string{it2})
		f.pool.Exec(ctx, `DELETE FROM review_items WHERE id = $1::uuid`, r2)
	})
	var target any
	if err := f.pool.QueryRow(ctx, `SELECT target_item_id::text FROM review_items WHERE id = $2::uuid`, r2).
		Scan(&target); err == nil && target != nil {
		if target.(string) != it1 {
			t.Fatalf("target = %v, muốn item bản đầu %s", target, it1)
		}
	}
	if _, err := f.pool.Exec(ctx, `SELECT apply_review($1::uuid, 'user:lead')`, r2); err != nil {
		t.Fatalf("apply lần 2 (sửa): %v", err)
	}
	var st1, st2 string
	f.pool.QueryRow(ctx, `SELECT status FROM items WHERE id = $1::uuid`, it1).Scan(&st1)
	f.pool.QueryRow(ctx, `SELECT status FROM items WHERE id = $1::uuid`, it2).Scan(&st2)
	if st1 != "superseded" || st2 != "active" {
		t.Fatalf("sau sửa: bản cũ=%s bản mới=%s, muốn superseded/active", st1, st2)
	}
}
