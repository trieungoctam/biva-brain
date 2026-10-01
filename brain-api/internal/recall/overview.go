package recall

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TopicSpec: một mục của template ngành (kb/L1/<ngành>/template.yaml).
type TopicSpec struct {
	ID       string
	Title    string
	Required bool
}

type TopicStatus struct {
	Topic  string `json:"topic"`
	Title  string `json:"title"`
	Active int    `json:"active"` // số item active của nhà xe (L2)
}

type Overview struct {
	Operator struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"operator"`
	Items      map[string]int `json:"items_by_status"` // item L2 theo trạng thái
	LastUpdate *time.Time     `json:"last_update,omitempty"`
	Coverage   struct {
		Required        []TopicStatus `json:"required"`
		MissingRequired []string      `json:"missing_required"`
		Recommended     []TopicStatus `json:"recommended"`
		Other           int           `json:"other_active"` // item topic other / ngoài template
	} `json:"coverage"`
	Reviews     map[string]int `json:"open_reviews"` // theo rủi ro: high, low
	Jobs        []JobCount     `json:"jobs"`         // job chưa xong + lỗi trong 24h
	NextActions []string       `json:"next_actions"`
}

type JobCount struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

func GetOverview(ctx context.Context, db *pgxpool.Pool, operatorID string, topics []TopicSpec) (Overview, error) {
	var ov Overview
	o := &ov.Operator
	err := db.QueryRow(ctx, `SELECT id, name, status, created_at FROM operators WHERE id = $1`, operatorID).
		Scan(&o.ID, &o.Name, &o.Status, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ov, ErrNotFound
	}
	if err != nil {
		return ov, err
	}

	ov.Items = map[string]int{}
	if err := countInto(ctx, db, ov.Items, `SELECT status, count(*) FROM items
		WHERE operator_id = $1 AND layer = 2 GROUP BY status`, operatorID); err != nil {
		return ov, err
	}
	if err := db.QueryRow(ctx, `SELECT max(updated_at) FROM items WHERE operator_id = $1 AND layer = 2`,
		operatorID).Scan(&ov.LastUpdate); err != nil {
		return ov, err
	}

	byTopic := map[string]int{}
	if err := countInto(ctx, db, byTopic, `SELECT topic, count(*) FROM items
		WHERE operator_id = $1 AND layer = 2 AND status = 'active' GROUP BY topic`, operatorID); err != nil {
		return ov, err
	}
	c := &ov.Coverage
	c.Required, c.MissingRequired, c.Recommended = []TopicStatus{}, []string{}, []TopicStatus{}
	known := map[string]bool{}
	for _, t := range topics {
		known[t.ID] = true
		st := TopicStatus{Topic: t.ID, Title: t.Title, Active: byTopic[t.ID]}
		if t.Required {
			c.Required = append(c.Required, st)
			if st.Active == 0 {
				c.MissingRequired = append(c.MissingRequired, t.ID)
			}
		} else {
			c.Recommended = append(c.Recommended, st)
		}
	}
	for topic, n := range byTopic {
		if !known[topic] {
			c.Other += n
		}
	}

	ov.Reviews = map[string]int{"high": 0, "low": 0}
	if err := countInto(ctx, db, ov.Reviews, `SELECT risk, count(*) FROM review_items
		WHERE operator_id = $1 AND status = 'open' GROUP BY risk`, operatorID); err != nil {
		return ov, err
	}

	ov.Jobs = []JobCount{}
	rows, err := db.Query(ctx, `SELECT kind, status, count(*) FROM operations
		WHERE operator_id = $1 AND (status IN ('queued', 'running')
			OR (status = 'failed' AND updated_at > now() - interval '24 hours'))
		GROUP BY kind, status ORDER BY kind, status`, operatorID)
	if err != nil {
		return ov, err
	}
	defer rows.Close()
	for rows.Next() {
		var j JobCount
		if err := rows.Scan(&j.Kind, &j.Status, &j.Count); err != nil {
			return ov, err
		}
		ov.Jobs = append(ov.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return ov, err
	}

	ov.NextActions = []string{}
	if n := ov.Reviews["high"] + ov.Reviews["low"]; n > 0 {
		ov.NextActions = append(ov.NextActions, "list_review_queue — còn đề xuất chờ duyệt")
	}
	if len(c.MissingRequired) > 0 {
		ov.NextActions = append(ov.NextActions, "hỏi nhà xe các mục bắt buộc còn thiếu (missing_required), "+
			"rồi gửi bằng submit_knowledge")
	}
	ov.NextActions = append(ov.NextActions, "recall_knowledge / query_data để đọc tri thức khi viết bot")
	return ov, nil
}

func countInto(ctx context.Context, db *pgxpool.Pool, out map[string]int, sql string, args ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		out[k] = n
	}
	return rows.Err()
}
