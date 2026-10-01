package mcpserver

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
)

type getOperationIn struct {
	OperationID string `json:"operation_id" jsonschema:"id của job async (UUID) do tool ghi trả về"`
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true}

const getOperationDesc = "Trạng thái một job async (ingest, consolidate, test...): queued | running | done | failed, " +
	"số lần thử, kết quả hoặc lỗi. Gọi lại sau vài giây nếu còn queued/running."

// errOperationNotFound cũng dùng khi job thuộc nhà xe khác: không để lộ job của nhà xe khác có tồn tại.
var errOperationNotFound = errors.New("không tìm thấy operation trong phạm vi này")

// errUnavailable thay cho lỗi nội bộ (DB...): chi tiết chỉ ghi log, không trả cho AI.
var errUnavailable = errors.New("Brain tạm thời không trả lời được, thử lại sau")

func addOperatorTools(srv *mcp.Server, db *pgxpool.Pool, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "get_operation", Description: getOperationDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getOperationIn) (*mcp.CallToolResult, queue.Operation, error) {
			op, err := getOperation(ctx, db, in.OperationID)
			if err != nil {
				return nil, queue.Operation{}, err
			}
			if op.OperatorID == nil || *op.OperatorID != operatorID {
				return nil, queue.Operation{}, errOperationNotFound
			}
			return nil, op, nil
		})
}

func addPlatformTools(srv *mcp.Server, db *pgxpool.Pool) {
	mcp.AddTool(srv, &mcp.Tool{Name: "get_operation", Description: getOperationDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getOperationIn) (*mcp.CallToolResult, queue.Operation, error) {
			op, err := getOperation(ctx, db, in.OperationID)
			return nil, op, err
		})
}

func getOperation(ctx context.Context, db *pgxpool.Pool, id string) (queue.Operation, error) {
	if !isUUID(id) {
		return queue.Operation{}, errors.New("operation_id phải là UUID")
	}
	op, err := queue.Get(ctx, db, id)
	if errors.Is(err, queue.ErrNotFound) {
		return op, errOperationNotFound
	}
	if err != nil {
		slog.Error("get_operation lỗi", "err", err)
		return op, errUnavailable
	}
	return op, nil
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}
