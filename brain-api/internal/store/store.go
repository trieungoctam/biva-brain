// Package store giữ các pool kết nối Postgres: primary (ghi, queue) và replica (đọc).
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Primary *pgxpool.Pool
	Replica *pgxpool.Pool
}

// Open mở hai pool. Khi replicaURL trùng primaryURL, cả hai trỏ cùng một pool.
func Open(ctx context.Context, primaryURL, replicaURL string) (*DB, error) {
	primary, err := pgxpool.New(ctx, primaryURL)
	if err != nil {
		return nil, fmt.Errorf("mở pool primary: %w", err)
	}
	db := &DB{Primary: primary, Replica: primary}
	if replicaURL != "" && replicaURL != primaryURL {
		replica, err := pgxpool.New(ctx, replicaURL)
		if err != nil {
			primary.Close()
			return nil, fmt.Errorf("mở pool replica: %w", err)
		}
		db.Replica = replica
	}
	return db, nil
}

func (db *DB) Close() {
	if db.Replica != db.Primary {
		db.Replica.Close()
	}
	db.Primary.Close()
}

// Check kiểm tra cả hai pool; dùng cho /health/ready.
func (db *DB) Check(ctx context.Context) map[string]error {
	res := map[string]error{"primary": db.Primary.Ping(ctx)}
	if db.Replica != db.Primary {
		res["replica"] = db.Replica.Ping(ctx)
	}
	return res
}
