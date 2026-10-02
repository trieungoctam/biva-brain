package release

import (
	"context"
	"fmt"
	"sync"
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
	for _, want := range []string{"system_prompt: chưa có", "coverage", "snapshot", "chưa test được"} {
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
	var snapID string
	if err := pool.QueryRow(ctx, `INSERT INTO snapshots (bot_id, operator_id, version, artifact_versions,
		definition, created_by) VALUES ($1, $2, 1, '{}', '{}', 't') RETURNING id::text`,
		bot, op).Scan(&snapID); err != nil {
		t.Fatal(err)
	}
	// Test run phải gắn đúng snapshot mới nhất — gate không nhận kết quả của snapshot cũ.
	if _, err := pool.Exec(ctx, `INSERT INTO test_runs (operator_id, bot_channel, snapshot_id, total, passed, report)
		VALUES ($1, 'zalo', $2::uuid, 1, 1, '[]')`, op, snapID); err != nil {
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

// S4.1.2: staging tự động, production chờ duyệt, rollback < 1 phút.
func TestPublishApproveRollback(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	op := "pb" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'P')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	bot := op + ":zalo"
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`, bot, op); err != nil {
		t.Fatal(err)
	}
	req, topics := []string{"faq"}, []string{"fare"}
	seed := func(ver int) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO snapshots (bot_id, operator_id, version, artifact_versions,
			definition, created_by) VALUES ($1, $2, $3, '{}', '{}', 't') RETURNING id::text`,
			bot, op, ver).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content,
			content_hash, status, author) VALUES ($1, $2, 'faq', $3, 'x', 'h', 'valid', 't')`,
			bot, op, ver); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, err := pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
		VALUES (2, $1, 'policy', 'fare', 'nội dung', 'active')`, op); err != nil {
		t.Fatal(err)
	}
	// Gate chặn khi thiếu (chưa có artifact/snapshot).
	if _, _, err := Publish(ctx, pool, op, "zalo", "staging", "t", req, topics); err == nil ||
		!contains(err.Error(), "gate") {
		t.Fatalf("phải chặn vì gate: %v", err)
	}

	seed(1)
	// Gate chỉ nhận test run gắn đúng snapshot sẽ phát hành (S4.1.1): seed trước, test sau.
	if _, err := pool.Exec(ctx, `INSERT INTO test_runs (operator_id, bot_channel, snapshot_id, total, passed, report)
		SELECT $1, 'zalo', id::uuid, 1, 1, '[]' FROM snapshots
		WHERE bot_id = $2 ORDER BY version DESC LIMIT 1`, op, bot); err != nil {
		t.Fatal(err)
	}
	// Staging: published ngay.
	_, st, err := Publish(ctx, pool, op, "zalo", "staging", "t", req, topics)
	if err != nil || st != "published" {
		t.Fatalf("staging = %s %v", st, err)
	}
	// Production: requested, chờ duyệt.
	id2, st, err := Publish(ctx, pool, op, "zalo", "production", "t", req, topics)
	if err != nil || st != "requested" {
		t.Fatalf("production = %s %v", st, err)
	}
	// Duyệt → published.
	if _, err := Approve(ctx, pool, id2, "user:lead"); err != nil {
		t.Fatal(err)
	}

	// Bản production thứ 2: snapshot v2 + test lại cho đúng bản đó, duyệt → bản 1 rolled_back.
	seed(2)
	if _, err := pool.Exec(ctx, `INSERT INTO test_runs (operator_id, bot_channel, snapshot_id, total, passed, report)
		SELECT $1, 'zalo', id::uuid, 1, 1, '[]' FROM snapshots
		WHERE bot_id = $2 ORDER BY version DESC LIMIT 1`, op, bot); err != nil {
		t.Fatal(err)
	}
	id3, st, _ := Publish(ctx, pool, op, "zalo", "production", "t", req, topics)
	if st != "requested" {
		t.Fatalf("st = %s", st)
	}
	if _, err := Approve(ctx, pool, id3, "user:lead"); err != nil {
		t.Fatal(err)
	}
	var old1 string
	pool.QueryRow(ctx, `SELECT status FROM releases WHERE id = $1::uuid`, id2).Scan(&old1)
	if old1 != "rolled_back" {
		t.Fatalf("bản cũ phải rolled_back: %s", old1)
	}

	// Rollback: bản 2 (v2) → rolled_back, bản id2 published lại.
	start := time.Now()
	prev, ver, err := Rollback(ctx, pool, op, "zalo", "user:lead")
	if err != nil || prev != id2 || ver != 2 {
		t.Fatalf("rollback = %s v%d %v", prev, ver, err)
	}
	if d := time.Since(start); d > time.Minute {
		t.Fatalf("rollback mất %v", d)
	}
	var now1, now3 string
	pool.QueryRow(ctx, `SELECT status FROM releases WHERE id = $1::uuid`, id2).Scan(&now1)
	pool.QueryRow(ctx, `SELECT status FROM releases WHERE id = $1::uuid`, id3).Scan(&now3)
	if now1 != "published" || now3 != "rolled_back" {
		t.Fatalf("sau rollback: %s/%s", now1, now3)
	}
}

// Concurrency: hai Publish staging cùng (operator, kênh) chạy song song thật — advisory lock
// phải serialize để KẾT QUẢ luôn còn đúng MỘT bản published (round 4; trước đây 3 lệnh
// autocommit rời có thể kết thúc cả hai rolled_back).
func TestPublishConcurrentExactlyOnePublished(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	op := "pc" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'P')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	bot := op + ":zalo"
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`, bot, op); err != nil {
		t.Fatal(err)
	}
	req, topics := []string{"faq"}, []string{"fare"}
	var snapID string
	if err := pool.QueryRow(ctx, `INSERT INTO snapshots (bot_id, operator_id, version, artifact_versions,
		definition, created_by) VALUES ($1, $2, 1, '{}', '{}', 't') RETURNING id::text`, bot, op).Scan(&snapID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content,
		content_hash, status, author) VALUES ($1, $2, 'faq', 1, 'x', 'h', 'valid', 't')`, bot, op); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
		VALUES (2, $1, 'policy', 'fare', 'nội dung', 'active')`, op); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO test_runs (operator_id, bot_channel, snapshot_id, total, passed, report)
		VALUES ($1, 'zalo', $2::uuid, 1, 1, '[]')`, op, snapID); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := Publish(ctx, pool, op, "zalo", "staging", "t", req, topics)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var published, rolled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='published'),
		count(*) FILTER (WHERE status='rolled_back') FROM releases WHERE operator_id=$1`, op).
		Scan(&published, &rolled); err != nil {
		t.Fatal(err)
	}
	if published != 1 || rolled != n-1 {
		t.Fatalf("sau %d publish song song: published=%d rolled_back=%d, muốn 1/%d", n, published, rolled, n-1)
	}
}
