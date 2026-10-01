package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/trieungoctam/biva-brain/brain-api/internal/config"
	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/store"
)

func kbCheck() error {
	kbDir, schemasDir := config.Paths()
	b, err := kb.Load(kbDir, schemasDir)
	if err != nil {
		return err
	}
	l0 := 0
	for _, r := range b.Rules {
		if r.Layer == 0 {
			l0++
		}
	}
	fmt.Printf("kb hợp lệ: %d rule L0, %d rule L1, %d template\n", l0, len(b.Rules)-l0, len(b.Templates))
	return nil
}

// runKB: brain-api kb sync [--dry-run]
func runKB(cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return errors.New("dùng: brain-api kb check | kb sync [--dry-run]")
	}
	dryRun := len(args) > 1 && args[1] == "--dry-run"
	b, err := kb.Load(cfg.KBDir, cfg.SchemasDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := kb.Sync(ctx, db.Primary, b, "cli:"+osUser(), dryRun)
	if err != nil {
		return err
	}
	for _, c := range rep.Changes {
		fmt.Printf("  %-7s L%d %s\n", c.Action, c.Layer, c.Key)
	}
	mode := "đã sync"
	if dryRun {
		mode = "dry-run (chưa ghi)"
	}
	fmt.Printf("%s: thêm %d, sửa %d, bỏ %d, giữ nguyên %d\n",
		mode, rep.Count("add"), rep.Count("update"), rep.Count("retire"), rep.Unchanged)
	if rep.IndexJob != "" {
		fmt.Printf("job index.items: %s\n", rep.IndexJob)
	}
	return nil
}
