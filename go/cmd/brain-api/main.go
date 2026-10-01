// Lệnh brain-api: MCP + REST + scheduler (M0: HTTP health + migrate).
//
//	brain-api serve          chạy HTTP server (mặc định)
//	brain-api migrate up     áp dụng mọi migration
//	brain-api migrate down   lùi một migration
//	brain-api migrate version
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
	"github.com/trieungoctam/biva-brain/go/internal/migrate"
	"github.com/trieungoctam/biva-brain/go/internal/store"
)

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
	default:
		return fmt.Errorf("lệnh không hợp lệ %q (dùng: serve | migrate up|down|version)", cmd)
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

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: httpapi.NewRouter(db)}
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
