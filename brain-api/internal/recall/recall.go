// Package recall tìm tri thức cho AI build bot (M1, S1.3.1) — recall() của Hindsight theo docs/architecture.md §3.4.
//
// Hai nhánh chạy song song trên item active, còn hiệu lực tại valid_at, thuộc scope (L0/L1 + L2/L3 của nhà xe):
//
//	semantic  pgvector HNSW cosine trên embedding (TEI bge-m3, cùng model với job index.items)
//	keyword   full-text trên tsv (search_text = textnorm: không dấu + bigram) — query qua cùng textnorm
//
// → RRF (k=60) → boost nhân (tầng ±10%, proof_count +5%, độ mới ±5%) → cắt theo max_tokens.
// TEI lỗi/chậm → chỉ còn keyword (Degraded ghi lý do). Graph/temporal arm và rerank: M3.
package recall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/entity"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

const (
	rrfK             = 60
	defaultMaxTokens = 2000
	defaultArmLimit  = 40
	// Ngưỡng cosine của nhánh semantic: dưới mức này coi là không liên quan (bge-m3 cho câu tiếng Việt
	// khác chủ đề thường ~0.3–0.45). Không có ngưỡng thì nhánh semantic luôn trả đủ N item dù vô nghĩa.
	minSimilarity = 0.45
	embedTimeout  = 800 * time.Millisecond
	// Nhánh keyword: giữ item có ts_rank_cd ≥ 25% item khớp nhất.
	minKeywordRatio = 0.25
)

var ErrEmptyQuery = errors.New("cần query hoặc topics")

type Query struct {
	OperatorID string
	Text       string
	Topics     []string
	Kinds      []string
	ValidAt    time.Time // đầu khoảng hiệu lực; zero = bây giờ
	ValidUntil time.Time // cuối khoảng (vd hết ngày đi); zero = ValidAt. Item còn hiệu lực ở bất kỳ lúc nào trong khoảng
	MaxTokens  int       // ngân sách token của kết quả, mặc định 2000
	ArmLimit   int       // số ứng viên mỗi nhánh, mặc định 40
}

// Source: nguồn gốc của item — document (tin Zalo, Excel...) hoặc file kb/ với L0/L1.
type Source struct {
	DocumentID string     `json:"document_id,omitempty"`
	Channel    string     `json:"channel,omitempty"`
	ReceivedAt *time.Time `json:"received_at,omitempty"`
	KBFile     string     `json:"kb_file,omitempty"`
}

type Hit struct {
	ID        string            `json:"id"`
	Layer     string            `json:"layer"` // L0 · L1 · L2 · L3
	Label     string            `json:"label"` // nhãn bắt buộc khi dùng trong artifact (vd "thông lệ chung")
	Locked    bool              `json:"locked,omitempty"`
	Kind      string            `json:"kind"`
	Topic     string            `json:"topic"`
	Key       string            `json:"key,omitempty"`
	Text      string            `json:"text"`
	Facts     map[string]string `json:"facts,omitempty"`
	ValidFrom *time.Time        `json:"valid_from,omitempty"`
	ValidTo   *time.Time        `json:"valid_to,omitempty"`
	Source    *Source           `json:"source,omitempty"`
	Score     float64           `json:"score"`
	Arms      []string          `json:"arms,omitempty"` // nhánh tìm ra: semantic, keyword, temporal, graph, browse

	proof     int
	createdAt time.Time
}

type Result struct {
	Hits      []Hit    `json:"items"`
	Total     int      `json:"total_candidates"`    // số ứng viên trước khi cắt theo max_tokens
	Truncated bool     `json:"truncated,omitempty"` // còn ứng viên bị cắt vì ngân sách token
	Degraded  []string `json:"degraded,omitempty"`  // nhánh không chạy được (vd semantic khi TEI lỗi)
	TookMS    int64    `json:"took_ms"`
}

type Recaller struct {
	DB       *pgxpool.Pool
	Embedder Embedder         // nil = chỉ keyword
	Entities *entity.Resolver // nil = không mở rộng alias

	once      sync.Once
	iterative bool
}

type armHit struct {
	id   string
	rank int
}

// filter chung của mọi nhánh: $1 operator, $2 đầu khoảng, $3 topics, $4 kinds, $5 cuối khoảng.
// L1 hiện chỉ có một ngành (xe-khach); khi có nhiều ngành thì lọc theo ngành của nhà xe.
const scopeFilter = `status = 'active'
	AND (layer <= 1 OR operator_id = $1)
	AND (valid_from IS NULL OR valid_from <= $5) AND (valid_to IS NULL OR valid_to >= $2)
	AND (cardinality($3::text[]) = 0 OR topic = ANY($3))
	AND (cardinality($4::text[]) = 0 OR kind = ANY($4))`

