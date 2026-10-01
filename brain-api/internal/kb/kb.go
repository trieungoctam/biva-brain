// Package kb nạp tri thức nền L0/L1 từ thư mục kb/ (YAML, review bằng PR) vào bảng items.
//
//	kb/L0/rules.yaml                 luật nền tảng (layer 0)
//	kb/L1/<ngành>/rules.yaml         thông lệ ngành (layer 1)
//	kb/L1/<ngành>/template.yaml      template onboarding (mục bắt buộc/khuyến nghị, capability, artifact)
//
// Sync idempotent theo key: không đổi → bỏ qua; đổi nội dung → bản cũ superseded, bản mới active (trích dẫn
// cũ trở thành stale); key bị xoá khỏi kb/ → retracted. Mỗi thay đổi ghi audit_log; có thay đổi → enqueue
// index.items. Spec file: kb/README.md, schema: contracts/schemas/kb/*.schema.json.
package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/trieungoctam/biva-brain/brain-api/internal/entity"
	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

type Rule struct {
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Topic  string         `json:"topic"`
	Locked bool           `json:"locked"`
	Text   string         `json:"text"`
	Value  map[string]any `json:"value,omitempty"`

	Layer    int    `json:"-"`
	Industry string `json:"-"`
	Source   string `json:"-"` // đường dẫn tương đối trong kb/
}

type rulesFile struct {
	Layer    int    `json:"layer"`
	Industry string `json:"industry"`
	Rules    []Rule `json:"rules"`
}

type Section struct {
	Topic     string   `json:"topic"`
	Level     string   `json:"level"`
	Title     string   `json:"title"`
	Facts     []string `json:"facts"`
	Questions []string `json:"questions"`
}

type Capability struct {
	ID    string   `json:"id"`
	Level string   `json:"level"`
	Title string   `json:"title"`
	Modes []string `json:"modes,omitempty"`
}

type Template struct {
	Industry     string       `json:"industry"`
	Version      int          `json:"version"`
	Sections     []Section    `json:"sections"`
	Capabilities []Capability `json:"capabilities"`
	Artifacts    struct {
		Required    []string `json:"required"`
		Recommended []string `json:"recommended,omitempty"`
	} `json:"artifacts"`
}

// Feature: một điểm khác biệt về cách xử lý (logic) dùng so khớp nhà xe — kb/L1/<ngành>/features.yaml.
type Feature struct {
	ID           string         `json:"id"`
	Capability   string         `json:"capability"`
	Description  string         `json:"description"`
	ParamsSchema map[string]any `json:"params_schema,omitempty"`
}

// Bundle là toàn bộ nội dung kb/ đã kiểm tra.
type Bundle struct {
	Rules     []Rule
	Templates map[string]Template        // theo ngành
	Entities  map[string][]entity.Entity // từ điển thực thể + alias theo ngành (entities.yaml)
	Features  map[string][]Feature       // danh mục feature logic L1 theo ngành (features.yaml)
}

