// Package config đọc cấu hình brain-api từ biến môi trường.
package config

import (
	"errors"
	"os"
	"time"
)

type Config struct {
	DatabaseURL        string        // BIVA_DATABASE_URL (bắt buộc) — primary: ghi + queue
	DatabaseReplicaURL string        // BIVA_DATABASE_REPLICA_URL — đọc; trống thì dùng primary
	HTTPAddr           string        // BIVA_HTTP_ADDR, mặc định :8080
	MigrationsDir      string        // BIVA_MIGRATIONS_DIR, mặc định ../contracts/migrations
	PublicURL          string        // BIVA_PUBLIC_URL: URL công khai (issuer OAuth, resource MCP), mặc định http://localhost:8080
	KBDir              string        // BIVA_KB_DIR, mặc định ../kb
	SchemasDir         string        // BIVA_SCHEMAS_DIR, mặc định ../contracts/schemas
	TEIURL             string        // BIVA_TEI_URL: TEI embedding cho recall; trống = recall chỉ dùng keyword
	ShutdownTimeout    time.Duration // thời gian chờ tắt êm
}

// Paths trả về thư mục kb/ và contracts/schemas — dùng cả cho lệnh không cần DB (kb check).
func Paths() (kbDir, schemasDir string) {
	return getenv("BIVA_KB_DIR", "../kb"), getenv("BIVA_SCHEMAS_DIR", "../contracts/schemas")
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:        os.Getenv("BIVA_DATABASE_URL"),
		DatabaseReplicaURL: os.Getenv("BIVA_DATABASE_REPLICA_URL"),
		HTTPAddr:           getenv("BIVA_HTTP_ADDR", ":8080"),
		MigrationsDir:      getenv("BIVA_MIGRATIONS_DIR", "../contracts/migrations"),
		PublicURL:          getenv("BIVA_PUBLIC_URL", "http://localhost:8080"),
		TEIURL:             os.Getenv("BIVA_TEI_URL"),
		ShutdownTimeout:    10 * time.Second,
	}
	c.KBDir, c.SchemasDir = Paths()
	if c.DatabaseURL == "" {
		return c, errors.New("thiếu BIVA_DATABASE_URL")
	}
	if c.DatabaseReplicaURL == "" {
		c.DatabaseReplicaURL = c.DatabaseURL
	}
	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
