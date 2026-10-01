// Package pack dựng knowledge pack — một gói tri thức đã xử lý sẵn để AI build bot (M1, S1.4.1–S1.4.2),
// thay vì AI tự ghép hàng chục lần recall. Thiết kế: docs/mcp.md §3.
//
// Luật kế thừa:
//   - rule locked (L0/L1) luôn có mặt, không tính vào ngân sách cắt bỏ;
//   - L2 của nhà xe thắng L1: topic đã có tri thức L2 (không tính data) thì thông lệ L1 của topic đó bị thay
//     (ghi vào overrides của item L2), không đưa vào gói;
//   - thông lệ L1 chỉ dùng khi nhà xe chưa có gì ở topic đó, luôn mang nhãn "thông lệ chung";
//   - data vận hành (giá, lịch...) chỉ tóm tắt: con số cụ thể tra bằng query_data, bot gọi tool lúc chạy.
//
// Cache theo (nhà xe, purpose, ngân sách, ngày, version tri thức) — version tăng bằng trigger khi item active đổi
// (migration 000010), nên đổi tri thức là cache tự vô hiệu.
package pack

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/recall"
)

const (
	DefaultBudget = 6000
	MaxBudget     = 30000
)

var Purposes = []string{"build", "faq", "logic", "review"}

// Thứ tự ưu tiên các phần khi cắt theo ngân sách (rule locked và gaps luôn có).
var priority = map[string][]string{
	"build":  {"persona", "policies", "lessons", "data_summary"},
	"faq":    {"policies", "data_summary", "persona", "lessons"},
	"logic":  {"data_summary", "policies", "lessons", "persona"},
	"review": {"policies", "data_summary", "lessons", "persona"},
}

type TopicSpec struct {
	ID       string
	Title    string
	Required bool
}

