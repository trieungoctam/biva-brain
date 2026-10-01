// Package config đọc cấu hình brain-api từ biến môi trường.
package config

import (
	"errors"
	"os"
	"time"
)

type Config struct {
	DatabaseURL        string // BIVA_DATABASE_URL (bắt buộc) — primary: ghi + queue
	DatabaseReplicaURL string // BIVA_DATABASE_REPLICA_URL — đọc; trống thì dùng primary
	HTTPAddr           string // BIVA_HTTP_ADDR, mặc định :8080
	MigrationsDir      string // BIVA_MIGRATIONS_DIR, mặc định ../contracts/migrations
	PublicURL          string // BIVA_PUBLIC_URL: URL công khai (issuer OAuth, resource MCP), mặc định http://localhost:8080
	KBDir              string // BIVA_KB_DIR, mặc định ../kb
	SchemasDir         string // BIVA_SCHEMAS_DIR, mặc định ../contracts/schemas
	TEIURL             string // BIVA_TEI_URL: TEI embedding cho recall; trống = recall chỉ dùng keyword
	RerankURL          string // BIVA_RERANK_URL: TEI rerank (bge-reranker-v2-m3); trống = không rerank
	S3Endpoint         string // BIVA_S3_ENDPOINT: object storage cho bản export (vd http://localhost:8333); trống = trả nội dung, không lưu
	S3Bucket           string // BIVA_S3_BUCKET, mặc định biva-exports
	S3PublicURL        string // BIVA_S3_PUBLIC_URL: cơ sở link tải (khác endpoint khi truy cập từ ngoài); trống = dùng endpoint
	IntegrationsRepo   string // BIVA_INTEGRATIONS_REPO: repo biva-integrations (URL git hoặc path checkout) cho job index.code; trống = tắt
	S3AccessKey        string // BIVA_S3_ACCESS_KEY / BIVA_S3_SECRET_KEY — trống = anonymous (SeaweedFS local)
	S3SecretKey        string
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
		RerankURL:          os.Getenv("BIVA_RERANK_URL"),
		S3Endpoint:         os.Getenv("BIVA_S3_ENDPOINT"),
		S3Bucket:           getenv("BIVA_S3_BUCKET", "biva-exports"),
		S3AccessKey:        os.Getenv("BIVA_S3_ACCESS_KEY"),
		S3PublicURL:        os.Getenv("BIVA_S3_PUBLIC_URL"),
		IntegrationsRepo:   os.Getenv("BIVA_INTEGRATIONS_REPO"),
		S3SecretKey:        os.Getenv("BIVA_S3_SECRET_KEY"),
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
