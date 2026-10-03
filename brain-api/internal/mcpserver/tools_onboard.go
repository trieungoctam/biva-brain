package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/coverage"
	"github.com/trieungoctam/biva-brain/brain-api/internal/form"
)

type coverageIn struct{}

type questionsIn struct {
	Topics []string `json:"topics,omitempty" jsonschema:"chỉ hỏi các topic này; bỏ trống = mọi mục chưa phủ (bắt buộc trước)"`
	Max    int      `json:"max,omitempty" jsonschema:"tối đa số câu (mặc định 10, tối đa 30)"`
}

type createFormIn struct {
	Title     string          `json:"title,omitempty" jsonschema:"tiêu đề hiện cho nhà xe"`
	Questions []form.Question `json:"questions,omitempty" jsonschema:"[{topic, question}]; bỏ trống = lấy từ generate_questions"`
	Days      int             `json:"expires_days,omitempty" jsonschema:"số ngày link còn hiệu lực (mặc định 14, tối đa 60)"`
}

type createFormOut struct {
	form.Created
	Message     string   `json:"message" jsonschema:"tin nhắn gửi kèm link cho nhà xe"`
	NextActions []string `json:"next_actions"`
}

type getFormIn struct {
	FormID string `json:"form_id"`
}

const (
	coverageDesc = "Độ phủ tri thức của nhà xe theo template ngành: từng mục bắt buộc/khuyến nghị là covered " +
		"(nhà xe đã có), industry_default (đang dùng thông lệ chung, cần nhà xe xác nhận), ambiguous (đang có đề xuất " +
		"mâu thuẫn) hay missing; % mục bắt buộc đã phủ."
	questionsDesc = "Bộ câu hỏi gửi nhà xe cho các mục chưa phủ (bắt buộc trước). Mục mà đa số nhà xe theo thông lệ " +
		"→ câu xác nhận nhanh dựa trên thông lệ; mục nhiều nhà xe khác nhau → câu hỏi mở. Kèm message gộp sẵn để " +
		"gửi qua Zalo, hoặc dùng create_form để có link điền trên điện thoại."
	createFormDesc = "Tạo link form (điền trên điện thoại, không cần đăng nhập) để nhà xe trả lời câu hỏi; câu trả lời " +
		"tự vào Brain như một nguồn (ingest) và đi qua review. Link bí mật, dùng một lần, có hạn."
	getFormDesc = "Trạng thái một form: đã gửi chưa, câu trả lời, job ingest (operation_id) để theo dõi."
)

func (s *Server) addOnboardTools(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "get_coverage", Description: coverageDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ coverageIn) (*mcp.CallToolResult, coverage.Coverage, error) {
			c, err := coverage.Get(ctx, s.db, operatorID, s.template)
			if err != nil {
				return nil, c, internal("get_coverage", err)
			}
			return nil, c, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "generate_questions", Description: questionsDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in questionsIn) (*mcp.CallToolResult, coverage.Questions, error) {
			if err := s.checkTopics(in.Topics); err != nil {
				return nil, coverage.Questions{}, err
			}
			q, err := coverage.Generate(ctx, s.db, operatorID, s.template, in.Topics, in.Max)
			if err != nil {
				return nil, q, internal("generate_questions", err)
			}
			return nil, q, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "create_form", Description: createFormDesc, Annotations: writeTool},
		func(ctx context.Context, req *mcp.CallToolRequest, in createFormIn) (*mcp.CallToolResult, createFormOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, createFormOut{}, err
			}
			qs := in.Questions
			if len(qs) == 0 {
				gen, err := coverage.Generate(ctx, s.db, operatorID, s.template, nil, 15)
				if err != nil {
					return nil, createFormOut{}, internal("create_form", err)
				}
				for _, g := range gen.Questions {
					qs = append(qs, form.Question{Topic: g.Topic, Question: g.Question})
				}
				if len(qs) == 0 {
					return nil, createFormOut{}, errors.New("nhà xe đã phủ đủ các mục — không còn gì để hỏi")
				}
			}
			days := in.Days
			if days <= 0 {
				days = 14
			}
			if days > 60 {
				days = 60
			}
			if err := form.Check(qs); err != nil {
				return nil, createFormOut{}, err
			}
			// Topic phải thuộc template (hoặc "other") — topic lạ làm job ingest fail vĩnh viễn
			// SAU khi nhà xe đã trả lời: câu trả lời thật kẹt trong form đã submitted (review r3).
			for _, q := range qs {
				if err := s.checkTopics([]string{q.Topic}); err != nil {
					return nil, createFormOut{}, err
				}
			}
			c, err := form.Create(ctx, s.db, s.publicURL, operatorID, in.Title, qs, p.Actor(), time.Duration(days)*24*time.Hour)
			if err != nil {
				return nil, createFormOut{}, internal("create_form", err)
			}
			msg := "Chào anh/chị, nhờ anh/chị trả lời giúp vài câu về dịch vụ của nhà xe để trợ lý chat trả lời khách " +
				"chính xác (mở trên điện thoại, mất khoảng 5 phút): " + c.URL
			return nil, createFormOut{Created: c, Message: msg,
				NextActions: []string{"gửi message cho nhà xe", "get_form(" + c.ID + ") để xem đã trả lời chưa"}}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_form", Description: getFormDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getFormIn) (*mcp.CallToolResult, form.Status, error) {
			if !isUUID(in.FormID) {
				return nil, form.Status{}, errors.New("form_id phải là UUID")
			}
			st, err := form.Get(ctx, s.db, operatorID, in.FormID)
			if errors.Is(err, form.ErrNotFound) {
				return nil, st, err
			}
			if err != nil {
				return nil, st, internal("get_form", err)
			}
			return nil, st, nil
		})

	srv.AddPrompt(&mcp.Prompt{Name: "onboard_operator", Title: "Onboard nhà xe mới",
		Description: "Xem độ phủ, xin nguồn còn thiếu (câu hỏi / form), đưa câu trả lời vào Brain"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return userPrompt("Onboard nhà xe "+operatorID, onboardPrompt), nil
		})
}

const onboardPrompt = `Hãy giúp builder onboard nhà xe này bằng các tool của Brain:

1. get_operator_overview và get_coverage: tóm tắt nhà xe đã có gì, mục bắt buộc nào còn thiếu / đang dùng thông lệ /
   đang mâu thuẫn.
2. Nếu builder có sẵn tài liệu (tin Zalo, file Excel giá/lịch, ảnh bảng giá) trong cuộc trò chuyện: đọc và đưa vào
   theo prompt process_update (submit_knowledge, dùng lại key).
3. Mục còn thiếu: generate_questions. Đề xuất builder chọn: gửi message (Zalo) hoặc create_form để có link cho nhà xe
   điền. Mục 'ambiguous': list_review_queue để builder chọn bản đúng.
4. Khi nhà xe đã trả lời (get_form status=submitted): get_operation(operation_id) → list_review_queue → trình bày,
   builder đồng ý mới apply_review.
5. Báo lại: % mục bắt buộc đã phủ, việc còn lại, và khi đủ thì gợi ý chạy build_bot.`