type Entry struct {
	ID        string            `json:"id"`
	Layer     string            `json:"layer"`
	Label     string            `json:"label,omitempty"`
	Locked    bool              `json:"locked,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Topic     string            `json:"topic"`
	Key       string            `json:"key,omitempty"`
	Text      string            `json:"text"`
	Facts     map[string]string `json:"facts,omitempty"`
	ValidFrom *time.Time        `json:"valid_from,omitempty"`
	ValidTo   *time.Time        `json:"valid_to,omitempty"`
	Overrides []string          `json:"overrides,omitempty"` // id thông lệ L1 bị item này thay
}

type DataTopic struct {
	Topic   string    `json:"topic"`
	Title   string    `json:"title,omitempty"`
	Count   int       `json:"count"`
	Updated time.Time `json:"updated"`
	Samples []Sample  `json:"samples"` // vài dòng mẫu (cắt ngắn) để biết có gì; không dùng làm con số
	Lookup  string    `json:"lookup"`
}

// Sample: một dòng data mẫu, có id để trích dẫn khi artifact nói "nhà xe có chạy tuyến X" (không nêu con số).
type Sample struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Gap struct {
	Topic  string `json:"topic"`
	Title  string `json:"title"`
	Level  string `json:"level"`  // required | recommended
	Status string `json:"status"` // missing | industry_default (đang dùng thông lệ L1, cần nhà xe xác nhận)
}

type Pack struct {
	Operator         string         `json:"operator"`
	AsOf             string         `json:"as_of"`
	Purpose          string         `json:"purpose"`
	BudgetTokens     int            `json:"budget_tokens"`
	UsedTokens       int            `json:"used_tokens"`
	KnowledgeVersion string         `json:"knowledge_version"`
	Persona          []Entry        `json:"persona"`
	Rules            []Entry        `json:"rules"`
	Policies         []Entry        `json:"policies"`
	Lessons          []Entry        `json:"lessons"`
	DataSummary      []DataTopic    `json:"data_summary"`
	Gaps             []Gap          `json:"gaps"`
	Omitted          map[string]int `json:"omitted,omitempty"` // số mục bị cắt vì ngân sách, theo phần
	Cached           bool           `json:"cached"`
	NextActions      []string       `json:"next_actions"`
}

type Query struct {
	OperatorID string
	Purpose    string
	Budget     int
	Day        time.Time // ngày as_of (00:00 giờ VN); zero = hôm nay
}

type Builder struct {
	DB     *pgxpool.Pool
	Topics []TopicSpec
	TTL    time.Duration // mặc định 10 phút; version đổi thì key đổi nên TTL chỉ để dọn bộ nhớ

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	pack Pack
	at   time.Time
}

const maxCache = 512

var vn = time.FixedZone("ICT", 7*3600)

// Version: version tri thức của scope dạng "<L0/L1>.<nhà xe>".
func Version(ctx context.Context, db *pgxpool.Pool, operatorID string) (string, error) {
	var g, o int64
	err := db.QueryRow(ctx, `SELECT
			COALESCE((SELECT version FROM knowledge_versions WHERE scope = '*'), 0),
			COALESCE((SELECT version FROM knowledge_versions WHERE scope = $1), 0)`, operatorID).Scan(&g, &o)
	return fmt.Sprintf("%d.%d", g, o), err
}

func (b *Builder) Build(ctx context.Context, q Query) (Pack, error) {
	if q.Purpose == "" {
		q.Purpose = "build"
	}
	if !slices.Contains(Purposes, q.Purpose) {
		return Pack{}, fmt.Errorf("purpose phải là: %s", strings.Join(Purposes, ", "))
	}
	if q.Budget <= 0 {
		q.Budget = DefaultBudget
	}
	q.Budget = min(q.Budget, MaxBudget)
	if q.Day.IsZero() {
		now := time.Now().In(vn)
		q.Day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, vn)
	}
	version, err := Version(ctx, b.DB, q.OperatorID)
	if err != nil {
		return Pack{}, err
	}
	key := strings.Join([]string{q.OperatorID, q.Purpose, fmt.Sprint(q.Budget), q.Day.Format("2006-01-02"), version}, "|")
	if p, ok := b.get(key); ok {
		p.Cached = true
		return p, nil
	}
	p, err := b.build(ctx, q)
	if err != nil {
		return Pack{}, err
	}
	p.KnowledgeVersion = version
	b.put(key, p)
	return p, nil
}

func (b *Builder) get(key string) (Pack, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.cache[key]
	ttl := b.TTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if !ok || time.Since(c.at) > ttl {
		return Pack{}, false
	}
	return c.pack, true
}

func (b *Builder) put(key string, p Pack) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cache == nil || len(b.cache) >= maxCache {
		b.cache = map[string]cached{} // đơn giản: đầy thì xoá hết (version đổi làm key cũ vô dụng)
	}
	b.cache[key] = cached{pack: p, at: time.Now()}
}

type row struct {
	Entry
	layer   int
	updated time.Time
}

func (b *Builder) build(ctx context.Context, q Query) (Pack, error) {
	start, end := q.Day, q.Day.Add(24*time.Hour-time.Microsecond)
	rows, err := b.DB.Query(ctx, `SELECT id::text, layer, locked, kind, topic, key, text, value, valid_from, valid_to,
			metadata, updated_at
		FROM items
		WHERE status = 'active' AND kind <> 'observation' AND (layer <= 1 OR (layer = 2 AND operator_id = $1))
			AND (valid_from IS NULL OR valid_from <= $3) AND (valid_to IS NULL OR valid_to >= $2)
		ORDER BY layer, topic, key NULLS LAST, valid_from NULLS FIRST, id`, q.OperatorID, start, end)
	if err != nil {
		return Pack{}, err
	}
	defer rows.Close()
	var all []row
	for rows.Next() {
		var r row
		var key *string
		var value, meta []byte
		if err := rows.Scan(&r.ID, &r.layer, &r.Locked, &r.Kind, &r.Topic, &key, &r.Text, &value, &r.ValidFrom,
			&r.ValidTo, &meta, &r.updated); err != nil {
			return Pack{}, err
		}
		if key != nil {
			r.Key = *key
		}
		r.Facts = recall.Facts(value)
		var m struct {
			Label string `json:"label"`
		}
		_ = json.Unmarshal(meta, &m)
		r.Layer, r.Label = recall.LayerLabel(r.layer, r.Locked, m.Label)
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return Pack{}, err
	}
	return b.assemble(q, all), nil
}

func (b *Builder) assemble(q Query, all []row) Pack {
	p := Pack{Operator: q.OperatorID, AsOf: q.Day.Format("2006-01-02"), Purpose: q.Purpose, BudgetTokens: q.Budget,
		Persona: []Entry{}, Rules: []Entry{}, Policies: []Entry{}, Lessons: []Entry{}, DataSummary: []DataTopic{},
		Gaps: []Gap{}, NextActions: []string{}}

	// Topic nhà xe đã có tri thức riêng (không tính data) → thông lệ L1 của topic đó bị thay.
	ownTopic := map[string]bool{}
	ownAny := map[string]bool{} // kể cả data — cho coverage
	hasPersona := false
	for _, r := range all {
		if r.layer == 2 {
			ownAny[r.Topic] = true
			if r.Kind != "data" {
				ownTopic[r.Topic] = true
			}
			if r.Kind == "persona" {
				hasPersona = true
			}
		}
	}
	defaultsByTopic := map[string][]string{}
	defaultTopic := map[string]bool{}

	var persona, policies, lessons []Entry
	data := map[string]*DataTopic{}
	for _, r := range all {
		e := r.Entry
		switch {
		case r.layer <= 1 && r.Locked:
			p.Rules = append(p.Rules, e)
		case r.Kind == "lesson":
			lessons = append(lessons, e)
		case r.layer <= 1 && r.Kind == "persona":
			if !hasPersona {
				persona = append(persona, e)
			}
		case r.layer <= 1:
			if ownTopic[r.Topic] {
				defaultsByTopic[r.Topic] = append(defaultsByTopic[r.Topic], r.ID)
				continue
			}
			defaultTopic[r.Topic] = true
			policies = append(policies, e)
		case r.Kind == "persona":
			persona = append(persona, e)
		case r.Kind == "data":
			d := data[r.Topic]
			if d == nil {
				d = &DataTopic{Topic: r.Topic, Lookup: fmt.Sprintf("query_data(topics=[%s])", r.Topic)}
				data[r.Topic] = d
			}
			d.Count++
			if r.updated.After(d.Updated) {
				d.Updated = r.updated
			}
			if len(d.Samples) < 3 {
				d.Samples = append(d.Samples, Sample{ID: r.ID, Text: clip(r.Text, 80)})
			}
		default:
			policies = append(policies, e)
		}
	}
	for i := range policies {
		if policies[i].Layer == "L2" {
			policies[i].Overrides = defaultsByTopic[policies[i].Topic]
		}
	}
	order := b.topicOrder()
	sortEntries(policies, order)
	sortEntries(lessons, order)
	titles := map[string]string{}
	for _, t := range b.Topics {
		titles[t.ID] = t.Title
	}
	var dataList []DataTopic
	for _, d := range data {
		d.Title = titles[d.Topic]
		dataList = append(dataList, *d)
	}
	sort.Slice(dataList, func(i, j int) bool { return rank(order, dataList[i].Topic) < rank(order, dataList[j].Topic) })

	for _, t := range b.Topics {
		level := "recommended"
		if t.Required {
			level = "required"
		}
		switch {
		case ownAny[t.ID]:
		case defaultTopic[t.ID]:
			p.Gaps = append(p.Gaps, Gap{Topic: t.ID, Title: t.Title, Level: level, Status: "industry_default"})
		default:
			p.Gaps = append(p.Gaps, Gap{Topic: t.ID, Title: t.Title, Level: level, Status: "missing"})
		}
	}
	sort.SliceStable(p.Gaps, func(i, j int) bool { return p.Gaps[i].Level == "required" && p.Gaps[j].Level != "required" })

	// Ngân sách: rule locked + gaps luôn có; các phần khác theo thứ tự ưu tiên của purpose.
	used := 60 // khung JSON
	for _, e := range p.Rules {
		used += entryTokens(e)
	}
	used += len(p.Gaps) * 12
	omitted := map[string]int{}
	omittedTopics := map[string]bool{}
	take := func(section string, in []Entry) []Entry {
		out := []Entry{}
		for _, e := range in {
			if c := entryTokens(e); used+c <= q.Budget {
				used += c
				out = append(out, e)
			} else {
				omitted[section]++
				omittedTopics[e.Topic] = true
			}
		}
		return out
	}
	for _, section := range priority[q.Purpose] {
		switch section {
		case "persona":
			p.Persona = take(section, persona)
		case "policies":
			p.Policies = take(section, policies)
		case "lessons":
			p.Lessons = take(section, lessons)
		case "data_summary":
			for _, d := range dataList {
				c := 30 + len(d.Samples)*40
				if used+c > q.Budget {
					omitted[section]++
					omittedTopics[d.Topic] = true
					continue
				}
				used += c
				p.DataSummary = append(p.DataSummary, d)
			}
		}
	}
	p.UsedTokens = used
	if len(omitted) > 0 {
		p.Omitted = omitted
		var ts []string
		for t := range omittedTopics {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		p.NextActions = append(p.NextActions, "phần bị cắt vì ngân sách: recall_knowledge(topics=["+
			strings.Join(ts, ", ")+"]) hoặc tăng budget_tokens")
	}
	if slices.ContainsFunc(p.Gaps, func(g Gap) bool { return g.Level == "required" && g.Status == "missing" }) {
		p.NextActions = append(p.NextActions, "còn mục bắt buộc chưa có tri thức (gaps): hỏi nhà xe rồi submit_knowledge; "+
			"trong artifact dùng fallback, không bịa")
	}
	p.NextActions = append(p.NextActions, "trích dẫn [[id]] cho mọi câu mang thông tin; item label 'thông lệ chung' "+
		"phải nói rõ là thông lệ; rule locked phải có trong system_prompt", "save_artifact")
	return p
}

func (b *Builder) topicOrder() map[string]int {
	m := map[string]int{}
	for i, t := range b.Topics {
		m[t.ID] = i
	}
	return m
}

func rank(order map[string]int, topic string) int {
	if r, ok := order[topic]; ok {
		return r
	}
	return len(order)
}

// sortEntries: theo thứ tự topic của template (bắt buộc trước), trong topic thì nhà xe (L2) trước thông lệ.
func sortEntries(es []Entry, order map[string]int) {
	sort.SliceStable(es, func(i, j int) bool {
		ri, rj := rank(order, es[i].Topic), rank(order, es[j].Topic)
		if ri != rj {
			return ri < rj
		}
		if es[i].Topic != es[j].Topic {
			return es[i].Topic < es[j].Topic
		}
		return es[i].Layer > es[j].Layer
	})
}

func entryTokens(e Entry) int {
	n := utf8.RuneCountInString(e.Text) + len(e.Key) + len(e.Topic) + len(e.Label)
	for k, v := range e.Facts {
		n += len(k) + utf8.RuneCountInString(v)
	}
	return n/3 + 30
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
