// Package logic đọc tri thức logic (M2, S2.5.4): logic spec của nhà xe và tìm nhà xe tương tự theo tầng spec.
//
// find_similar_operators (docs/logic-knowledge.md §5.1):
//
//	sim(A,B) = 0.5 × feature_overlap  (trọng số theo độ hiếm — feature nhà xe nào cũng có gần như không đóng góp)
//	         + 0.3 × rule_text_similarity (cosine embedding; không có vector thì bigram Jaccard trên textnorm)
//	         + 0.2 × param_closeness (tham số cùng feature: bằng nhau 1, số gần nhau theo tỉ lệ, khác 0)
//
// Mọi kết quả kèm giải thích: feature trùng / thiếu (mình có, họ không) / khác (cả hai có nhưng tham số lệch).
package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

// Spec: logic spec của một nhà xe theo một capability (bảng logic_specs).
type Spec struct {
	Operator       string           `json:"operator"`
	Capability     string           `json:"capability"`
	Features       []FeatureRef     `json:"features"`
	RulesText      []string         `json:"rules_text"`
	Implementation map[string]any   `json:"implementation,omitempty"`
	Proposed       []map[string]any `json:"proposed_features"`
	ExampleCount   int              `json:"examples"`
}

type FeatureRef struct {
	ID     string         `json:"id"`
	Params map[string]any `json:"params,omitempty"`
}

// GetSpec đọc spec theo (operator, capability); ErrNoSpec khi chưa có (job logic.spec chưa chạy).
var ErrNoSpec = fmt.Errorf("chưa có logic spec — chạy job logic.spec (extract_logic_spec) trước")

