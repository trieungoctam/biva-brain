package recall

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/entity"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

// DataQuery: tra data vận hành của nhà xe (tuyến, chuyến, giá, điểm đón...) có hiệu lực tại một ngày.
// Khác recall: lọc chính xác (topic, từ khoá phải có đủ, facts khớp), không xếp hạng — để AI lấy con số đúng.
type DataQuery struct {
	OperatorID      string
	Topics          []string
	Match           string            // mọi từ phải có trong key/text/facts (không phân biệt dấu)
	Facts           map[string]string // facts phải chứa giá trị này (không phân biệt dấu), vd {diem_di: "sài gòn"}
	At              time.Time         // đầu khoảng tra; zero = bây giờ
	Until           time.Time         // cuối khoảng (vd hết ngày đi); zero = At
	IncludeUpcoming bool              // thêm bản sẽ có hiệu lực sau At (vd giá Tết đã chốt)
	Limit           int
	Entities        *entity.Resolver // "SG" khớp "Sài Gòn"; nil = so token thường
}

type DataRow struct {
	ID        string            `json:"id"`
	Topic     string            `json:"topic"`
	Key       string            `json:"key,omitempty"`
	Text      string            `json:"text"`
	Facts     map[string]string `json:"facts,omitempty"`
	ValidFrom *time.Time        `json:"valid_from,omitempty"`
	ValidTo   *time.Time        `json:"valid_to,omitempty"`
}

type DataResult struct {
	At        time.Time `json:"as_of"`
	Rows      []DataRow `json:"items"`
	Upcoming  []DataRow `json:"upcoming,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

// maxScan: số item data tối đa đọc từ DB trước khi lọc từ khoá/facts trong Go (data một nhà xe cỡ vài trăm).
const maxScan = 2000

func QueryData(ctx context.Context, db *pgxpool.Pool, q DataQuery) (DataResult, error) {
	if q.At.IsZero() {
		q.At = time.Now()
	}
	if q.Until.Before(q.At) {
		q.Until = q.At
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	if q.Topics == nil {
		q.Topics = []string{}
	}
	res := DataResult{At: q.At, Rows: []DataRow{}}
	rows, err := db.Query(ctx, `SELECT id::text, topic, key, text, value, valid_from, valid_to,
			(valid_from IS NOT NULL AND valid_from > $6) AS upcoming
		FROM items
		WHERE operator_id = $1 AND layer = 2 AND kind = 'data' AND status = 'active'
			AND (valid_to IS NULL OR valid_to >= $2)
			AND ($4 OR valid_from IS NULL OR valid_from <= $6)
			AND (cardinality($3::text[]) = 0 OR topic = ANY($3))
		ORDER BY topic, key NULLS LAST, valid_from NULLS FIRST, id LIMIT $5`,
		q.OperatorID, q.At, q.Topics, q.IncludeUpcoming, maxScan, q.Until)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	words := q.Entities.QueryTerms(q.Match) // resolver nil → token thường
	for rows.Next() {
		var r DataRow
		var key *string
		var value []byte
		var upcoming bool
		if err := rows.Scan(&r.ID, &r.Topic, &key, &r.Text, &value, &r.ValidFrom, &r.ValidTo, &upcoming); err != nil {
			return res, err
		}
		r.Key = deref(key)
		r.Facts = Facts(value)
		if !matchWords(r, words, q.Entities) || !matchFacts(r.Facts, q.Facts, q.Entities) {
			continue
		}
		if len(res.Rows)+len(res.Upcoming) >= q.Limit {
			res.Truncated = true
			break
		}
		if upcoming {
			res.Upcoming = append(res.Upcoming, r)
		} else {
			res.Rows = append(res.Rows, r)
		}
	}
	return res, rows.Err()
}

func matchWords(r DataRow, words []string, ents *entity.Resolver) bool {
	if len(words) == 0 {
		return true
	}
	parts := []string{strings.ReplaceAll(r.Key, "_", " "), r.Text}
	for k, v := range r.Facts {
		parts = append(parts, strings.ReplaceAll(k, "_", " "), v)
	}
	have := map[string]bool{}
	for _, p := range parts {
		for _, t := range ents.Terms(p) {
			have[t] = true
		}
	}
	for _, w := range words {
		if !have[w] {
			return false
		}
	}
	return true
}

// matchFacts: mỗi điều kiện {tên: giá trị} khớp khi fact cùng tên (chuẩn hoá) chứa giá trị (trọn từ, hoặc cùng
// thực thể: "SG" khớp "Sài Gòn").
func matchFacts(facts, want map[string]string, ents *entity.Resolver) bool {
	for wk, wv := range want {
		wk = textnorm.Fold(strings.ReplaceAll(wk, "_", " "))
		ok := false
		for k, v := range facts {
			if textnorm.Fold(strings.ReplaceAll(k, "_", " ")) != wk {
				continue
			}
			if containsWords(textnorm.Fold(v), textnorm.Fold(wv)) || sameEntities(ents, v, wv) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// sameEntities: giá trị cần tìm là (các) thực thể và fact nhắc tới đủ các thực thể đó.
func sameEntities(ents *entity.Resolver, have, want string) bool {
	wm := ents.Find(want)
	if len(wm) == 0 {
		return false
	}
	hm := map[string]bool{}
	for _, m := range ents.Find(have) {
		hm[m.Entity.ID] = true
	}
	for _, m := range wm {
		if !hm[m.Entity.ID] {
			return false
		}
	}
	return true
}

// containsWords: s chứa cụm sub trọn từ ("sai gon" khớp "tp sai gon", không khớp "sai gonx").
func containsWords(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(" "+s+" ", " "+sub+" ")
}
