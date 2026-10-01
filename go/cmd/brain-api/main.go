// Lệnh brain-api: MCP + REST + scheduler (M0: HTTP health, scheduler, migrate).
//
//	brain-api serve          chạy HTTP server (mặc định)
//	brain-api migrate up     áp dụng mọi migration
//	brain-api migrate down   lùi một migration
//	brain-api migrate version
//	brain-api operator|user|token ...   quản trị (xem adminUsage)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/trieungoctam/biva-brain/go/internal/config"
	"github.com/trieungoctam/biva-brain/go/internal/httpapi"
	"github.com/trieungoctam/biva-brain/go/internal/mcpserver"
	"github.com/trieungoctam/biva-brain/go/internal/migrate"
	"github.com/trieungoctam/biva-brain/go/internal/scheduler"
	"github.com/trieungoctam/biva-brain/go/internal/store"
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
	default:
		return fmt.Errorf("lệnh không hợp lệ %q (dùng: serve | migrate up|down|version | operator | user | token)", cmd)
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

	mux := httpapi.NewRouter(db)
	mcpserver.New(db.Primary, version).Mount(mux)
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: mux}
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
