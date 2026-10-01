package migrate

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestToPgx5URL(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@h:5432/db": "pgx5://u:p@h:5432/db",
		"postgresql://u@h/db?x=1":  "pgx5://u@h/db?x=1",
		"pgx5://already/converted": "pgx5://already/converted",
	}
	for in, want := range cases {
		if got := toPgx5URL(in); got != want {
			t.Errorf("toPgx5URL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUpDownUp chạy migration thật khi có BIVA_TEST_DATABASE_URL (DB trống, dùng riêng cho test).
func TestUpDownUp(t *testing.T) {
	url := os.Getenv("BIVA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("đặt BIVA_TEST_DATABASE_URL để chạy test migration với Postgres thật")
	}
	dir := "../../../contracts/migrations"
	for i, step := range []func(string, string) error{Up, Down, Up} {
		if err := step(dir, url); err != nil {
			t.Fatalf("bước %d: %v", i, err)
		}
	}
	v, dirty, err := Version(dir, url)
	if err != nil || dirty || v != 18 {
		t.Fatalf("version = %d dirty=%v err=%v", v, dirty, err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, table := range []string{"operators", "bots", "documents", "items", "entities", "operations", "audit_log", "users", "user_operators", "api_tokens", "llm_usage", "review_items", "confirm_tokens"} {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&ok); err != nil || !ok {
			t.Errorf("thiếu bảng %s (err=%v)", table, err)
		}
	}
	// Ràng buộc tầng ↔ scope: L2 bắt buộc có operator_id.
	if _, err := conn.Exec(ctx, `INSERT INTO items (layer, kind, topic, text) VALUES (2, 'policy', 'pets', 'x')`); err == nil {
		t.Error("item L2 không có operator_id lẽ ra phải bị từ chối")
	}
	// locked chỉ cho L0/L1.
	if _, err := conn.Exec(ctx, `INSERT INTO operators (id, name) VALUES ('t_op', 'T') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, `DELETE FROM operators WHERE id = 't_op'`) // chạy trước conn.Close (LIFO)
	if _, err := conn.Exec(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, locked) VALUES (2, 't_op', 'policy', 'pets', 'x', true)`); err == nil {
		t.Error("item L2 locked lẽ ra phải bị từ chối")
	}
}
