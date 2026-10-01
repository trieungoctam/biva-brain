package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/logic"
	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
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

type planIn struct {
	Capability string `json:"capability"`
}

type recordDecisionIn struct {
	Capability string   `json:"capability"`
	Title      string   `json:"title" jsonschema:"tóm tắt quyết định"`
	Context    string   `json:"context" jsonschema:"vì sao phải quyết định (trạng thái, ràng buộc)"`
	Options    []string `json:"options,omitempty" jsonschema:"các phương án đã cân nhắc"`
	Decision   string   `json:"decision" jsonschema:"phương án chọn và hệ quả"`
}

type proposeProfileIn struct {
	ProfileYAML string        `json:"profile_yaml" jsonschema:"nội dung operators/<id>/profile.yaml theo schema logic.profile"`
	Files       []proposeFile `json:"files,omitempty" jsonschema:"file hook/custom kèm theo"`
}
type proposeFile struct {
	Path    string `json:"path" jsonschema:"đường dẫn trong thư mục nhà xe, vd hooks/pickup_by_hour.py"`
	Content string `json:"content"`
}

const (
	planDesc = "Kế hoạch triển khai logic theo capability: module L1 phủ nhiều feature của spec nhất " +
		"(config nếu đủ), phần thiếu — viết mới hoặc tái dùng của nhà xe tương tự (hook), thật sự đặc biệt " +
		"thì custom + ADR. Bậc thấp nhất đủ dùng. Kèm params_draft từ spec (khi viết profile phải ghi source item)."
	recordDecisionDesc = "Ghi ADR — lý do nhà xe cần hook/custom thay vì module chuẩn. Bắt buộc trước " +
		"propose_logic_profile với mode=custom. Trả id để ghi vào profile.yaml (decision:)."
	proposeProfileDesc = "Đề xuất hồ sơ logic (profile.yaml + file hook/custom) → job tạo PR vào repo " +
		"biva-integrations (cần cấu hình token; chưa có thì trả patch để builder tạo PR tay). Custom phải có " +
		"ADR (record_decision) — bị từ chối nếu chưa có. Trả operation_id, xem kết quả bằng get_operation."
)

type recordDecisionOut struct {
	ID          string   `json:"id"`
	RecordedAs  string   `json:"recorded_as"`
	NextActions []string `json:"next_actions"`
}

type proposeProfileOut struct {
	OperationID string   `json:"operation_id"`
	NextActions []string `json:"next_actions"`
}

func (s *Server) addPlanTools(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "plan_logic_implementation", Description: planDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in planIn) (*mcp.CallToolResult, logic.ImplementationPlan, error) {
			if in.Capability == "" {
				return nil, logic.ImplementationPlan{}, errors.New("thiếu capability")
			}
			p, err := logic.BuildPlan(ctx, s.db, operatorID, in.Capability)
			if errors.Is(err, logic.ErrNoSpec) {
				return nil, logic.ImplementationPlan{}, errors.New("chưa có logic spec — ops chạy job logic.spec trước")
			}
			if err != nil {
				return nil, logic.ImplementationPlan{}, internal("plan_logic_implementation", err)
			}
			return nil, p, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "record_decision", Description: recordDecisionDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in recordDecisionIn) (*mcp.CallToolResult, recordDecisionOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, recordDecisionOut{}, err
			}
			if in.Capability == "" || in.Title == "" || in.Decision == "" {
				return nil, recordDecisionOut{}, errors.New("thiếu capability, title hoặc decision")
			}
			id, err := logic.RecordDecision(ctx, s.db, operatorID, in.Capability, in.Title, in.Context,
				in.Options, in.Decision, p.Actor())
			if err != nil {
				return nil, recordDecisionOut{}, internal("record_decision", err)
			}
			return nil, recordDecisionOut{ID: id, RecordedAs: p.Actor(),
				NextActions: []string{"ghi \"" + id + "\" vào phần decision của capability (profile.yaml)",
					"propose_logic_profile"}}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "propose_logic_profile", Description: proposeProfileDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in proposeProfileIn) (*mcp.CallToolResult, proposeProfileOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, proposeProfileOut{}, err
			}
			if in.ProfileYAML == "" {
				return nil, proposeProfileOut{}, errors.New("thiếu profile_yaml")
			}
			files := make([]map[string]string, len(in.Files))
			for i, f := range in.Files {
				files[i] = map[string]string{"path": f.Path, "content": f.Content}
			}
			opID, created, err := queue.Enqueue(ctx, s.db, queue.Job{
				Kind: "logic.propose", OperatorID: operatorID,
				Payload: map[string]any{"profile_yaml": in.ProfileYAML, "files": files, "requested_by": p.Actor()},
			})
			if err != nil {
				return nil, proposeProfileOut{}, internal("propose_logic_profile", err)
			}
			_ = created
			return nil, proposeProfileOut{OperationID: opID,
				NextActions: []string{"get_operation(operation_id=" + opID + ") — job tạo PR hoặc trả patch",
					"sau khi PR merge: job index_code đồng bộ lại trong ≤ 1 phút"}}, nil
		})
}

