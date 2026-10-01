package logic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestSimilarAndSpec(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	ops := []string{"la" + sfx, "lb" + sfx, "lc" + sfx}
	for _, op := range ops {
		if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'L')`, op); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, op := range ops {
			pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op)
		}
	})
	ins := func(op, feats string, rules []string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO logic_specs (operator_id, capability, features, rules_text, source_item_ids, created_by)
			VALUES ($1, 'fare', $2::jsonb, $3, '{}', 't')`, op, feats, rules); err != nil {
			t.Fatal(err)
		}
	}
	ins(ops[0], `[{"id":"fare.common"},{"id":"fare.holiday","params":{"tet":0.20}}]`,
		[]string{"Phụ thu Tết 20% theo ngày đi", "Cuối tuần cộng 30000"})
	ins(ops[1], `[{"id":"fare.common"},{"id":"fare.holiday","params":{"tet":0.22}},{"id":"fare.weekend"}]`,
		[]string{"Phụ thu Tết 22% theo ngày đi", "Cuối tuần cộng 30000"})
	ins(ops[2], `[{"id":"fare.parcel_only"}]`, []string{"Giá gồm cước hàng"})

	// GetSpec: đọc đúng feature + params.
	spec, err := GetSpec(ctx, pool, ops[0], "fare")
	if err != nil || len(spec.Features) != 2 || spec.Features[1].Params["tet"] != 0.20 {
		t.Fatalf("GetSpec = %+v %v", spec, err)
	}
	if _, err := GetSpec(ctx, pool, ops[0], "booking"); err != ErrNoSpec {
		t.Fatalf("muốn ErrNoSpec, được %v", err)
	}

	cands, err := Similar(ctx, pool, ops[0], "fare", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("cands = %+v", cands)
	}
	top, second := cands[0], cands[1]
	if top.Operator != ops[1] || second.Operator != ops[2] {
		t.Fatalf("thứ tự = %+v %+v", top, second)
	}
	// Giải thích: trùng common + holiday; họ giải quyết thêm weekend (Extra — mình thiếu);
	// param Tết lệch nhẹ (0.20 vs 0.22).
	if len(top.Common) != 2 || len(top.Missing) != 0 || len(top.Extra) != 1 || top.Extra[0] != "fare.weekend" {
		t.Fatalf("top = %+v", top)
	}
	if len(top.ParamDiffs) != 1 || top.ParamDiffs[0].Key != "tet" {
		t.Fatalf("param diff = %+v", top.ParamDiffs)
	}
	if top.Params < 0.7 { // holiday gần nhau + common không params → ≥ 0.7
		t.Fatalf("param closeness = %v", top.Params)
	}
	if top.Score <= second.Score {
		t.Fatalf("score top=%v second=%v", top.Score, second.Score)
	}
	// Nhà xe chỉ có feature lạ: overlap thấp, extra đúng.
	if len(second.Common) != 0 || len(second.Missing) != 2 || len(second.Extra) != 1 ||
		second.Extra[0] != "fare.parcel_only" {
		t.Fatalf("second = %+v", second)
	}

	// Overview: capability có profile → đếm; chưa có → thiếu.
	if _, err := pool.Exec(ctx, `
		INSERT INTO logic_profiles (operator_id, capability, mode, module_id, module_version, commit)
		VALUES ($1, 'fare', 'config', 'fare.standard', 1, 'abc')`, ops[0]); err != nil {
		t.Fatal(err)
	}
	states, err := Overview(ctx, pool, ops[0], []CapabilityInfo{
		{ID: "fare", Title: "Tính giá", Level: "required"},
		{ID: "booking", Title: "Giữ chỗ", Level: "required"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Profile == nil || states[0].Profile.Module != "fare.standard" || states[0].Spec == nil ||
		states[0].Spec.FeatureCount != 2 {
		t.Fatalf("fare = %+v", states[0])
	}
	if states[1].Profile != nil || states[1].Spec != nil {
		t.Fatalf("booking phải trống: %+v", states[1])
	}
}
