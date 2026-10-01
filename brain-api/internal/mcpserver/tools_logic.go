package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/logic"
)

type logicSpecIn struct {
	Capability string `json:"capability,omitempty" jsonschema:"bỏ trống = mọi capability đã có spec"`
}

type similarIn struct {
	Capability string `json:"capability"`
	Limit      int    `json:"limit,omitempty" jsonschema:"tối đa ứng viên (mặc định 10)"`
}

type compareIn struct {
	With       string `json:"with" jsonschema:"id nhà xe muốn so sánh"`
	Capability string `json:"capability"`
}

const (
	logicSpecDesc = "Logic spec của nhà xe theo capability (dấu vân tay logic: feature + tham số, quy tắc bằng lời, " +
		"hồ sơ triển khai nếu có) — dựng bởi job logic.spec từ tri thức đã duyệt; có cả khi chưa có code."
	similarDesc = "Tìm nhà xe tương tự theo tầng spec (docs/logic-knowledge.md §5.1): 50% trùng feature (trọng số " +
		"theo độ hiếm) + 30% tương tự quy tắc + 20% gần tham số. Kèm giải thích: feature trùng, thiếu " +
		"(mình có, họ không — phần phải tự viết), khác (họ có, mình chưa), tham số lệch. Làm điểm xuất phát " +
		"triển khai logic cho khách mới."
	compareDesc = "So logic của nhà xe này với một nhà xe khác theo capability: điểm và chi tiết feature trùng/" +
		"thiếu/khác, tham số lệch."
	operatorLogicDesc = "Hồ sơ logic của nhà xe theo capability của template: cái gì có hồ sơ (config/hook/custom, " +
		"module, trạng thái), cái gì có spec (đã trích từ tri thức), capability còn thiếu — gọi trước khi " +
		"triển khai logic; capability thiếu mà nhà xe thật sự cần → custom phải có ADR."
)

type logicOut struct {
	logic.Spec
	NextActions []string `json:"next_actions"`
}

type similarOut struct {
	Capability  string            `json:"capability"`
	Candidates  []logic.Candidate `json:"candidates"`
	NextActions []string          `json:"next_actions"`
}

type compareOut struct {
	logic.Candidate
	NextActions []string `json:"next_actions"`
}

type operatorLogicOut struct {
	Capabilities []logic.CapabilityState `json:"capabilities"`
	Missing      []string                `json:"missing"`
	NextActions  []string                `json:"next_actions"`
}

func (s *Server) addLogicTools(srv *mcp.Server, operatorID string) {
	caps := make([]logic.CapabilityInfo, len(s.template.Capabilities))
	for i, c := range s.template.Capabilities {
		caps[i] = logic.CapabilityInfo{ID: c.ID, Title: c.Title, Level: c.Level}
	}

	mcp.AddTool(srv, &mcp.Tool{Name: "get_logic_spec", Description: logicSpecDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logicSpecIn) (*mcp.CallToolResult, logicOut, error) {
			if in.Capability != "" && !slices.ContainsFunc(caps, func(c logic.CapabilityInfo) bool { return c.ID == in.Capability }) {
				return nil, logicOut{}, errors.New("capability phải thuộc template: " + s.topicGuide())
			}
			spec, err := logic.GetSpec(ctx, s.db, operatorID, in.Capability)
			if errors.Is(err, logic.ErrNoSpec) {
				return nil, logicOut{}, errors.New("chưa có logic spec cho capability này — ops chạy job logic.spec " +
					"(extract_logic_spec) sau khi nhà xe đã có tri thức đã duyệt")
			}
			if err != nil {
				return nil, logicOut{}, internal("get_logic_spec", err)
			}
			next := []string{"find_similar_operators (capability=" + spec.Capability + ")"}
			if len(spec.Proposed) > 0 {
				next = append([]string{"review proposed_features (chưa có trong danh mục L1)"}, next...)
			}
			return nil, logicOut{Spec: spec, NextActions: next}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "find_similar_operators", Description: similarDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in similarIn) (*mcp.CallToolResult, similarOut, error) {
			if in.Capability == "" {
				return nil, similarOut{}, errors.New("thiếu capability")
			}
			cands, err := logic.Similar(ctx, s.db, operatorID, in.Capability, in.Limit)
			if errors.Is(err, logic.ErrNoSpec) {
				return nil, similarOut{}, errors.New("nhà xe này chưa có logic spec — ops chạy job logic.spec trước")
			}
			if err != nil {
				return nil, similarOut{}, internal("find_similar_operators", err)
			}
			return nil, similarOut{Capability: in.Capability, Candidates: cands,
				NextActions: []string{"compare_logic(with=<ứng viên>, capability=" + in.Capability + ")",
					"plan_logic_implementation khi đã chốt điểm xuất phát"}}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "compare_logic", Description: compareDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in compareIn) (*mcp.CallToolResult, compareOut, error) {
			if in.With == "" || in.Capability == "" {
				return nil, compareOut{}, errors.New("thiếu with hoặc capability")
			}
			if in.With == operatorID {
				return nil, compareOut{}, errors.New("không so với chính nhà xe này")
			}
			cand, err := logic.Compare(ctx, s.db, operatorID, in.With, in.Capability)
			if err != nil {
				return nil, compareOut{}, internal("compare_logic", err)
			}
			return nil, compareOut{Candidate: *cand,
				NextActions: []string{"tái dùng module/hook của " + in.With + " nếu trùng cao",
					"viết mới phần Missing"}}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_operator_logic", Description: operatorLogicDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, operatorLogicOut, error) {
			states, err := logic.Overview(ctx, s.db, operatorID, caps)
			if err != nil {
				return nil, operatorLogicOut{}, internal("get_operator_logic", err)
			}
			var missing []string
			for _, st := range states {
				if st.Profile == nil {
					missing = append(missing, st.ID)
				}
			}
			next := []string{}
			if len(missing) > 0 {
				next = append(next, "triển khai capability còn thiếu: "+strings.Join(missing, ", "))
			}
			next = append(next, "get_logic_spec(capability=…) cho từng capability")
			return nil, operatorLogicOut{Capabilities: states, Missing: missing, NextActions: next}, nil
		})
}
