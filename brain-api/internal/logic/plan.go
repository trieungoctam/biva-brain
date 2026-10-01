// plan.go — S2.5.5 plan_logic_implementation: kế hoạch triển khai logic cho nhà xe theo capability,
// dựng từ logic spec + danh mục module L1 + nhà xe tương tự (docs/logic-knowledge.md §7).
package logic

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ModuleInfo struct {
	ID       string   `json:"id"`
	Version  int      `json:"version"`
	Summary  string   `json:"summary"`
	Features []string `json:"features"`
	Hooks    []string `json:"hooks"`
}

type ReuseFrom struct {
	Operator string   `json:"operator"`
	Score    float64  `json:"score"`
	Covers   []string `json:"covers"` // feature của phần thiếu mà nhà xe đó đã có
}

type ImplementationPlan struct {
	Capability  string                    `json:"capability"`
	Mode        string                    `json:"mode"` // config | hook | custom
	Module      string                    `json:"module,omitempty"`
	Covered     []string                  `json:"covered"`          // feature đã có trong module đề xuất
	Missing     []string                  `json:"missing_features"` // feature spec chưa module nào phủ → viết mới
	ReuseFrom   []ReuseFrom               `json:"reuse_from"`
	NeedsADR    bool                      `json:"needs_adr"`
	ParamsDraft map[string]map[string]any `json:"params_draft"` // tên tham số theo feature → giá trị từ spec
	Notes       []string                  `json:"notes"`
}

// Plan dựng kế hoạch: chọn module L1 phủ nhiều feature của spec nhất; phần thiếu nhìn sang nhà xe
// tương tự đã có; chọn bậc thấp nhất đủ dùng (config → hook → custom + ADR).
func BuildPlan(ctx context.Context, db *pgxpool.Pool, operator, capability string) (ImplementationPlan, error) {
	spec, err := GetSpec(ctx, db, operator, capability)
	if err != nil {
		return ImplementationPlan{}, err
	}
	rows, err := db.Query(ctx, `
		SELECT DISTINCT ON (id) id, version, summary, features,
		       coalesce((SELECT jsonb_agg(h->>'name') FROM jsonb_array_elements(hooks) h), '[]'::jsonb)::text
		FROM logic_modules
		WHERE capability = $1 AND status = 'active' AND operator_id IS NULL
		ORDER BY id, version DESC`, capability)
	if err != nil {
		return ImplementationPlan{}, err
	}
	defer rows.Close()
	var modules []ModuleInfo
	for rows.Next() {
		var m ModuleInfo
		var hooks string
		if err := rows.Scan(&m.ID, &m.Version, &m.Summary, &m.Features, &hooks); err != nil {
			return ImplementationPlan{}, err
		}
		_ = json.Unmarshal([]byte(hooks), &m.Hooks)
		modules = append(modules, m)
	}
	if err := rows.Err(); err != nil {
		return ImplementationPlan{}, err
	}

	want := map[string]map[string]any{}
	for _, f := range spec.Features {
		want[f.ID] = f.Params
	}

	p := ImplementationPlan{Capability: capability, Mode: "custom", NeedsADR: true,
		Covered: []string{}, Missing: []string{}, ReuseFrom: []ReuseFrom{},
		ParamsDraft: map[string]map[string]any{}}
	var best *ModuleInfo
	bestCover := -1
	for i := range modules {
		n := 0
		for _, f := range modules[i].Features {
			if _, ok := want[f]; ok {
				n++
			}
		}
		if n > bestCover {
			bestCover = n
			best = &modules[i]
		}
	}
	coveredSet := map[string]bool{}
	if best != nil {
		p.Module = fmt.Sprintf("%s@%d", best.ID, best.Version)
		for _, f := range best.Features {
			if _, ok := want[f]; ok {
				coveredSet[f] = true
				p.Covered = append(p.Covered, f)
			}
		}
	}
	for id, params := range want {
		if coveredSet[id] {
			if len(params) > 0 {
				p.ParamsDraft[id] = params
			}
			continue
		}
		p.Missing = append(p.Missing, id)
		if len(params) > 0 {
			p.ParamsDraft[id] = params
		}
	}

	// Phần thiếu: nhà xe tương tự đã giải quyết được gì (Extra của họ trùng Missing của mình).
	if len(p.Missing) > 0 {
		cands, err := Similar(ctx, db, operator, capability, 3)
		if err == nil {
			for _, c := range cands {
				var covers []string
				for _, e := range c.Extra {
					if _, ok := want[e]; ok {
						covers = append(covers, e)
					}
				}
				if len(covers) > 0 {
					p.ReuseFrom = append(p.ReuseFrom, ReuseFrom{Operator: c.Operator, Score: c.Score, Covers: covers})
				}
			}
		}
	}

	// Bậc thấp nhất đủ dùng: đủ → config; thiếu nhưng module có hook / có nhà xe tái dùng → hook; còn lại custom.
	switch {
	case len(p.Missing) == 0 && p.Module != "":
		p.Mode, p.NeedsADR = "config", false
	case p.Module != "" || len(p.ReuseFrom) > 0:
		p.Mode, p.NeedsADR = "hook", false
	default:
		p.Mode, p.NeedsADR = "custom", true
	}
	p.Notes = append(p.Notes,
		"tham số trong params_draft lấy từ spec — khi viết profile.yaml phải ghi source là id item tri thức",
		"chọn bậc thấp nhất đủ dùng: config → hook → custom (custom bắt buộc ADR)")
	if p.NeedsADR {
		p.Notes = append(p.Notes, "record_decision trước khi propose_logic_profile với mode=custom")
	}
	return p, nil
}