func GetSpec(ctx context.Context, db *pgxpool.Pool, operator, capability string) (Spec, error) {
	rows, err := db.Query(ctx, `
		SELECT features::text, rules_text, coalesce(implementation::text, ''), proposed_features::text,
		       (SELECT count(*) FROM logic_tests t WHERE t.operator_id = s.operator_id
		         AND t.capability = s.capability) AS examples
		FROM logic_specs s
		WHERE s.operator_id = $1 AND s.capability = $2 AND s.status = 'active'`, operator, capability)
	if err != nil {
		return Spec{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Spec{}, ErrNoSpec
	}
	var s Spec
	var feats, impl, proposed string
	if err := rows.Scan(&feats, &s.RulesText, &impl, &proposed, &s.ExampleCount); err != nil {
		return Spec{}, err
	}
	s.Operator, s.Capability = operator, capability
	_ = json.Unmarshal([]byte(feats), &s.Features)
	if impl != "" && impl != "null" {
		_ = json.Unmarshal([]byte(impl), &s.Implementation)
	}
	_ = json.Unmarshal([]byte(proposed), &s.Proposed)
	if s.Proposed == nil {
		s.Proposed = []map[string]any{}
	}
	return s, nil
}

// Candidate: một nhà xe tương tự kèm giải thích.
type Candidate struct {
	Operator   string      `json:"operator"`
	Score      float64     `json:"score"`
	Overlap    float64     `json:"feature_overlap"`
	Rules      float64     `json:"rule_text_similarity"`
	Params     float64     `json:"param_closeness"`
	Common     []string    `json:"common"`
	Missing    []string    `json:"missing"` // mình có, họ không có
	Extra      []string    `json:"extra"`   // họ có, mình không có
	ParamDiffs []ParamDiff `json:"param_diffs"`
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type ParamDiff struct {
	Feature string `json:"feature"`
	Key     string `json:"key"`
	Mine    any    `json:"mine"`
	Theirs  any    `json:"theirs"`
}

type specRow struct {
	operator  string
	features  []FeatureRef
	rulesText string
	embedding []float32
}

// Similar xếp hạng các nhà xe khác có spec cùng capability; limit 0 = mặc định 10.
func Similar(ctx context.Context, db *pgxpool.Pool, operator, capability string, limit int) ([]Candidate, error) {
	mine, err := GetSpec(ctx, db, operator, capability)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(ctx, `
		SELECT operator_id, features::text, coalesce(array_to_string(rules_text, ' '), ''),
		       coalesce(embedding::text, '')
		FROM logic_specs
		WHERE capability = $1 AND status = 'active' AND operator_id <> $2`, capability, operator)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	corpus := []specRow{{operator: mine.Operator, features: mine.Features,
		rulesText: strings.Join(mine.RulesText, " ")}}
	for rows.Next() {
		var r specRow
		var feats, rules, emb string
		if err := rows.Scan(&r.operator, &feats, &rules, &emb); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(feats), &r.features)
		r.rulesText = rules
		r.embedding = parseVector(emb)
		corpus = append(corpus, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Trọng số theo độ hiếm (IDF): feature nhiều nhà xe dùng → trọng số thấp.
	n := float64(len(corpus))
	df := map[string]float64{}
	for _, r := range corpus {
		for _, f := range r.features {
			df[f.ID]++
		}
	}
	weight := func(id string) float64 { return math.Log(1 + n/(1+df[id])) }

	var mineEmb []float32
	if len(corpus) > 0 && corpus[0].operator == mine.Operator {
		mineEmb = corpus[0].embedding
	}
	mineRules := textnorm.SearchText(corpus[0].rulesText)

	var cands []Candidate
	for _, r := range corpus[1:] {
		c := compare(mine, mineEmb, mineRules, r, weight)
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	if len(cands) > limit {
		cands = cands[:limit]
	}
	return cands, nil
}

func compare(mine Spec, mineEmb []float32, mineRules string, other specRow, weight func(string) float64) Candidate {
	c := Candidate{Operator: other.operator}
	mineSet := map[string]FeatureRef{}
	for _, f := range mine.Features {
		mineSet[f.ID] = f
	}
	otherSet := map[string]FeatureRef{}
	for _, f := range other.features {
		otherSet[f.ID] = f
	}
	inter := 0.0
	union := 0.0
	for id, f := range mineSet {
		w := weight(id)
		union += w
		if _, ok := otherSet[id]; ok {
			inter += w
			c.Common = append(c.Common, id)
			c.ParamDiffs = append(c.ParamDiffs, paramDiffs(id, f, otherSet[id])...)
		} else {
			c.Missing = append(c.Missing, id)
		}
	}
	for id := range otherSet {
		if _, ok := mineSet[id]; !ok {
			union += weight(id)
			c.Extra = append(c.Extra, id)
		}
	}
	if union > 0 {
		c.Overlap = inter / union
	}
	c.Common = orEmpty(c.Common)
	c.Missing = orEmpty(c.Missing)
	c.Extra = orEmpty(c.Extra)
	sort.Strings(c.Common)
	sort.Strings(c.Missing)
	sort.Strings(c.Extra)

	// rule text: cosine embedding khi cả hai có vector, không thì bigram Jaccard (keyword).
	if len(mineEmb) > 0 && len(other.embedding) > 0 {
		c.Rules = cosine(mineEmb, other.embedding)
	} else {
		c.Rules = jaccard(mineRules, textnorm.SearchText(other.rulesText))
	}

	// param closeness: trung bình theo feature chung (feature không có params coi như khớp).
	sum, n := 0.0, 0.0
	for _, id := range c.Common {
		a, b := mineSet[id], otherSet[id]
		n++
		sum += paramScore(a.Params, b.Params)
	}
	if n > 0 {
		c.Params = sum / n
	}
	c.Score = 0.5*c.Overlap + 0.3*c.Rules + 0.2*c.Params
	return c
}

func paramDiffs(feature string, a, b FeatureRef) []ParamDiff {
	var diffs []ParamDiff
	keys := map[string]bool{}
	for k := range a.Params {
		keys[k] = true
	}
	for k := range b.Params {
		keys[k] = true
	}
	for k := range keys {
		av, bv := a.Params[k], b.Params[k]
		if fmt.Sprint(av) != fmt.Sprint(bv) {
			diffs = append(diffs, ParamDiff{Feature: feature, Key: k, Mine: av, Theirs: bv})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Feature+diffs[i].Key < diffs[j].Feature+diffs[j].Key })
	return diffs
}

func paramScore(a, b map[string]any) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sum, n := 0.0, float64(len(keys))
	for k := range keys {
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok || !bok:
			// một bên không có tham số này
		case fmt.Sprint(av) == fmt.Sprint(bv):
			sum++
		default:
			an, aerr := toFloat(av)
			bn, berr := toFloat(bv)
			if aerr == nil && berr == nil {
				d := math.Abs(an-bn) / math.Max(math.Abs(an), math.Abs(bn))
				if d <= 0.2 {
					sum += 0.7 // số gần nhau (20% vs 25%)
				}
			}
		}
	}
	return sum / n
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(x, 64)
	}
	return 0, fmt.Errorf("không phải số")
}

func parseVector(s string) []float32 {
	s = strings.Trim(s, "[]")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil
		}
		out[i] = float32(f)
	}
	return out
}