// Load đọc và kiểm tra kb/: schema, key duy nhất, prefix key khớp tầng, topic L1 có trong template ngành.
func Load(kbDir, schemasDir string) (*Bundle, error) {
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	rulesSchema, err := compiler.Compile(filepath.Join(schemasDir, "kb", "rules.schema.json"))
	if err != nil {
		return nil, fmt.Errorf("schema kb/rules: %w", err)
	}
	templateSchema, err := compiler.Compile(filepath.Join(schemasDir, "kb", "template.schema.json"))
	if err != nil {
		return nil, fmt.Errorf("schema kb/template: %w", err)
	}
	entitiesSchema, err := compiler.Compile(filepath.Join(schemasDir, "kb", "entities.schema.json"))
	if err != nil {
		return nil, fmt.Errorf("schema kb/entities: %w", err)
	}
	featuresSchema, err := compiler.Compile(filepath.Join(schemasDir, "kb", "features.schema.json"))
	if err != nil {
		return nil, fmt.Errorf("schema kb/features: %w", err)
	}

	b := &Bundle{Templates: map[string]Template{}, Entities: map[string][]entity.Entity{},
		Features: map[string][]Feature{}}
	files, err := filepath.Glob(filepath.Join(kbDir, "L0", "*.yaml"))
	if err != nil {
		return nil, err
	}
	l1, _ := filepath.Glob(filepath.Join(kbDir, "L1", "*", "*.yaml"))
	files = append(files, l1...)
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("không có file YAML nào trong %s", kbDir)
	}

	seen := map[string]string{}
	var problems []string
	for _, path := range files {
		rel, _ := filepath.Rel(kbDir, path)
		rel = "kb/" + filepath.ToSlash(rel)
		doc, err := readYAML(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if filepath.Base(path) == "template.yaml" {
			if err := templateSchema.Validate(doc); err != nil {
				return nil, fmt.Errorf("%s: %v", rel, err)
			}
			var t Template
			if err := remarshal(doc, &t); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if want := filepath.Base(filepath.Dir(path)); t.Industry != want {
				problems = append(problems, fmt.Sprintf("%s: industry %q phải trùng tên thư mục %q", rel, t.Industry, want))
			}
			b.Templates[t.Industry] = t
			continue
		}
		if filepath.Base(path) == "features.yaml" {
			if err := featuresSchema.Validate(doc); err != nil {
				return nil, fmt.Errorf("%s: %v", rel, err)
			}
			var f struct {
				Industry string    `json:"industry"`
				Features []Feature `json:"features"`
			}
			if err := remarshal(doc, &f); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if want := filepath.Base(filepath.Dir(path)); f.Industry != want {
				problems = append(problems, fmt.Sprintf("%s: industry %q phải trùng tên thư mục %q", rel, f.Industry, want))
			}
			// Template đọc sau theo thứ tự tên file — check capability chuyển xuống cuối Load.
			ids := map[string]bool{}
			for _, feat := range f.Features {
				prefix, _, _ := strings.Cut(feat.ID, ".")
				if prefix != feat.Capability {
					problems = append(problems, fmt.Sprintf("%s: id %s phải có tiền tố %q.", rel, feat.ID, feat.Capability))
				}
				if ids[feat.ID] {
					problems = append(problems, fmt.Sprintf("%s: id %s trùng", rel, feat.ID))
				}
				ids[feat.ID] = true
			}
			b.Features[f.Industry] = f.Features
			continue
		}
		if filepath.Base(path) == "entities.yaml" {
			if err := entitiesSchema.Validate(doc); err != nil {
				return nil, fmt.Errorf("%s: %v", rel, err)
			}
			var f struct {
				Industry string          `json:"industry"`
				Entities []entity.Entity `json:"entities"`
			}
			if err := remarshal(doc, &f); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if want := filepath.Base(filepath.Dir(path)); f.Industry != want {
				problems = append(problems, fmt.Sprintf("%s: industry %q phải trùng tên thư mục %q", rel, f.Industry, want))
			}
			if _, err := entity.New(f.Entities); err != nil { // alias trùng giữa hai thực thể
				problems = append(problems, rel+": "+err.Error())
			}
			ids := map[string]bool{}
			for _, e := range f.Entities {
				if ids[e.ID] {
					problems = append(problems, fmt.Sprintf("%s: id %s trùng", rel, e.ID))
				}
				ids[e.ID] = true
			}
			b.Entities[f.Industry] = f.Entities
			continue
		}
		if err := rulesSchema.Validate(doc); err != nil {
			return nil, fmt.Errorf("%s: %v", rel, err)
		}
		var f rulesFile
		if err := remarshal(doc, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		inL0 := strings.Contains(filepath.ToSlash(path), "/L0/")
		if (f.Layer == 0) != inL0 {
			problems = append(problems, fmt.Sprintf("%s: layer %d không khớp thư mục", rel, f.Layer))
		}
		for _, r := range f.Rules {
			if !strings.HasPrefix(r.Key, fmt.Sprintf("l%d.", f.Layer)) {
				problems = append(problems, fmt.Sprintf("%s: key %s phải bắt đầu bằng l%d.", rel, r.Key, f.Layer))
			}
			if prev, dup := seen[r.Key]; dup {
				problems = append(problems, fmt.Sprintf("%s: key %s trùng với %s", rel, r.Key, prev))
			}
			seen[r.Key] = rel
			r.Layer, r.Industry, r.Source = f.Layer, f.Industry, rel
			r.Text = strings.TrimSpace(r.Text)
			b.Rules = append(b.Rules, r)
		}
	}
	// Topic của rule L1 phải thuộc bộ từ vựng của template ngành.
	for _, r := range b.Rules {
		if r.Layer != 1 {
			continue
		}
		t, ok := b.Templates[r.Industry]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: ngành %s chưa có template.yaml", r.Source, r.Industry))
			continue
		}
		if !t.hasTopic(r.Topic) {
			problems = append(problems, fmt.Sprintf("%s: %s dùng topic %q không có trong template", r.Source, r.Key, r.Topic))
		}
	}
	// Feature phải thuộc capability của template ngành (template đọc sau features theo thứ tự tên file).
	for industry, feats := range b.Features {
		t, ok := b.Templates[industry]
		if !ok {
			problems = append(problems, fmt.Sprintf("kb/L1/%s/features.yaml: ngành %s chưa có template.yaml", industry, industry))
			continue
		}
		for _, feat := range feats {
			if !t.hasCapability(feat.Capability) {
				problems = append(problems, fmt.Sprintf("kb/L1/%s/features.yaml: capability %s không có trong template", industry, feat.Capability))
			}
		}
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return b, nil
}

func (t Template) hasTopic(topic string) bool {
	for _, s := range t.Sections {
		if s.Topic == topic {
			return true
		}
	}
	return false
}

func (t Template) hasCapability(id string) bool {
	for _, c := range t.Capabilities {
		if c.ID == id {
			return true
		}
	}
	return false
}

func readYAML(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	// Qua JSON để có kiểu mà validator JSON Schema hiểu (map[string]any, float64...).
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
}

func remarshal(doc any, out any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// ─────────────────────────────── sync ───────────────────────────────

type Change struct {
	Action string // add | update | retire
	Key    string
	Layer  int
}

type Report struct {
	Changes   []Change
	Unchanged int
	IndexJob  string
}

func (r Report) Count(action string) int {
	n := 0
	for _, c := range r.Changes {
		if c.Action == action {
			n++
		}
	}
	return n
}

// lockKey: advisory lock cho kb sync (tránh hai lệnh sync chạy chồng).
const lockKey int64 = 0x0B1A_6B01

type activeItem struct {
	id     string
	kind   string
	topic  string
	text   string
	locked bool
	value  map[string]any
}

// Sync đưa items L0/L1 về đúng nội dung bundle. dryRun = chỉ tính thay đổi, rollback.
func Sync(ctx context.Context, db *pgxpool.Pool, b *Bundle, actor string, dryRun bool) (Report, error) {
	var rep Report
	tx, err := db.Begin(ctx)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return rep, err
	}

	active := map[string]activeItem{}
	rows, err := tx.Query(ctx, `
		SELECT id::text, key, kind, topic, text, locked, value FROM items
		WHERE operator_id IS NULL AND layer IN (0, 1) AND status = 'active' AND key IS NOT NULL
		  AND metadata->>'source' LIKE 'kb/%'`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var a activeItem
		var key string
		if err := rows.Scan(&a.id, &key, &a.kind, &a.topic, &a.text, &a.locked, &a.value); err != nil {
			rows.Close()
			return rep, err
		}
		active[key] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	record := func(action string, r Rule, payload map[string]any) error {
		rep.Changes = append(rep.Changes, Change{Action: action, Key: r.Key, Layer: r.Layer})
		return auditTx(ctx, tx, actor, "kb."+action, "kb:"+r.Key, payload)
	}

	inBundle := map[string]bool{}
	for _, r := range b.Rules {
		inBundle[r.Key] = true
		old, exists := active[r.Key]
		if exists && old.kind == r.Kind && old.topic == r.Topic && old.text == r.Text && old.locked == r.Locked &&
			sameValue(old.value, r.Value) {
			rep.Unchanged++
			continue
		}
		if exists {
			// Nhả bản cũ trước (unique index chỉ cho một bản active), rồi nối superseded_by sau khi có bản mới.
			if _, err := tx.Exec(ctx, `UPDATE items SET status = 'superseded', updated_at = now() WHERE id = $1`, old.id); err != nil {
				return rep, err
			}
		}
		newID, err := insertRule(ctx, tx, r)
		if err != nil {
			return rep, fmt.Errorf("%s: %w", r.Key, err)
		}
		if exists {
			if _, err := tx.Exec(ctx, `UPDATE items SET superseded_by = $2 WHERE id = $1`, old.id, newID); err != nil {
				return rep, err
			}
			err = record("update", r, map[string]any{"old_id": old.id, "new_id": newID, "source": r.Source})
		} else {
			err = record("add", r, map[string]any{"id": newID, "source": r.Source})
		}
		if err != nil {
			return rep, err
		}
	}
	retired := make([]string, 0)
	for key := range active {
		if !inBundle[key] {
			retired = append(retired, key)
		}
	}
	sort.Strings(retired)
	for _, key := range retired {
		a := active[key]
		if _, err := tx.Exec(ctx, `
			UPDATE items SET status = 'retracted', valid_to = now(), updated_at = now() WHERE id = $1`, a.id); err != nil {
			return rep, err
		}
		layer := 1
		if strings.HasPrefix(key, "l0.") {
			layer = 0
		}
		if err := record("retire", Rule{Key: key, Layer: layer}, map[string]any{"id": a.id}); err != nil {
			return rep, err
		}
	}

	// industries: ngành có entities hoặc features (dùng cho 2 block dưới).
	var industries []string
	seen := map[string]bool{}
	for industry := range b.Entities {
		if !seen[industry] {
			seen[industry] = true
			industries = append(industries, industry)
		}
	}
	for industry := range b.Features {
		if !seen[industry] {
			seen[industry] = true
			industries = append(industries, industry)
		}
	}
	sort.Strings(industries)
	for _, industry := range industries {
		for _, e := range b.Entities[industry] {
			aliases := append([]string{e.Name}, e.Aliases...)
			known := 0
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM entities WHERE ext_id = $1`, e.ID).
				Scan(&known); err != nil {
				return rep, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO entities (ext_id, layer, entity_type, name, name_norm, aliases, last_seen)
				VALUES ($1, 1, $2, $3, $4, $5, now())
				ON CONFLICT (ext_id) DO UPDATE SET
					entity_type = EXCLUDED.entity_type, name = EXCLUDED.name,
					name_norm = EXCLUDED.name_norm, aliases = EXCLUDED.aliases, last_seen = now()`,
				e.ID, e.Type, e.Name, textnorm.Fold(e.Name), aliases); err != nil {
				return rep, fmt.Errorf("entity %s: %w", e.ID, err)
			}
			if known == 0 {
				rep.Changes = append(rep.Changes, Change{Action: "entity.add", Key: e.ID, Layer: 1})
			}
		}
	}

	// Danh mục feature → logic_features: upsert theo id; feature rời bundle → deprecated
	// (không xoá — spec và module có thể còn tham chiếu; operator_count do index.code đếm lại).
	featIDs := map[string]bool{}
	for _, industry := range industries {
		for _, feat := range b.Features[industry] {
			featIDs[feat.ID] = true
			var known bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM logic_features WHERE id = $1)`,
				feat.ID).Scan(&known); err != nil {
				return rep, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO logic_features (id, capability, description, params_schema, status)
				VALUES ($1, $2, $3, $4::jsonb, 'active')
				ON CONFLICT (id) DO UPDATE SET
					capability = EXCLUDED.capability, description = EXCLUDED.description,
					params_schema = EXCLUDED.params_schema,
					status = CASE WHEN logic_features.status = 'deprecated' THEN 'active'
					              ELSE logic_features.status END`,
				feat.ID, feat.Capability, feat.Description, paramsSchema(feat)); err != nil {
				return rep, fmt.Errorf("feature %s: %w", feat.ID, err)
			}
			if !known {
				rep.Changes = append(rep.Changes, Change{Action: "feature.add", Key: feat.ID})
			}
		}
	}
	depRows, err := tx.Query(ctx, `SELECT id FROM logic_features WHERE status = 'active'`)
	if err != nil {
		return rep, err
	}
	var toDeprecate []string
	for depRows.Next() {
		var id string
		if err := depRows.Scan(&id); err != nil {
			depRows.Close()
			return rep, err
		}
		if !featIDs[id] {
			toDeprecate = append(toDeprecate, id)
		}
	}
	depRows.Close()
	if err := depRows.Err(); err != nil {
		return rep, err
	}
	for _, id := range toDeprecate {
		if _, err := tx.Exec(ctx, `UPDATE logic_features SET status = 'deprecated' WHERE id = $1`, id); err != nil {
			return rep, err
		}
		rep.Changes = append(rep.Changes, Change{Action: "feature.deprecate", Key: id})
	}

	if dryRun {
		return rep, nil // defer Rollback
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, err
	}
	if rep.Count("add")+rep.Count("update") > 0 {
		id, _, err := queue.Enqueue(ctx, db, queue.Job{Kind: "index.items", Payload: map[string]any{}})
		if err != nil {
			return rep, fmt.Errorf("đã sync nhưng enqueue index.items lỗi: %w", err)
		}
		rep.IndexJob = id
	}
	return rep, nil
}

