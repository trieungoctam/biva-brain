package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func testPool(t *testing.T) *pgxpool.Pool { return testdb.Pool(t) }

func TestEnqueueIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	key := "test-idem-" + time.Now().Format(time.RFC3339Nano)
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operations WHERE idempotency_key = $1`, key) })

	// Gọi đồng thời: chỉ một job được tạo, mọi lời gọi nhận cùng id.
	const n = 8
	ids := make([]string, n)
	created := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], created[i], errs[i] = Enqueue(ctx, pool, Job{Kind: "test.noop", IdempotencyKey: key})
		}()
	}
	wg.Wait()

	nCreated := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("lời gọi %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("id khác nhau: %s vs %s", ids[i], ids[0])
		}
		if created[i] {
			nCreated++
		}
	}
	if nCreated != 1 {
		t.Fatalf("created=true ở %d lời gọi, muốn đúng 1", nCreated)
	}
	var count int
	pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE idempotency_key = $1`, key).Scan(&count)
	if count != 1 {
		t.Fatalf("có %d job, muốn 1", count)
	}

	op, err := Get(ctx, pool, ids[0])
	if err != nil || op.Status != "queued" || op.Kind != "test.noop" {
		t.Fatalf("Get = %+v, %v", op, err)
	}
}

func TestEnqueueNotifies(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	conn, err := pgx.Connect(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "LISTEN biva_operations"); err != nil {
		t.Fatal(err)
	}

	id, _, err := Enqueue(ctx, pool, Job{Kind: "test.notify"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operations WHERE id = $1`, id) })

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	n, err := conn.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("không nhận NOTIFY: %v", err)
	}
	if n.Channel != "biva_operations" || n.Payload != "test.notify" {
		t.Fatalf("NOTIFY = %s/%s", n.Channel, n.Payload)
	}
}

func TestGetNotFound(t *testing.T) {
	pool := testPool(t)
	_, err := Get(context.Background(), pool, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, muốn ErrNotFound", err)
	}
}
