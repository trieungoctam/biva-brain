package release

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestGateBlocksAndPasses(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	op := "rl" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'R')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	bot := op + ":zalo"
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`, bot, op); err != nil {
		t.Fatal(err)
	}
	req := []string{"system_prompt", "faq"}
	topics := []string{"fare", "pets"}

	// Chưa có gì: chặn với lý do cụ thể từng mục.
	rep, err := Gate(ctx, pool, op, "zalo", req, topics)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed || len(rep.Blocked) < 4 {
		t.Fatalf("phải chặn đủ các nhóm: %+v", rep)
	}
	join := func(ss []string) string {
		out := ""
		for _, s := range ss {
			out += s
		}
		return out
	}
	all := join(rep.Blocked)
	for _, want := range []string{"system_prompt: chưa có", "coverage", "snapshot", "chưa chạy test"} {
		if !contains(all, want) {
			t.Fatalf("thiếu lý do %q trong %v", want, rep.Blocked)
		}
	}

	// Đủ điều kiện: artifact valid (2 kind), item phủ 2 topic, snapshot, test 1/1 pass.
	for _, kind := range req {
		if _, err := pool.Exec(ctx, `INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content,
			content_hash, status, author) VALUES ($1, $2, $3, 1, 'x', $3, 'valid', 't')`, bot, op, kind); err != nil {
			t.Fatal(err)
		}
	}
	for _, topic := range topics {
		if _, err := pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
			VALUES (2, $1, 'policy', $2, 'nội dung', 'active')`, op, topic); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO snapshots (bot_id, operator_id, version, artifact_versions,
		definition, created_by) VALUES ($1, $2, 1, '{}', '{}', 't')`, bot, op); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO test_runs (operator_id, bot_channel, total, passed, report)
		VALUES ($1, 'zalo', 1, 1, '[]')`, op); err != nil {
		t.Fatal(err)
	}
	rep, err = Gate(ctx, pool, op, "zalo", req, topics)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		t.Fatalf("phải đạt: %+v", rep)
	}

	// Artifact stale → chặn với hướng dẫn refresh.
	if _, err := pool.Exec(ctx, `UPDATE bot_artifacts SET status = 'stale' WHERE bot_id = $1 AND kind = 'faq'`, bot); err != nil {
		t.Fatal(err)
	}
	rep, err = Gate(ctx, pool, op, "zalo", req, topics)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed || !contains(join(rep.Blocked), "stale") {
		t.Fatalf("stale phải chặn: %+v", rep)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