func (r *Recaller) Recall(ctx context.Context, q Query) (Result, error) {
	start := time.Now()
	if q.ValidAt.IsZero() {
		q.ValidAt = start
	}
	if q.ValidUntil.Before(q.ValidAt) {
		q.ValidUntil = q.ValidAt
	}
	if q.MaxTokens <= 0 {
		q.MaxTokens = defaultMaxTokens
	}
	if q.ArmLimit <= 0 {
		q.ArmLimit = defaultArmLimit
	}
	if q.Topics == nil {
		q.Topics = []string{}
	}
	if q.Kinds == nil {
		q.Kinds = []string{}
	}
	text := strings.TrimSpace(q.Text)
	if text == "" && len(q.Topics) == 0 {
		return Result{}, ErrEmptyQuery
	}

	var res Result
	arms := map[string][]armHit{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	run := func(name string, f func() ([]armHit, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hits, err := f()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if name == "semantic" {
					slog.Warn("recall: nhánh semantic lỗi, chỉ dùng keyword", "err", err)
					res.Degraded = append(res.Degraded, "semantic: "+err.Error())
					return
				}
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			arms[name] = hits
		}()
	}

	matched := r.Entities.Find(text)
	if text == "" {
		// Chỉ có topics: liệt kê theo topic (không xếp hạng ngữ nghĩa).
		run("browse", func() ([]armHit, error) { return r.browse(ctx, q) })
	} else {
		tsq := tsQuery(r.expand(text))
		if tsq != "" {
			run("keyword", func() ([]armHit, error) { return r.keyword(ctx, q, tsq) })
			// Temporal arm (S3.1.2): có ngày đi rõ ràng → item có mùa (cửa sổ hiệu lực hẹp chứa
			// ngày đó, vd giá Tết) được thêm phiếu, vượt item quanh năm trong RRF.
			if !q.ValidUntil.Equal(q.ValidAt) {
				run("temporal", func() ([]armHit, error) { return r.temporal(ctx, q, tsq) })
			}
		}
		// Graph arm (S3.1.1): query nhắc thực thể (bến/tỉnh/loại xe) → item cũng nhắc thực thể đó.
		if len(matched) > 0 {
			extIDs := make([]string, len(matched))
			for i, m := range matched {
				extIDs[i] = m.Entity.ID
			}
			run("graph", func() ([]armHit, error) { return r.graph(ctx, q, extIDs) })
		}
		if r.Embedder != nil {
			run("semantic", func() ([]armHit, error) { return r.semantic(ctx, q, text) })
		} else {
			res.Degraded = append(res.Degraded, "semantic: chưa cấu hình TEI (BIVA_TEI_URL)")
		}
	}
	wg.Wait()
	if firstErr != nil {
		return Result{}, firstErr
	}

	scores := map[string]float64{}
	found := map[string][]string{}
	for _, name := range []string{"semantic", "keyword", "temporal", "graph", "browse"} {
		for _, h := range arms[name] {
			scores[h.id] += 1.0 / float64(rrfK+h.rank)
			found[h.id] = append(found[h.id], name)
		}
	}
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	hits, err := r.load(ctx, ids)
	if err != nil {
		return Result{}, err
	}
	for i := range hits {
		h := &hits[i]
		h.Arms = found[h.ID]
		h.Score = roundScore(scores[h.ID] * boost(*h, q.ValidAt))
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})

	res.Total = len(hits)
	used := 0
	for i, h := range hits {
		cost := tokens(h)
		if i > 0 && used+cost > q.MaxTokens {
			res.Truncated = true
			break
		}
		used += cost
		res.Hits = append(res.Hits, h)
	}
	if res.Hits == nil {
		res.Hits = []Hit{}
	}
	res.TookMS = time.Since(start).Milliseconds()
	return res, nil
}

// expand: thêm mọi cách viết của thực thể nhận ra trong query ("SG" → "TP.HCM", "Sài Gòn"…) cho nhánh keyword.
func (r *Recaller) expand(text string) string {
	parts := []string{text}
	for _, m := range r.Entities.Find(text) {
		parts = append(parts, m.Entity.Variants()...)
	}
	return strings.Join(parts, " . ")
}

// tsQuery: token + bigram của textnorm nối bằng OR. Bigram khớp cụm từ nên item chứa đúng cụm được điểm cao hơn.
// Token chỉ gồm [a-z0-9_] nên không cần escape cú pháp tsquery.
func tsQuery(text string) string {
	parts := strings.Fields(textnorm.SearchText(text))
	return strings.Join(parts, " | ")
}

