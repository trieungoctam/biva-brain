package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/authz"
	"github.com/trieungoctam/biva-brain/brain-api/internal/confirm"
	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
	"github.com/trieungoctam/biva-brain/brain-api/internal/review"
)

// ─────────────────────────────── kiểu vào/ra ───────────────────────────────

type ingestIn struct {
	Content    string `json:"content" jsonschema:"nội dung nhà xe gửi (tin Zalo, ghi chú...), nguyên văn"`
	Source     string `json:"source,omitempty" jsonschema:"kênh: zalo | form | console (mặc định zalo)"`
	ReceivedAt string `json:"received_at,omitempty" jsonschema:"thời điểm nhà xe gửi, RFC 3339; mặc định là bây giờ"`
}

type ingestOut struct {
	OperationID string `json:"operation_id"`
	Duplicate   bool   `json:"duplicate" jsonschema:"true nếu nội dung này đã được gửi trước đó"`
	Next        string `json:"next"`
}

type listIn struct {
	Status string `json:"status,omitempty" jsonschema:"open (mặc định) | applied | rejected | stale"`
	Risk   string `json:"risk,omitempty" jsonschema:"lọc theo rủi ro: high | low"`
	Limit  int    `json:"limit,omitempty" jsonschema:"tối đa (mặc định 50, tối đa 200)"`
}

type listOut struct {
	Items []review.Summary `json:"items"`
}

type reviewIn struct {
	ReviewID string `json:"review_id"`
}

type applyIn struct {
	ReviewID     string `json:"review_id"`
	Decision     string `json:"decision" jsonschema:"approve | reject"`
	Reason       string `json:"reason,omitempty" jsonschema:"lý do (bắt buộc khi reject)"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"bỏ trống ở lần gọi đầu để nhận preview; gửi lại token sau khi builder đồng ý"`
}

type applyOut struct {
	Executed     bool            `json:"executed"`
	Preview      *review.Detail  `json:"preview,omitempty"`
	ConfirmToken string          `json:"confirm_token,omitempty"`
	ExpiresAt    *time.Time      `json:"confirm_expires_at,omitempty"`
	Outcome      *review.Outcome `json:"outcome,omitempty"`
	Message      string          `json:"message"`
}

type proposeOut struct {
	Review review.Summary `json:"review"`
	Note   string         `json:"note"`
}

const (
	ingestDesc = "Gửi nội dung nhà xe cung cấp (tin Zalo, ghi chú) để Brain trích tri thức. Chạy nền: trả operation_id, " +
		"theo dõi bằng get_operation; xong thì xem đề xuất bằng list_review_queue. Thay đổi rủi ro thấp được tự áp " +
		"dụng; giá, giờ, huỷ vé và mọi sửa/bỏ điều đang đúng phải được builder duyệt."
	listDesc = "Danh sách đề xuất thay đổi tri thức của nhà xe đang chờ duyệt (rủi ro cao trước): key, loại thay đổi " +
		"(NEW/CHANGE/REMOVE/DUPLICATE/CONFLICT), bản hiện tại và bản đề xuất."
	getDesc = "Chi tiết một đề xuất: trước/sau, nguồn (trích tin gốc), hiệu lực, tác động khi duyệt. " +
		"Luôn đọc trước khi hỏi builder duyệt."
	proposeDesc = "Đề xuất thêm/sửa/bỏ một item tri thức của nhà xe (ví dụ builder vừa xác nhận qua điện thoại). " +
		"Không bao giờ tự áp dụng: tạo đề xuất trong hàng đợi duyệt. topic phải thuộc template ngành; key mô tả chủ " +
		"thể (vd fare.sai_gon_da_lat.giuong_nam) và được chuẩn hoá; reason ghi nguồn thông tin."
	applyDesc = "Duyệt (approve) hoặc từ chối (reject) một đề xuất. Hai bước: gọi KHÔNG có confirm_token để nhận " +
		"preview + confirm_token; trình bày preview cho builder; chỉ khi builder đồng ý mới gọi lại với confirm_token " +
		"(dùng một lần, hết hạn sau 5 phút)."
)

var (
	writeTool  = &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	decideTool = &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
)

func ptr[T any](v T) *T { return &v }

// ─────────────────────────────── đăng ký ───────────────────────────────

func (s *Server) addReviewTools(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "ingest", Description: ingestDesc, Annotations: writeTool},
		func(ctx context.Context, req *mcp.CallToolRequest, in ingestIn) (*mcp.CallToolResult, ingestOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, ingestOut{}, err
			}
			return s.ingest(ctx, p, operatorID, in)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_review_queue", Description: listDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
			items, err := review.List(ctx, s.db, operatorID, in.Status, in.Risk, in.Limit)
			if err != nil {
				return nil, listOut{}, internal("list_review_queue", err)
			}
			return nil, listOut{Items: items}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_review_item", Description: getDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reviewIn) (*mcp.CallToolResult, review.Detail, error) {
			if !isUUID(in.ReviewID) {
				return nil, review.Detail{}, errors.New("review_id phải là UUID")
			}
			d, err := review.Get(ctx, s.db, operatorID, in.ReviewID)
			if errors.Is(err, review.ErrNotFound) {
				return nil, d, err
			}
			if err != nil {
				return nil, d, internal("get_review_item", err)
			}
			return nil, d, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "propose_item", Description: proposeDesc, Annotations: writeTool},
		func(ctx context.Context, req *mcp.CallToolRequest, in review.Proposal) (*mcp.CallToolResult, proposeOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, proposeOut{}, err
			}
			sum, err := review.Propose(ctx, s.db, operatorID, in, s.topics, p.Actor())
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) {
					return nil, proposeOut{}, internal("propose_item", err)
				}
				return nil, proposeOut{}, err // lỗi kiểm tra đầu vào: trả nguyên văn để AI sửa
			}
			return nil, proposeOut{Review: sum, Note: "đã vào hàng đợi duyệt; dùng apply_review khi builder đồng ý"}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "apply_review", Description: applyDesc, Annotations: decideTool},
		func(ctx context.Context, req *mcp.CallToolRequest, in applyIn) (*mcp.CallToolResult, applyOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, applyOut{}, err
			}
			return s.applyReview(ctx, p, operatorID, in)
		})
}

