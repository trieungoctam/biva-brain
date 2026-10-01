// Package testdb cấp pool Postgres cho test cần DB thật.
//
// Cần BIVA_TEST_DATABASE_URL (DB dùng riêng cho test); thiếu thì test được skip.
// Lần đầu trong mỗi process test, migration được chạy tới bản mới nhất — test không phụ thuộc
// thứ tự chạy giữa các package.
package testdb

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/migrate"
)

var (
	once       sync.Once
	migrateErr error
)

// URL trả về BIVA_TEST_DATABASE_URL hoặc skip test.
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("BIVA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("đặt BIVA_TEST_DATABASE_URL để chạy test với Postgres thật")
	}
	return url
}

// MigrationsDir: contracts/migrations tính từ vị trí file này (không phụ thuộc thư mục chạy test).
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "migrations")
}

// Pool trả về pool tới DB test đã migrate; tự đóng khi test xong.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := URL(t)
	once.Do(func() { migrateErr = migrate.Up(MigrationsDir(), url) })
	if migrateErr != nil {
		t.Fatalf("migrate DB test: %v", migrateErr)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
