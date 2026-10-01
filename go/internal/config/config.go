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
	ShutdownTimeout    time.Duration // thời gian chờ tắt êm
}

func Load() (Config, error) {
	c := Config{
		DatabaseURL:        os.Getenv("BIVA_DATABASE_URL"),
		DatabaseReplicaURL: os.Getenv("BIVA_DATABASE_REPLICA_URL"),
		HTTPAddr:           getenv("BIVA_HTTP_ADDR", ":8080"),
		MigrationsDir:      getenv("BIVA_MIGRATIONS_DIR", "../contracts/migrations"),
		ShutdownTimeout:    10 * time.Second,
	}
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