func (r *Recaller) keyword(ctx context.Context, q Query, tsq string) ([]armHit, error) {
	rows, err := r.DB.Query(ctx, `SELECT id::text, ts_rank_cd(tsv, tq) AS rank FROM items, to_tsquery('simple', $6) tq
		WHERE `+scopeFilter+` AND tsv @@ tq
		ORDER BY rank DESC, id LIMIT $7`,
		q.OperatorID, q.ValidAt, q.Topics, q.Kinds, q.ValidUntil, tsq, q.ArmLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Query OR nên item chỉ chung một từ phổ biến ("vé", "ngày") cũng khớp: bỏ đuôi dưới minKeywordRatio × hạng đầu.
	var out []armHit
	var top float32
	for rows.Next() {
		var id string
		var rank float32
		if err := rows.Scan(&id, &rank); err != nil {
			return nil, err
		}
		if len(out) == 0 {
			top = rank
		} else if rank < top*minKeywordRatio {
			break
		}
		out = append(out, armHit{id: id, rank: len(out) + 1})
	}
	return out, rows.Err()
}

func (r *Recaller) semantic(ctx context.Context, q Query, text string) ([]armHit, error) {
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	vec, err := r.Embedder.Embed(ectx, text)
	if err != nil {
		return nil, err
	}
	// HNSW lọc SAU khi lấy ef_search ứng viên gần nhất toàn bảng: nhiều nhà xe thì ứng viên của đúng nhà xe bị
	// thiếu. Tăng ef_search; pgvector ≥ 0.8 thì bật iterative scan (quét tiếp tới khi đủ LIMIT sau lọc).
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	settings := `SELECT set_config('hnsw.ef_search', '200', true)`
	if r.iterativeScan(ctx) {
		settings += `, set_config('hnsw.iterative_scan', 'relaxed_order', true)`
	}
	if _, err := tx.Exec(ctx, settings); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM (
			SELECT id, embedding <=> $6::vector AS dist FROM items
			WHERE `+scopeFilter+` AND embedding IS NOT NULL
			ORDER BY embedding <=> $6::vector LIMIT $7) c
		WHERE dist <= $8 ORDER BY dist, id`,
		q.OperatorID, q.ValidAt, q.Topics, q.Kinds, q.ValidUntil, pgvector(vec), q.ArmLimit, 1-minSimilarity)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

// iterativeScan: pgvector ≥ 0.8 có hnsw.iterative_scan (kiểm một lần).
func (r *Recaller) iterativeScan(ctx context.Context) bool {
	r.once.Do(func() {
		var v string
		if err := r.DB.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&v); err == nil {
			var major, minor int
			fmt.Sscanf(v, "%d.%d", &major, &minor)
			r.iterative = major > 0 || minor >= 8
		}
	})
	return r.iterative
}

// temporal: item có cửa sổ hiệu lực (mùa) chứa ngày đi, khớp text, xếp theo cửa sổ hẹp nhất trước —
// "giá 28 Tết" phải thắng "giá thường" vốn hiệu lực quanh năm.
func (r *Recaller) temporal(ctx context.Context, q Query, tsq string) ([]armHit, error) {
	rows, err := r.DB.Query(ctx, `SELECT id::text FROM items, to_tsquery('simple', $6) tq
		WHERE `+scopeFilter+` AND tsv @@ tq
		  AND valid_from IS NOT NULL AND valid_to IS NOT NULL
		ORDER BY (valid_to - valid_from), ts_rank_cd(tsv, tq) DESC, id LIMIT $7`,
		q.OperatorID, q.ValidAt, q.Topics, q.Kinds, q.ValidUntil, tsq, q.ArmLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []armHit
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, armHit{id: id, rank: len(out) + 1})
	}
	return out, rows.Err()
}

// graphScopeFilter: scopeFilter với prefix i. (join cùng entities có cột trùng tên).
const graphScopeFilter = `i.status = 'active'
	AND (i.layer <= 1 OR i.operator_id = $1)
	AND (i.valid_from IS NULL OR i.valid_from <= $5) AND (i.valid_to IS NULL OR i.valid_to >= $2)
	AND (cardinality($3::text[]) = 0 OR i.topic = ANY($3))
	AND (cardinality($4::text[]) = 0 OR i.kind = ANY($4))`

// graph: item nhắc cùng thực thể với query (qua item_entities; entity theo ext_id của kb/).
func (r *Recaller) graph(ctx context.Context, q Query, extIDs []string) ([]armHit, error) {
	rows, err := r.DB.Query(ctx, `SELECT ie.item_id::text, count(DISTINCT e.id) AS n
		FROM item_entities ie
		JOIN entities e ON e.id = ie.entity_id
		JOIN items i ON i.id = ie.item_id AND `+graphScopeFilter+`
		WHERE e.ext_id = ANY($7::text[])
		GROUP BY ie.item_id ORDER BY n DESC, ie.item_id LIMIT $6`,
		q.OperatorID, q.ValidAt, q.Topics, q.Kinds, q.ValidUntil, q.ArmLimit, extIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []armHit
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out = append(out, armHit{id: id, rank: len(out) + 1})
	}
	return out, rows.Err()
}

func (r *Recaller) browse(ctx context.Context, q Query) ([]armHit, error) {
	// Tầng cao trước (nhà xe > ngành > nền tảng), rồi theo key.
	rows, err := r.DB.Query(ctx, `SELECT id::text FROM items WHERE `+scopeFilter+`
		ORDER BY layer DESC, topic, key NULLS LAST, id LIMIT $6`,
		q.OperatorID, q.ValidAt, q.Topics, q.Kinds, q.ValidUntil, q.ArmLimit*3)
	if err != nil {
		return nil, err
	}
	return collectIDs(rows)
}

type idRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

func collectIDs(rows idRows) ([]armHit, error) {
	defer rows.Close()
	var out []armHit
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, armHit{id: id, rank: len(out) + 1})
	}
	return out, rows.Err()
}

func (r *Recaller) load(ctx context.Context, ids []string) ([]Hit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.DB.Query(ctx, `SELECT i.id::text, i.layer, i.locked, i.kind, i.topic, i.key, i.text, i.value,
			i.valid_from, i.valid_to, i.proof_count, i.created_at, i.metadata,
			d.id::text, d.source, d.received_at
		FROM items i LEFT JOIN documents d ON d.id = i.document_id
		WHERE i.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var layer int16
		var key, docID, channel *string
		var received *time.Time
		var value, meta []byte
		if err := rows.Scan(&h.ID, &layer, &h.Locked, &h.Kind, &h.Topic, &key, &h.Text, &value,
			&h.ValidFrom, &h.ValidTo, &h.proof, &h.createdAt, &meta, &docID, &channel, &received); err != nil {
			return nil, err
		}
		if key != nil {
			h.Key = *key
		}
		h.Facts = Facts(value)
		var m struct {
			Source string `json:"source"`
			Label  string `json:"label"`
		}
		_ = json.Unmarshal(meta, &m)
		h.Layer, h.Label = LayerLabel(int(layer), h.Locked, m.Label)
		switch {
		case docID != nil:
			h.Source = &Source{DocumentID: *docID, Channel: deref(channel), ReceivedAt: received}
		case m.Source != "":
			h.Source = &Source{KBFile: m.Source}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Facts đọc value.facts (map chuỗi) của item; giá trị không phải chuỗi được giữ dạng JSON.
func Facts(value []byte) map[string]string {
	var v struct {
		Facts map[string]any `json:"facts"`
	}
	if json.Unmarshal(value, &v) != nil || len(v.Facts) == 0 {
		return nil
	}
	out := make(map[string]string, len(v.Facts))
	for k, x := range v.Facts {
		if s, ok := x.(string); ok {
			out[k] = s
		} else {
			b, _ := json.Marshal(x)
			out[k] = string(b)
		}
	}
	return out
}

// LayerLabel: nhãn tầng + nhãn hiển thị bắt buộc khi dùng trong artifact.
func LayerLabel(layer int, locked bool, metaLabel string) (string, string) {
	switch layer {
	case 0:
		return "L0", "quy tắc nền tảng (bắt buộc)"
	case 1:
		if locked {
			return "L1", "quy tắc ngành (bắt buộc)"
		}
		if metaLabel == "" {
			metaLabel = "thông lệ chung"
		}
		return "L1", metaLabel
	case 3:
		return "L3", "riêng bot"
	default:
		return "L2", "nhà xe"
	}
}

// boost nhân: tầng (L3 > L2 > L1 > L0) ±10%, proof_count tới +5%, độ mới ±5% (theo 365 ngày).
func boost(h Hit, at time.Time) float64 {
	layer := map[string]float64{"L0": 0.90, "L1": 0.95, "L2": 1.05, "L3": 1.10}[h.Layer]
	proof := 1 + 0.05*math.Min(float64(h.proof-1), 4)/4
	ref := h.createdAt
	if h.ValidFrom != nil && h.ValidFrom.After(ref) {
		ref = *h.ValidFrom
	}
	age := math.Min(math.Max(at.Sub(ref).Hours()/24, 0), 365)
	recency := 1.05 - 0.10*age/365
	return layer * proof * recency
}

// tokens ước lượng thô số token của một hit trong output (tiếng Việt ~3 ký tự/token + phần khung JSON).
func tokens(h Hit) int {
	n := utf8.RuneCountInString(h.Text) + len(h.Key) + len(h.Topic)
	for k, v := range h.Facts {
		n += len(k) + utf8.RuneCountInString(v)
	}
	return n/3 + 40
}

func roundScore(x float64) float64 { return math.Round(x*1e6) / 1e6 }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