func cosine(a, b []float32) float64 {
	n := min(len(a), len(b))
	var dot, na, nb float64
	for i := range n {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func jaccard(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, t := range strings.Fields(s) {
			m[t] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	inter := 0
	for t := range sa {
		if sb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// ───────────────────────── get_operator_logic ─────────────────────────

// CapabilityState: trạng thái logic của nhà xe theo một capability của template.
type CapabilityState struct {
	ID       string          `json:"capability"`
	Title    string          `json:"title"`
	Level    string          `json:"level"` // required | recommended
	Profile  *ProfileSummary `json:"profile,omitempty"`
	Spec     *SpecSummary    `json:"spec,omitempty"`
	Proposed int             `json:"proposed_features"`
}

type ProfileSummary struct {
	Mode   string         `json:"mode"`
	Module string         `json:"module"`
	Hooks  map[string]any `json:"hooks,omitempty"`
	Status string         `json:"status"`
	Commit string         `json:"commit"`
}

type SpecSummary struct {
	FeatureCount int `json:"features"`
	RuleCount    int `json:"rules"`
	ExampleCount int `json:"examples"`
}

// Overview: hồ sơ logic của nhà xe theo các capability của template (chưa có profile → "còn thiếu").
func Overview(ctx context.Context, db *pgxpool.Pool, operator string, capabilities []CapabilityInfo) ([]CapabilityState, error) {
	states := make([]CapabilityState, 0, len(capabilities))
	for _, cap := range capabilities {
		st := CapabilityState{ID: cap.ID, Title: cap.Title, Level: cap.Level}
		var prof *ProfileSummary
		var mode, module, status, commit string
		var hooks string
		err := db.QueryRow(ctx, `
			SELECT mode, module_id, coalesce(hooks::text, '{}'), status, commit
			FROM logic_profiles WHERE operator_id = $1 AND capability = $2`, operator, cap.ID).
			Scan(&mode, &module, &hooks, &status, &commit)
		if err == nil {
			prof = &ProfileSummary{Mode: mode, Module: module, Status: status, Commit: commit}
			_ = json.Unmarshal([]byte(hooks), &prof.Hooks)
		}
		st.Profile = prof
		if spec, err := GetSpec(ctx, db, operator, cap.ID); err == nil {
			st.Spec = &SpecSummary{FeatureCount: len(spec.Features), RuleCount: len(spec.RulesText),
				ExampleCount: spec.ExampleCount}
			st.Proposed = len(spec.Proposed)
		}
		states = append(states, st)
	}
	return states, nil
}

// CapabilityInfo: đầu vào cho Overview — từ template ngành.
type CapabilityInfo struct {
	ID, Title, Level string
}

// Compare so spec của hai nhà xe theo một capability — dùng cho compare_logic.
func Compare(ctx context.Context, db *pgxpool.Pool, operator, other, capability string) (*Candidate, error) {
	mine, err := GetSpec(ctx, db, operator, capability)
	if err != nil {
		return nil, err
	}
	var feats, rules, emb string
	err = db.QueryRow(ctx, `
		SELECT features::text, coalesce(array_to_string(rules_text, ' '), ''), coalesce(embedding::text, '')
		FROM logic_specs
		WHERE capability = $1 AND status = 'active' AND operator_id = $2`, capability, other).
		Scan(&feats, &rules, &emb)
	if err != nil {
		return nil, fmt.Errorf("nhà xe %s chưa có spec %s: %w", other, capability, ErrNoSpec)
	}
	var r specRow
	_ = json.Unmarshal([]byte(feats), &r.features)
	r.rulesText, r.embedding, r.operator = rules, parseVector(emb), other

	rows, err := db.Query(ctx, `
		SELECT operator_id, features::text, coalesce(array_to_string(rules_text, ' '), '')
		FROM logic_specs WHERE capability = $1 AND status = 'active'`, capability)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	n := 0
	df := map[string]float64{}
	for rows.Next() {
		n++
		var op, f, rl string
		if err := rows.Scan(&op, &f, &rl); err != nil {
			return nil, err
		}
		var fr []FeatureRef
		_ = json.Unmarshal([]byte(f), &fr)
		for _, x := range fr {
			df[x.ID]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	total := float64(n) + 1
	weight := func(id string) float64 { return math.Log(1 + total/(1+df[id])) }
	c := compare(mine, nil, textnorm.SearchText(strings.Join(mine.RulesText, " ")), r, weight)
	return &c, nil
}
