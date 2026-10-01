package scheduler

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("BIVA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("đặt BIVA_TEST_DATABASE_URL để chạy test scheduler với Postgres thật")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Hai instance cùng chạy → đúng một leader; leader dừng → instance kia lên thay.
func TestSingleLeader(t *testing.T) {
	var leaders atomic.Int32
	var maxLeaders atomic.Int32
	track := func(v bool) {
		n := leaders.Add(map[bool]int32{true: 1, false: -1}[v])
		for {
			m := maxLeaders.Load()
			if n <= m || maxLeaders.CompareAndSwap(m, n) {
				break
			}
		}
	}

	var ran [2]atomic.Int32
	newSched := func(i int) *Scheduler {
		return &Scheduler{
			DB:         testPool(t),
			RetryEvery: 50 * time.Millisecond,
			OnLeader:   track,
			Tasks: []Task{{Name: "count", Every: 20 * time.Millisecond, Run: func(context.Context, *pgxpool.Pool) error {
				ran[i].Add(1)
				return nil
			}}},
		}
	}

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	defer stopB()
	var wg sync.WaitGroup
	for i, ctx := range []context.Context{ctxA, ctxB} {
		wg.Add(1)
		go func() { defer wg.Done(); newSched(i).Run(ctx) }()
	}

	time.Sleep(500 * time.Millisecond)
	if maxLeaders.Load() != 1 || leaders.Load() != 1 {
		t.Fatalf("leaders=%d max=%d, muốn đúng 1", leaders.Load(), maxLeaders.Load())
	}
	if (ran[0].Load() > 0) == (ran[1].Load() > 0) {
		t.Fatalf("task phải chạy ở đúng một instance: %d / %d", ran[0].Load(), ran[1].Load())
	}

	// Dừng instance đang là leader → instance còn lại lên thay.
	first := 0
	if ran[1].Load() > 0 {
		first = 1
	}
	other := 1 - first
	before := ran[other].Load()
	if first == 0 {
		stopA()
	} else {
		stopB()
	}
	deadline := time.Now().Add(2 * time.Second)
	for ran[other].Load() == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ran[other].Load() == before {
		t.Fatal("instance còn lại không lên làm leader")
	}
	if maxLeaders.Load() != 1 {
		t.Fatalf("có lúc %d leader", maxLeaders.Load())
	}
	stopA()
	stopB()
	wg.Wait()
}

func TestRequeueExpired(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	var retryID, deadID string
	err := pool.QueryRow(ctx, `
		INSERT INTO operations (kind, status, attempts, max_attempts, locked_by, lease_until)
		VALUES ('test.lease', 'running', 1, 3, 'w-dead', now() - interval '1 minute') RETURNING id`).Scan(&retryID)
	if err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO operations (kind, status, attempts, max_attempts, locked_by, lease_until)
		VALUES ('test.lease', 'running', 3, 3, 'w-dead', now() - interval '1 minute') RETURNING id`).Scan(&deadID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operations WHERE id = ANY($1)`, []string{retryID, deadID}) })

	if err := RequeueExpired(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{retryID: "queued", deadID: "failed"} {
		var status string
		var lockedBy *string
		pool.QueryRow(ctx, `SELECT status, locked_by FROM operations WHERE id = $1`, id).Scan(&status, &lockedBy)
		if status != want || lockedBy != nil {
			t.Errorf("job %s: status=%s locked_by=%v, muốn %s/nil", id, status, lockedBy, want)
		}
	}
}
