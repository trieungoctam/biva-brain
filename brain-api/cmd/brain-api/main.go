// Lệnh brain-api: MCP + REST + scheduler (M0: HTTP health, scheduler, migrate).
//
//	brain-api serve          chạy HTTP server (mặc định)
//	brain-api migrate up     áp dụng mọi migration
//	brain-api migrate down   lùi một migration
//	brain-api migrate version
//	brain-api operator|user|token ...   quản trị (xem adminUsage)
//	brain-api kb check | kb sync [--dry-run]   tri thức nền L0/L1 từ kb/
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/config"
	"github.com/trieungoctam/biva-brain/brain-api/internal/httpapi"
	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/mcpserver"
	"github.com/trieungoctam/biva-brain/brain-api/internal/migrate"
	"github.com/trieungoctam/biva-brain/brain-api/internal/scheduler"
	"github.com/trieungoctam/biva-brain/brain-api/internal/store"
	"github.com/trieungoctam/biva-brain/brain-api/internal/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// version được ghi đè lúc build: -ldflags "-X main.version=..."
var version = "0.1.0-dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(os.Args[1:]); err != nil {
		slog.Error("brain-api dừng vì lỗi", "err", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) >= 2 && args[0] == "kb" && args[1] == "check" {
		return kbCheck() // không cần DB: chạy được trong CI
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(cfg)
	case "migrate":
		return runMigrate(cfg, args[1:])
	case "operator", "user", "token":
		return runAdmin(cfg, cmd, args[1:])
	case "kb":
		return runKB(cfg, args[1:])
	default:
		return fmt.Errorf("lệnh không hợp lệ %q (dùng: serve | migrate up|down|version | operator | user | token | kb)", cmd)
	}
}

func runMigrate(cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("dùng: brain-api migrate up|down|version")
	}
	switch args[0] {
	case "up":
		return migrate.Up(cfg.MigrationsDir, cfg.DatabaseURL)
	case "down":
		return migrate.Down(cfg.MigrationsDir, cfg.DatabaseURL)
	case "version":
		v, dirty, err := migrate.Version(cfg.MigrationsDir, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		fmt.Printf("version=%d dirty=%v\n", v, dirty)
		return nil
	default:
		return fmt.Errorf("migrate %q không hợp lệ", args[0])
	}
}

func serve(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.Setup(ctx, "brain-api", version)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flushCtx)
	}()

	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseReplicaURL)
	if err != nil {
		return err
	}
	defer db.Close()

	// Scheduler chạy ở mọi instance nhưng chỉ leader (advisory lock) thực thi task.
	schedCtx, stopSched := context.WithCancel(ctx)
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		(&scheduler.Scheduler{DB: db.Primary, Tasks: scheduler.DefaultTasks()}).Run(schedCtx)
	}()
	defer func() { stopSched(); <-schedDone }()

	bundle, err := kb.Load(cfg.KBDir, cfg.SchemasDir)
	if err != nil {
		return fmt.Errorf("đọc kb/: %w", err)
	}
	var topics []mcpserver.Topic
	for _, sec := range bundle.Templates["xe-khach"].Sections {
		topics = append(topics, mcpserver.Topic{ID: sec.Topic, Title: sec.Title})
	}

	mux := httpapi.NewRouter(db)
	mcpserver.New(db.Primary, version, topics).Mount(mux)
	// otelhttp: mỗi request (MCP call...) là một span gốc; health không cần trace.
	handler := otelhttp.NewHandler(mux, "brain-api", otelhttp.WithFilter(func(r *http.Request) bool {
		return !strings.HasPrefix(r.URL.Path, "/health/")
	}), otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + routeName(r.URL.Path) }))
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("brain-api đang lắng nghe", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		slog.Info("nhận tín hiệu dừng, đang tắt êm")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// routeName đưa path về dạng template cho tên span (tránh mỗi nhà xe một tên span).
func routeName(path string) string {
	if rest, ok := strings.CutPrefix(path, "/mcp/operator/"); ok && rest != "" {
		return "/mcp/operator/{operator_id}/"
	}
	return path
}