type familiesIn struct {
	Capability string `json:"capability,omitempty" jsonschema:"bỏ trống = mọi capability"`
}

const familiesDesc = "Họ logic theo capability: cụm nhà xe có logic spec giống nhau (trùng feature ≥ 60%). " +
	"promote_candidate = ≥ 3 nhà xe cùng hook/custom — ứng viên đưa lên tham số/hook chuẩn " +
	"(docs/logic-knowledge.md §8); members là danh sách nhà xe được lợi."

type familiesOut struct {
	Families    []logic.Family `json:"families"`
	NextActions []string       `json:"next_actions"`
}

func (s *Server) addFamiliesTool(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_logic_families", Description: familiesDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in familiesIn) (*mcp.CallToolResult, familiesOut, error) {
			if in.Capability != "" && !slices.ContainsFunc(s.template.Capabilities, func(c kb.Capability) bool {
				return c.ID == in.Capability
			}) {
				return nil, familiesOut{}, errors.New("capability phải thuộc template")
			}
			fams, err := logic.Families(ctx, s.db, in.Capability)
			if err != nil {
				return nil, familiesOut{}, internal("list_logic_families", err)
			}
			var next []string
			for _, f := range fams {
				if f.PromoteCandidate {
					next = append(next, "họ "+f.ID+": đề xuất promote hook (members: "+
						strings.Join(f.Members, ", ")+")")
					break
				}
			}
			next = append(next, "compare_logic giữa các thành viên để xem khác biệt")
			return nil, familiesOut{Families: fams, NextActions: next}, nil
		})
}

type runExamplesIn struct {
	Capability string   `json:"capability" jsonschema:"capability cần thử"`
	Candidates []string `json:"candidates,omitempty" jsonschema:"nhà xe ứng viên (id); bỏ trống = mọi nhà xe có hồ sơ cùng capability"`
}

const runExamplesDesc = "Chạy ví dụ THẬT của nhà xe này (logic_tests) trên cách triển khai của các nhà xe " +
	"tương tự trong sandbox (không mạng, giới hạn CPU/RAM): mỗi ứng viên trả % pass và case fail " +
	"(input, kỳ vọng, nhận được) — ứng viên pass cao nhất là điểm xuất phát triển khai; case fail chỉ " +
	"đúng phần phải viết thêm. Async: trả operation_id, xem kết quả bằng get_operation."

type runExamplesOut struct {
	OperationID string   `json:"operation_id"`
	NextActions []string `json:"next_actions"`
}

func (s *Server) addRunExamplesTool(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "run_examples_against", Description: runExamplesDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in runExamplesIn) (*mcp.CallToolResult, runExamplesOut, error) {
			if in.Capability == "" {
				return nil, runExamplesOut{}, errors.New("thiếu capability")
			}
			if len(in.Candidates) > 5 {
				return nil, runExamplesOut{}, errors.New("tối đa 5 ứng viên")
			}
			payload := map[string]any{"capability": in.Capability}
			if len(in.Candidates) > 0 {
				payload["candidates"] = in.Candidates
			}
			opID, _, err := queue.Enqueue(ctx, s.db, queue.Job{
				Kind: "logic.examples", OperatorID: operatorID, Payload: payload})
			if err != nil {
				return nil, runExamplesOut{}, internal("run_examples_against", err)
			}
			return nil, runExamplesOut{OperationID: opID,
				NextActions: []string{"get_operation(operation_id=" + opID + ") — % pass từng ứng viên",
					"plan_logic_implementation khi đã chốt điểm xuất phát"}}, nil
		})
}
