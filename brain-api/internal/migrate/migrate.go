// Package migrate chạy SQL migration trong contracts/migrations bằng golang-migrate.
package migrate

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // driver pgx5://
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func newMigrator(dir, databaseURL string) (*migrate.Migrate, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return migrate.New("file://"+abs, toPgx5URL(databaseURL))
}

// toPgx5URL đổi postgres:// thành pgx5:// mà driver của golang-migrate yêu cầu.
func toPgx5URL(u string) string {
	for _, p := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(u, p) {
			return "pgx5://" + strings.TrimPrefix(u, p)
		}
	}
	return u
}

func Up(dir, databaseURL string) error {
	return run(dir, databaseURL, func(m *migrate.Migrate) error { return m.Up() })
}

// Down lùi đúng một bước, tránh xoá sạch schema do gõ nhầm.
func Down(dir, databaseURL string) error {
	return run(dir, databaseURL, func(m *migrate.Migrate) error { return m.Steps(-1) })
}

func Version(dir, databaseURL string) (uint, bool, error) {
	m, err := newMigrator(dir, databaseURL)
	if err != nil {
		return 0, false, err
	}
	defer m.Close()
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return v, dirty, err
}

func run(dir, databaseURL string, f func(*migrate.Migrate) error) error {
	m, err := newMigrator(dir, databaseURL)
	if err != nil {
		return fmt.Errorf("khởi tạo migrate: %w", err)
	}
	defer m.Close()
	if err := f(m); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
