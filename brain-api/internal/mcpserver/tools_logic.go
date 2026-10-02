package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
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

type addLogicTestIn struct {
	Capability string         `json:"capability" jsonschema:"capability của ví dụ (thuộc template)"`
	Input      map[string]any `json:"input" jsonschema:"input truyền cho entry (vd route/seat/date)"`
	Out        any            `json:"out,omitempty" jsonschema:"kết quả kỳ vọng (hoặc error)"`
	Error      string         `json:"error,omitempty" jsonschema:"thông điệp lỗi kỳ vọng (so theo nội dung, không cần nguyên văn)"`
	Note       string         `json:"note,omitempty" jsonschema:"hoàn cảnh của ví dụ (vd '28 Tết')"`
	SourceItem string         `json:"source_item,omitempty" jsonschema:"id item tri thức làm nguồn (nếu có)"`
}

type logicTestOut struct {
	ID         string         `json:"id"`
	Input      map[string]any `json:"input"`
	Expected   map[string]any `json:"expected"`
	Note       string         `json:"note,omitempty"`
	SourceItem string         `json:"source_item,omitempty"`
	Commit     string         `json:"commit,omitempty"`
}

type listLogicTestsIn struct {
	Capability string `json:"capability,omitempty" jsonschema:"lọc theo capability; bỏ trống = mọi capability"`
}

const (
	addLogicTestDesc = "Thêm ví dụ input → output của nhà xe (logic test): dùng cho run_examples_against " +
		"chọn điểm xuất phát triển khai và CI repo chạy lại. Chọn out HOẶC error. Ví dụ nên kèm note " +
		"hoàn cảnh và source item tri thức khi có."
	listLogicTestsDesc = "Danh sách ví dụ input → output của nhà xe theo capability (từ tests/cases.yaml " +
		"đồng bộ bởi index_code và add_logic_test)."
)

type addLogicTestOut struct {
	logicTestOut
	NextActions []string `json:"next_actions"`
}

type listLogicTestsOut struct {
	Tests       []logicTestOut `json:"tests"`
	NextActions []string       `json:"next_actions"`
}

func (s *Server) addLogicTestsTools(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "add_logic_test", Description: addLogicTestDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in addLogicTestIn) (*mcp.CallToolResult, addLogicTestOut, error) {
			if _, err := callerOf(req); err != nil {
				return nil, addLogicTestOut{}, err
			}
			if in.Capability == "" || len(in.Input) == 0 {
				return nil, addLogicTestOut{}, errors.New("thiếu capability hoặc input")
			}
			if !slices.ContainsFunc(s.template.Capabilities, func(c kb.Capability) bool { return c.ID == in.Capability }) {
				return nil, addLogicTestOut{}, errors.New("capability phải thuộc template")
			}
			if (in.Out == nil) == (in.Error == "") {
				return nil, addLogicTestOut{}, errors.New("chỉ một trong out hoặc error")
			}
			expected := map[string]any{"out": in.Out}
			if in.Error != "" {
				expected = map[string]any{"error": in.Error}
			}
			var id string
			if err := s.db.QueryRow(ctx, `
				INSERT INTO logic_tests (operator_id, capability, input, expected, note, source_item_id, commit)
				VALUES ($1, $2, $3::jsonb, $4::jsonb, NULLIF($5, ''), NULLIF($6, '')::uuid, 'mcp')
				RETURNING id::text`,
				operatorID, in.Capability, in.Input, expected, in.Note, in.SourceItem).Scan(&id); err != nil {
				return nil, addLogicTestOut{}, internal("add_logic_test", err)
			}
			return nil, addLogicTestOut{
				logicTestOut: logicTestOut{ID: id, Input: in.Input, Expected: expected, Note: in.Note, SourceItem: in.SourceItem},
				NextActions: []string{"thêm ví dụ đủ che các trường hợp đặc biệt (mùa lễ, ghế lạ…)",
					"run_examples_against (capability=" + in.Capability + ") khi đã có ứng viên"}}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_logic_tests", Description: listLogicTestsDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listLogicTestsIn) (*mcp.CallToolResult, listLogicTestsOut, error) {
			rows, err := s.db.Query(ctx, `
				SELECT id::text, input::text, expected::text, coalesce(note, ''),
				       coalesce(source_item_id::text, ''), coalesce(commit, '')
				FROM logic_tests
				WHERE operator_id = $1 AND ($2 = '' OR capability = $2)
				ORDER BY capability, note LIMIT 100`, operatorID, in.Capability)
			if err != nil {
				return nil, listLogicTestsOut{}, internal("list_logic_tests", err)
			}
			defer rows.Close()
			out := listLogicTestsOut{Tests: []logicTestOut{}}
			for rows.Next() {
				var t logicTestOut
				var inp, exp string
				if err := rows.Scan(&t.ID, &inp, &exp, &t.Note, &t.SourceItem, &t.Commit); err != nil {
					return nil, listLogicTestsOut{}, internal("list_logic_tests", err)
				}
				_ = json.Unmarshal([]byte(inp), &t.Input)
				_ = json.Unmarshal([]byte(exp), &t.Expected)
				out.Tests = append(out.Tests, t)
			}
			if len(out.Tests) > 0 {
				out.NextActions = []string{"run_examples_against để thử code ứng viên trên các ví dụ này"}
			}
			return nil, out, nil
		})
}

