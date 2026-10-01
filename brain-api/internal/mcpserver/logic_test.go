package mcpserver

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestLogicTools: get_operator_logic / get_logic_spec / find_similar_operators / compare_logic qua MCP.
func TestLogicTools(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	topic := "fare" + sfx // topic riêng để không vướng item của test khác
	ins := func(op string, feats string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO logic_specs (operator_id, capability, features, rules_text, source_item_ids, created_by)
			VALUES ($1, 'fare', $2::jsonb, $3, '{}', 't')`, op, feats, []string{"Phụ thu Tết 20% theo ngày đi"}); err != nil {
			t.Fatal(err)
		}
	}
	ins(f.opA, `[{"id":"fare.holiday_surcharge","params":{"tet":0.20}},{"id":"fare.child_policy"}]`)
	ins(f.opB, `[{"id":"fare.holiday_surcharge","params":{"tet":0.25}},{"id":"fare.weekend_surcharge"}]`)
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM logic_specs WHERE operator_id IN ($1, $2)`, f.opA, f.opB)
	})

	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// get_operator_logic: fare có spec, các capability khác trống; missing gồm các capability chưa hồ sơ.
	_, ov, _ := call(t, s, "get_operator_logic", map[string]any{})
	caps := ov["capabilities"].([]any)
	fare := caps[0].(map[string]any)
	if fare["capability"] != "fare" || fare["spec"].(map[string]any)["features"].(float64) != 2 {
		t.Fatalf("fare = %+v", fare)
	}
	if len(ov["missing"].([]any)) == 0 {
		t.Fatalf("missing rỗng: %+v", ov)
	}

	// get_logic_spec: đọc spec fare của nhà xe A.
	_, sp, _ := call(t, s, "get_logic_spec", map[string]any{"capability": "fare"})
	if sp["capability"] != "fare" || len(sp["features"].([]any)) != 2 {
		t.Fatalf("spec = %+v", sp)
	}
	// Chưa có spec booking → lỗi hướng dẫn chạy job.
	if isErr, _, _ := call(t, s, "get_logic_spec", map[string]any{"capability": "booking"}); !isErr {
		t.Fatal("chưa có spec phải lỗi kèm hướng dẫn")
	}
	_ = topic

	// find_similar_operators: ứng viên top là nhà xe B với giải thích trùng/thiếu/khác + tham số lệch.
	_, sm, _ := call(t, s, "find_similar_operators", map[string]any{"capability": "fare"})
	cands := sm["candidates"].([]any)
	if len(cands) == 0 {
		t.Fatalf("candidates = %+v", sm)
	}
	top := cands[0].(map[string]any)
	if top["operator"] != f.opB {
		t.Fatalf("top = %+v", top)
	}
	common := top["feature_overlap"].(float64)
	if common <= 0 || common >= 1 {
		t.Fatalf("overlap = %v", common)
	}
	diffs := top["param_diffs"].([]any)
	if len(diffs) != 1 || diffs[0].(map[string]any)["key"] != "tet" {
		t.Fatalf("param_diffs = %+v", diffs)
	}

	// compare_logic: so trực tiếp A với B.
	_, cmp, _ := call(t, s, "compare_logic", map[string]any{"with": f.opB, "capability": "fare"})
	if cmp["operator"] != f.opB || len(cmp["common"].([]any)) != 1 {
		t.Fatalf("compare = %+v", cmp)
	}
	// So với chính mình → lỗi.
	if isErr, _, _ := call(t, s, "compare_logic", map[string]any{"with": f.opA, "capability": "fare"}); !isErr {
		t.Fatal("so với chính mình phải lỗi")
	}
}

// TestPlanDecisionProposeTools: plan_logic_implementation, record_decision, propose_logic_profile.
func TestPlanDecisionProposeTools(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ins := func(op, feats string) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO logic_specs (operator_id, capability, features, rules_text, source_item_ids, created_by)
			VALUES ($1, 'fare', $2::jsonb, $3, '{}', 't')`, op, feats,
			[]string{"Phụ thu Tết 20% theo ngày đi"}); err != nil {
			t.Fatal(err)
		}
	}
	ins(f.opA, `[{"id":"fare.by_route_vehicle"},{"id":"fare.holiday_surcharge","params":{"tet":0.20}}]`)
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM logic_specs WHERE operator_id = $1`, f.opA) })
	if _, err := f.pool.Exec(ctx, `INSERT INTO logic_modules (id, version, layer, operator_id, capability,
		summary, entrypoint, features, params_schema, hooks, required_tests, repo, path, commit)
		VALUES ('fare.standard', 1, 'L1', NULL, 'fare', 'Giá chuẩn', 'calculator.py:calculate',
		$1::text[], '{}', '[]', '{}', 'biva-integrations', 'modules/fare/standard', 'abc')`,
		[]string{"fare.by_route_vehicle", "fare.holiday_surcharge"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM logic_modules WHERE id = 'fare.standard'`)
	})

	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// plan: module phủ đủ → config, không cần ADR.
	isErr, plan, _ := call(t, s, "plan_logic_implementation", map[string]any{"capability": "fare"})
	if isErr {
		t.Fatal("plan lỗi")
	}
	if plan["mode"] != "config" || plan["module"] != "fare.standard@1" || plan["needs_adr"] != false {
		t.Fatalf("plan = %+v", plan)
	}
	if _, ok := plan["params_draft"].(map[string]any)["fare.holiday_surcharge"]; !ok {
		t.Fatalf("params_draft = %+v", plan["params_draft"])
	}

	// record_decision → id adr_*.
	isErr, dec, _ := call(t, s, "record_decision", map[string]any{"capability": "fare",
		"title": "test ADR", "context": "c", "decision": "d"})
	if isErr {
		t.Fatal("record_decision lỗi")
	}
	adr := dec["id"].(string)
	if len(adr) < 8 || adr[:4] != "adr_" {
		t.Fatalf("adr = %v", adr)
	}

	// propose_logic_profile → operation_id của job logic.propose.
	profile := "operator: " + f.opA + "\ncapabilities:\n  fare:\n    mode: config\n    module: fare.standard@1\n"
	isErr, prop, _ := call(t, s, "propose_logic_profile", map[string]any{"profile_yaml": profile})
	if isErr {
		t.Fatal("propose lỗi")
	}
	opID := prop["operation_id"].(string)
	var kind, status string
	if err := f.pool.QueryRow(ctx, `SELECT kind, status FROM operations WHERE id = $1`, opID).
		Scan(&kind, &status); err != nil || kind != "logic.propose" || status != "queued" {
		t.Fatalf("job = %s %s %v", kind, status, err)
	}
}