func insertRule(ctx context.Context, tx pgx.Tx, r Rule) (string, error) {
	meta := map[string]any{"source": r.Source}
	if r.Industry != "" {
		meta["industry"] = r.Industry
	}
	if r.Layer == 1 && !r.Locked {
		meta["label"] = "thông lệ chung" // dùng khi nhà xe chưa có thông tin riêng phải gắn nhãn này
	}
	var value any
	if r.Value != nil {
		value = r.Value
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO items (layer, kind, topic, key, text, value, status, locked, valid_from, metadata, search_text)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, now(), $8, $9)
		RETURNING id::text`,
		r.Layer, r.Kind, r.Topic, r.Key, r.Text, value, r.Locked, meta, textnorm.ItemSearchText(r.Topic, r.Key, r.Text),
	).Scan(&id)
	return id, err
}

func sameValue(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var x, y any
	_ = json.Unmarshal(ja, &x)
	_ = json.Unmarshal(jb, &y)
	return reflect.DeepEqual(x, y)
}

// auditTx ghi audit trong cùng transaction (rollback cùng thay đổi).
func auditTx(ctx context.Context, tx pgx.Tx, actor, action, target string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (actor, action, target, payload) VALUES ($1, $2, $3, $4)`,
		actor, action, target, b)
	return err
}

// paramsSchema: nil map → JSON {} (cột NOT NULL, không ghi đè default bằng NULL).
func paramsSchema(f Feature) map[string]any {
	if len(f.ParamsSchema) == 0 {
		return map[string]any{}
	}
	return f.ParamsSchema
}
