package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"time"

	"github.com/trieungoctam/biva-brain/go/internal/audit"
	"github.com/trieungoctam/biva-brain/go/internal/authz"
	"github.com/trieungoctam/biva-brain/go/internal/config"
	"github.com/trieungoctam/biva-brain/go/internal/store"
)

const adminUsage = `quản trị (M0, trước khi có console):
  brain-api operator add <id> <tên>
  brain-api user add <id> --email <email> --name <tên> --role builder|lead|ops
  brain-api user grant <user_id> <operator_id>
  brain-api user ungrant <user_id> <operator_id>
  brain-api token issue <user_id> [--name <tên>] [--ttl 2160h]
  brain-api token revoke <token_id>`

// runAdmin chạy lệnh quản trị; mọi thay đổi ghi audit với actor cli:<user hệ điều hành>.
func runAdmin(cfg config.Config, group string, args []string) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	pool := db.Primary
	actor := "cli:" + osUser()
	record := func(action, target string, payload map[string]any) error {
		return audit.Write(ctx, pool, audit.Entry{Actor: actor, Action: action, Target: target, Payload: payload})
	}

	switch group + " " + args[0] {
	case "operator add":
		if len(args) != 3 {
			return errors.New("dùng: brain-api operator add <id> <tên>")
		}
		if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, $2)`, args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("đã tạo nhà xe %s\n", args[1])
		return record("operator.create", "operator:"+args[1], map[string]any{"name": args[2]})

	case "user add":
		fs := flag.NewFlagSet("user add", flag.ContinueOnError)
		email := fs.String("email", "", "email")
		name := fs.String("name", "", "tên hiển thị")
		roleS := fs.String("role", "builder", "builder | lead | ops")
		if len(args) < 2 {
			return errors.New("dùng: brain-api user add <id> --email ... --name ... --role ...")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		role, err := authz.ParseRole(*roleS)
		if err != nil {
			return err
		}
		if *email == "" || *name == "" {
			return errors.New("cần --email và --name")
		}
		if err := authz.CreateUser(ctx, pool, args[1], *email, *name, role); err != nil {
			return err
		}
		fmt.Printf("đã tạo người dùng %s (%s)\n", args[1], role)
		return record("user.create", "user:"+args[1], map[string]any{"role": role})

	case "user grant", "user ungrant":
		if len(args) != 3 {
			return fmt.Errorf("dùng: brain-api user %s <user_id> <operator_id>", args[0])
		}
		if args[0] == "grant" {
			err = authz.GrantOperator(ctx, pool, args[1], args[2], actor)
		} else {
			err = authz.RevokeOperator(ctx, pool, args[1], args[2])
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s %s ↔ %s\n", args[0], args[1], args[2])
		return record("user."+args[0], "user:"+args[1], map[string]any{"operator_id": args[2]})

	case "token issue":
		fs := flag.NewFlagSet("token issue", flag.ContinueOnError)
		name := fs.String("name", "", "tên gợi nhớ (vd: claude-code laptop)")
		ttl := fs.Duration("ttl", 90*24*time.Hour, "thời hạn")
		if len(args) < 2 {
			return errors.New("dùng: brain-api token issue <user_id> [--name ...] [--ttl ...]")
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		id, token, exp, err := authz.IssueToken(ctx, pool, args[1], *name, *ttl)
		if err != nil {
			return err
		}
		fmt.Printf("token_id: %s\nhết hạn: %s\ntoken (chỉ hiện một lần, hãy lưu lại):\n%s\n", id, exp.Format(time.RFC3339), token)
		return record("token.issue", "user:"+args[1], map[string]any{"token_id": id, "expires_at": exp})

	case "token revoke":
		if len(args) != 2 {
			return errors.New("dùng: brain-api token revoke <token_id>")
		}
		if err := authz.RevokeToken(ctx, pool, args[1]); err != nil {
			return err
		}
		fmt.Printf("đã thu hồi token %s\n", args[1])
		return record("token.revoke", "token:"+args[1], nil)
	}
	return errors.New(adminUsage)
}

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