// ─────────────────────────────── xử lý ───────────────────────────────

func (s *Server) ingest(ctx context.Context, p authz.Principal, operatorID string, in ingestIn) (*mcp.CallToolResult, ingestOut, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" || len(content) > 200000 {
		return nil, ingestOut{}, errors.New("content phải có nội dung và ≤ 200000 ký tự")
	}
	source := in.Source
	if source == "" {
		source = "zalo"
	}
	if source != "zalo" && source != "form" && source != "console" {
		return nil, ingestOut{}, errors.New("source phải là zalo | form | console (Excel: dùng tool upload ở S1.1.3)")
	}
	received := time.Now()
	if in.ReceivedAt != "" {
		t, err := time.Parse(time.RFC3339, in.ReceivedAt)
		if err != nil {
			return nil, ingestOut{}, errors.New("received_at phải dạng RFC 3339, vd 2026-10-01T09:00:00+07:00")
		}
		received = t
	}
	sum := sha256.Sum256([]byte(content))
	id, created, err := queue.Enqueue(ctx, s.db, queue.Job{
		Kind:       "ingest",
		OperatorID: operatorID,
		Payload: map[string]any{
			"operator_id":  operatorID,
			"source":       source,
			"content":      content,
			"received_at":  received.Format(time.RFC3339),
			"submitted_by": p.Actor(),
		},
		// Cùng nội dung gửi lại → cùng job (worker cũng chống trùng theo content_hash).
		IdempotencyKey: "ingest:" + operatorID + ":" + hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return nil, ingestOut{}, internal("ingest", err)
	}
	return nil, ingestOut{OperationID: id, Duplicate: !created,
		Next: "gọi get_operation tới khi status=done, rồi list_review_queue"}, nil
}

func (s *Server) applyReview(ctx context.Context, p authz.Principal, operatorID string, in applyIn) (*mcp.CallToolResult, applyOut, error) {
	if !isUUID(in.ReviewID) {
		return nil, applyOut{}, errors.New("review_id phải là UUID")
	}
	if in.Decision != "approve" && in.Decision != "reject" {
		return nil, applyOut{}, errors.New("decision phải là approve hoặc reject")
	}
	if in.Decision == "reject" && strings.TrimSpace(in.Reason) == "" {
		return nil, applyOut{}, errors.New("reject cần reason")
	}
	subject := confirm.Subject(in.ReviewID, in.Decision, in.Reason)

	if in.ConfirmToken == "" {
		d, err := review.Get(ctx, s.db, operatorID, in.ReviewID)
		if errors.Is(err, review.ErrNotFound) {
			return nil, applyOut{}, err
		}
		if err != nil {
			return nil, applyOut{}, internal("apply_review", err)
		}
		if d.Status != "open" {
			return nil, applyOut{}, errors.New("đề xuất không còn mở (" + d.Status + ")")
		}
		token, exp, err := confirm.Issue(ctx, s.db, p.UserID, operatorID, "apply_review", subject, confirm.DefaultTTL)
		if err != nil {
			return nil, applyOut{}, internal("apply_review", err)
		}
		return nil, applyOut{Preview: &d, ConfirmToken: token, ExpiresAt: &exp,
			Message: "CHƯA thực thi. Trình bày preview cho builder; chỉ gọi lại với confirm_token khi builder đồng ý " +
				in.Decision + "."}, nil
	}

	if err := confirm.Consume(ctx, s.db, in.ConfirmToken, p.UserID, operatorID, "apply_review", subject); err != nil {
		if errors.Is(err, confirm.ErrInvalid) {
			return nil, applyOut{}, err
		}
		return nil, applyOut{}, internal("apply_review", err)
	}
	out, err := review.Decide(ctx, s.db, operatorID, in.ReviewID, in.Decision, in.Reason, p.Actor())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" { // check_violation: review không còn mở
			return nil, applyOut{}, errors.New(pgErr.Message)
		}
		if errors.Is(err, review.ErrNotFound) {
			return nil, applyOut{}, err
		}
		return nil, applyOut{}, internal("apply_review", err)
	}
	msg := "đã " + map[string]string{"applied": "áp dụng", "rejected": "từ chối", "stale": "không áp dụng được"}[out.Status]
	if out.Status == "stale" {
		msg += ": " + out.Reason + " — cần ingest/đề xuất lại trên trạng thái mới"
	}
	return nil, applyOut{Executed: true, Outcome: &out, Message: msg}, nil
}

func callerOf(req *mcp.CallToolRequest) (authz.Principal, error) {
	p, ok := principalFrom(req.Extra.TokenInfo)
	if !ok {
		return p, errors.New("thiếu thông tin người gọi")
	}
	return p, nil
}

func internal(tool string, err error) error {
	slog.Error("tool MCP lỗi", "tool", tool, "err", err)
	return errUnavailable
}