type reflectIn struct {
	Question string `json:"question" jsonschema:"câu hỏi phân tích (vd 'chính sách huỷ vé của nhà xe có gì khác thông lệ?')"`
}

const (
	reflectDesc = "Agent phân tích tri thức cho builder: trả lời câu hỏi dựa trên tri thức đã duyệt, " +
		"mỗi nhận định kèm trích dẫn [[id]] (kiểm tự động: id không hợp lệ / câu số liệu thiếu trích dẫn → cảnh báo). " +
		"Async: operation_id."
	compareIndustryDesc = "Chỗ nhà xe khác thông lệ ngành: so observation của nhà xe với thông lệ L1 " +
		"theo từng topic (từ job consolidate)."
)

func (s *Server) addReflectTools(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "reflect", Description: reflectDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reflectIn) (*mcp.CallToolResult, testJobOut, error) {
			if strings.TrimSpace(in.Question) == "" {
				return nil, testJobOut{}, errors.New("thiếu question")
			}
			opID, _, err := queue.Enqueue(ctx, s.db, queue.Job{Kind: "bot.reflect", OperatorID: operatorID,
				Payload: map[string]string{"question": in.Question}})
			if err != nil {
				return nil, testJobOut{}, internal("reflect", err)
			}
			return nil, testJobOut{OperationID: opID, NextActions: []string{
				"get_operation(operation_id=" + opID + ") — câu trả lời có trích dẫn + warnings"}}, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "compare_with_industry", Description: compareIndustryDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, compareIndustryOut, error) {
			out, err := compareIndustry(ctx, s.db, operatorID)
			if err != nil {
				return nil, compareIndustryOut{}, internal("compare_with_industry", err)
			}
			return nil, out, nil
		})
}

type compareIndustryOut struct {
	Topics []industryTopic `json:"topics"`
}

type industryTopic struct {
	Topic    string   `json:"topic"`
	Operator []string `json:"operator"`
	Industry []string `json:"industry,omitempty"`
}

func compareIndustry(ctx context.Context, db *pgxpool.Pool, operator string) (compareIndustryOut, error) {
	rows, err := db.Query(ctx, `
		WITH own AS (
			SELECT topic, text FROM items
			WHERE operator_id = $1 AND kind = 'observation' AND layer = 2 AND status = 'active'
		), l1 AS (
			SELECT topic, text FROM items
			WHERE operator_id IS NULL AND kind = 'observation' AND layer = 1 AND status = 'active'
		)
		SELECT o.topic, o.text, l1.text FROM own o LEFT JOIN l1 ON l1.topic = o.topic`, operator)
	if err != nil {
		return compareIndustryOut{}, err
	}
	defer rows.Close()
	out := compareIndustryOut{Topics: []industryTopic{}}
	for rows.Next() {
		var t industryTopic
		var own, ind *string
		if err := rows.Scan(&t.Topic, &own, &ind); err != nil {
			return compareIndustryOut{}, err
		}
		t.Operator = lines(own)
		t.Industry = lines(ind)
		out.Topics = append(out.Topics, t)
	}
	return out, rows.Err()
}

func lines(s *string) []string {
	if s == nil {
		return []string{}
	}
	var out []string
	for _, l := range strings.Split(*s, "\n") {
		if strings.HasPrefix(l, "- ") {
			out = append(out, l[2:])
		}
	}
	return out
}
