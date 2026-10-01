// Package audit ghi nhật ký ai làm gì vào bảng audit_log.
//
// Actor: user:<id> | ai:<session> | system:<job> | cli:<os user>.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	Actor      string
	ApprovedBy string
	Action     string
	Target     string
	Payload    map[string]any
}

func Write(ctx context.Context, db *pgxpool.Pool, e Entry) error {
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("audit payload: %w", err)
	}
	_, err = db.Exec(ctx, `
		INSERT INTO audit_log (actor, approved_by, action, target, payload)
		VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5)`,
		e.Actor, e.ApprovedBy, e.Action, e.Target, b)
	if err != nil {
		return fmt.Errorf("ghi audit %s: %w", e.Action, err)
	}
	return nil
}
