// Package queue ghi job vào bảng operations cho ai-worker (Python) xử lý.
//
// Go chỉ enqueue và đọc trạng thái; claim/lease/retry nằm ở runner Python.
// Trigger trong migration 000002 phát NOTIFY 'biva_operations' để đánh thức worker.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/trieungoctam/biva-brain/go/internal/queue")

var ErrNotFound = errors.New("operation không tồn tại")

type Job struct {
	Kind           string
	OperatorID     string // rỗng = job không thuộc nhà xe nào (L0/L1)
	Payload        any
	IdempotencyKey string // rỗng = không chống trùng
	Priority       int16  // nhỏ chạy trước; 0 = mặc định 100
	RunAfter       time.Time
	ParentID       string
	TraceContext   map[string]string // rỗng = lấy từ span hiện tại (traceparent W3C)
}

type Operation struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	OperatorID  *string         `json:"operator_id,omitempty"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *string         `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Enqueue thêm job. Cùng IdempotencyKey → trả về id của job đã có, created=false.
// Trace context của ctx được ghi vào operations.trace_context để span của ai-worker nối tiếp trace.
func Enqueue(ctx context.Context, db *pgxpool.Pool, j Job) (id string, created bool, err error) {
	ctx, span := tracer.Start(ctx, "enqueue "+j.Kind, trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("biva.job.kind", j.Kind), attribute.String("biva.operator_id", j.OperatorID)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetAttributes(attribute.String("biva.operation_id", id), attribute.Bool("biva.created", created))
		}
		span.End()
	}()
	if j.Kind == "" {
		return "", false, errors.New("thiếu kind")
	}
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return "", false, fmt.Errorf("payload: %w", err)
	}
	if j.Payload == nil {
		payload = []byte("{}")
	}
	traceCtx := j.TraceContext
	if len(traceCtx) == 0 {
		traceCtx = map[string]string{}
		otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(traceCtx))
	}
	priority := j.Priority
	if priority == 0 {
		priority = 100
	}
	runAfter := j.RunAfter
	if runAfter.IsZero() {
		runAfter = time.Now()
	}

	err = db.QueryRow(ctx, `
		INSERT INTO operations (kind, operator_id, payload, idempotency_key, priority, run_after, parent_id, trace_context)
		VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5, $6, NULLIF($7, '')::uuid, $8)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		j.Kind, j.OperatorID, payload, j.IdempotencyKey, priority, runAfter, j.ParentID, traceCtx,
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("enqueue %s: %w", j.Kind, err)
	}
	// Trùng idempotency_key: job đã có (kể cả khi transaction kia vừa commit).
	err = db.QueryRow(ctx, `SELECT id FROM operations WHERE idempotency_key = $1`, j.IdempotencyKey).Scan(&id)
	if err != nil {
		return "", false, fmt.Errorf("đọc job trùng idempotency_key: %w", err)
	}
	return id, false, nil
}

// Get đọc trạng thái một job (dùng cho MCP get_operation).
func Get(ctx context.Context, db *pgxpool.Pool, id string) (Operation, error) {
	var op Operation
	err := db.QueryRow(ctx, `
		SELECT id, kind, operator_id, status, attempts, max_attempts, result, error, created_at, updated_at
		FROM operations WHERE id = $1`, id,
	).Scan(&op.ID, &op.Kind, &op.OperatorID, &op.Status, &op.Attempts, &op.MaxAttempts,
		&op.Result, &op.Error, &op.CreatedAt, &op.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, ErrNotFound
	}
	return op, err
}
