package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/artifact"
	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/pack"
)

// ─────────────────────────────── kiểu vào/ra ───────────────────────────────

type packIn struct {
	Purpose      string `json:"purpose,omitempty" jsonschema:"build (mặc định) | faq | logic | review — quyết định phần nào ưu tiên khi cắt theo ngân sách"`
	BudgetTokens int    `json:"budget_tokens,omitempty" jsonschema:"ngân sách token của gói (mặc định 6000, tối đa 30000)"`
	AsOf         string `json:"as_of,omitempty" jsonschema:"YYYY-MM-DD: tri thức hiệu lực vào ngày này; mặc định hôm nay"`
}

type specIn struct {
	Channel string `json:"channel,omitempty" jsonschema:"kênh của bot: zalo (mặc định) | messenger | web"`
}

type artifactReq struct {
	Kind    string `json:"kind"`
	Level   string `json:"level"` // required | recommended
	Purpose string `json:"purpose"`
	Status  string `json:"status"` // missing | draft | valid | invalid | stale | published
	Version int    `json:"version,omitempty"`
}

type sectionSpec struct {
	Topic     string   `json:"topic"`
	Title     string   `json:"title"`
	Level     string   `json:"level"`
	Facts     []string `json:"facts"`
	Questions []string `json:"questions,omitempty"`
	Coverage  string   `json:"coverage"` // operator | industry_default | missing
	Items     int      `json:"items"`    // số item active của nhà xe
}

type lockedRule struct {
	ID    string `json:"id"`
	Layer string `json:"layer"`
	Key   string `json:"key"`
	Text  string `json:"text"`
}

type botSpecOut struct {
	Industry        string          `json:"industry"`
	TemplateVersion int             `json:"template_version"`
	BotID           string          `json:"bot_id"`
	Channel         string          `json:"channel"`
	Artifacts       []artifactReq   `json:"artifacts"`
	Sections        []sectionSpec   `json:"sections"`
	Capabilities    []kb.Capability `json:"capabilities"`
	LockedRules     []lockedRule    `json:"locked_rules"`
	CitationGuide   []string        `json:"citation_guide"`
	NextActions     []string        `json:"next_actions"`
}

type saveIn struct {
	Kind        string `json:"kind" jsonschema:"persona | system_prompt | faq | flows | tool_spec | fallbacks"`
	Content     string `json:"content" jsonschema:"toàn bộ nội dung artifact (markdown); mỗi câu mang thông tin có [[item_id]]"`
	Channel     string `json:"channel,omitempty" jsonschema:"zalo (mặc định) | messenger | web"`
	Note        string `json:"note,omitempty" jsonschema:"đã sửa gì so với version trước (ngắn)"`
	BaseVersion int    `json:"base_version,omitempty" jsonschema:"version bạn đã sửa từ đó (từ get_artifact); có version mới hơn thì báo xung đột thay vì ghi đè"`
}

type saveOut struct {
	artifact.SaveResult
	NextActions []string `json:"next_actions"`
}

type getArtifactIn struct {
	Kind    string `json:"kind" jsonschema:"persona | system_prompt | faq | flows | tool_spec | fallbacks"`
	Channel string `json:"channel,omitempty" jsonschema:"zalo (mặc định) | messenger | web"`
	Version int    `json:"version,omitempty" jsonschema:"bỏ trống = bản mới nhất"`
}

type listArtifactsIn struct {
	Channel string `json:"channel,omitempty" jsonschema:"lọc theo kênh"`
}

type listArtifactsOut struct {
	Artifacts []artifact.Summary `json:"artifacts"`
	Missing   []string           `json:"missing_required" jsonschema:"artifact bắt buộc chưa có (kênh zalo hoặc kênh được lọc)"`
}

const (
	packDesc = "Gói tri thức đã xử lý sẵn để viết bot — gọi TRƯỚC khi viết artifact thay vì recall nhiều lần. " +
		"Gồm: rules (quy tắc bắt buộc L0/L1, luôn có), persona, policies (của nhà xe; thông lệ L1 chỉ khi nhà xe " +
		"chưa có, mang nhãn 'thông lệ chung'), lessons, data_summary (chỉ tóm tắt — con số tra query_data, bot " +
		"gọi tool lúc chạy), gaps (mục còn thiếu / đang dùng thông lệ). Mỗi mục có id để trích dẫn [[id]]."
	specDesc = "Bot cần những gì: artifact bắt buộc/khuyến nghị (kèm trạng thái hiện có), mục tri thức theo template " +
		"ngành (độ phủ của nhà xe), capability cần có, quy tắc bắt buộc phải xuất hiện trong system_prompt, và " +
		"hướng dẫn trích dẫn. Gọi đầu tiên khi build bot."
	saveDesc = "Lưu một artifact của bot (version mới, không ghi đè; nội dung y hệt bản hiện tại thì không tạo " +
		"version). Trích dẫn dạng [[item_id]] được kiểm ngay: id phải là item của nhà xe này hoặc tri thức nền; " +
		"item đã hết hiệu lực trả về warnings. Khi sửa bản có sẵn, gửi base_version."
	getArtifactDesc   = "Nội dung một artifact (bản mới nhất hoặc version chỉ định), trích dẫn theo dòng, lịch sử version."
	listArtifactsDesc = "Các artifact hiện có của bot (version mới nhất, trạng thái, số trích dẫn) và artifact bắt buộc còn thiếu."
)

var artifactPurpose = map[string]string{
	"persona":       "xưng hô, giọng, phong cách theo kênh",
	"system_prompt": "hướng dẫn chính cho bot; phải chứa mọi quy tắc bắt buộc (locked_rules) kèm [[id]]",
	"faq":           "cặp hỏi–đáp chuẩn; mỗi câu trả lời mang thông tin có [[id]]",
	"flows":         "kịch bản nghiệp vụ: hỏi giá, đặt vé, huỷ, khiếu nại…",
	"tool_spec":     "bot gọi tool nào, khi nào (giá, lịch, ghế…); mỗi tool ứng với một capability",
	"fallbacks":     "câu trả lời khi thiếu dữ liệu (gaps) hoặc ngoài phạm vi — không bịa",
}

var citationGuide = []string{
	"Mỗi câu mang thông tin gắn [[id]] của item (id từ get_knowledge_pack, recall_knowledge, query_data).",
	"Không ghi cứng giá, giờ chạy, số ghế: hướng bot gọi tool (khai báo trong tool_spec).",
	"Item nhãn 'thông lệ chung' (L1): câu phải nói rõ là thông lệ, khách nên xác nhận với nhà xe.",
	"Mọi quy tắc bắt buộc (locked_rules) phải có trong system_prompt, kèm [[id]].",
	"Mục còn thiếu tri thức (coverage = missing): viết vào fallbacks, không bịa.",
}

// ─────────────────────────────── đăng ký ───────────────────────────────

func (s *Server) addBuildTools(srv *mcp.Server, operatorID string) {
	mcp.AddTool(srv, &mcp.Tool{Name: "get_bot_spec", Description: specDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in specIn) (*mcp.CallToolResult, botSpecOut, error) {
			return s.botSpec(ctx, operatorID, in)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_knowledge_pack", Description: packDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in packIn) (*mcp.CallToolResult, pack.Pack, error) {
			day, _, err := parseDate("as_of", in.AsOf)
			if err != nil {
				return nil, pack.Pack{}, err
			}
			if strings.TrimSpace(in.AsOf) == "" {
				day = time.Time{}
			}
			if in.Purpose != "" && !slices.Contains(pack.Purposes, in.Purpose) {
				return nil, pack.Pack{}, errors.New("purpose phải là: " + strings.Join(pack.Purposes, ", "))
			}
			p, err := s.packs.Build(ctx, pack.Query{OperatorID: operatorID, Purpose: in.Purpose,
				Budget: in.BudgetTokens, Day: day})
			if err != nil {
				return nil, p, internal("get_knowledge_pack", err)
			}
			return nil, p, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "save_artifact", Description: saveDesc, Annotations: writeTool},
		func(ctx context.Context, req *mcp.CallToolRequest, in saveIn) (*mcp.CallToolResult, saveOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, saveOut{}, err
			}
			kv, err := pack.Version(ctx, s.db, operatorID)
			if err != nil {
				return nil, saveOut{}, internal("save_artifact", err)
			}
			res, err := artifact.Save(ctx, s.db, artifact.SaveInput{OperatorID: operatorID, Channel: in.Channel,
				Kind: in.Kind, Content: in.Content, Note: in.Note, BaseVersion: in.BaseVersion, Author: p.Actor(),
				KnowledgeVersion: kv})
			var inputErr *artifact.InputError
			if errors.As(err, &inputErr) || errors.Is(err, artifact.ErrConflict) {
				return nil, saveOut{}, err
			}
			if err != nil {
				return nil, saveOut{}, internal("save_artifact", err)
			}
			next := []string{}
			if len(res.Warnings) > 0 {
				next = append(next, "sửa các trích dẫn trong warnings (item đã hết hiệu lực) rồi lưu lại")
			}
			if missing, err := s.missingArtifacts(ctx, operatorID, in.Channel); err == nil && len(missing) > 0 {
				next = append(next, "artifact bắt buộc còn thiếu: "+strings.Join(missing, ", "))
			}
			return nil, saveOut{SaveResult: res, NextActions: next}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_artifact", Description: getArtifactDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getArtifactIn) (*mcp.CallToolResult, artifact.Artifact, error) {
			if !slices.Contains(artifact.Kinds, in.Kind) {
				return nil, artifact.Artifact{}, errors.New("kind phải là: " + strings.Join(artifact.Kinds, ", "))
			}
			a, err := artifact.Get(ctx, s.db, operatorID, in.Channel, in.Kind, in.Version)
			if errors.Is(err, artifact.ErrNotFound) {
				return nil, a, err
			}
			if err != nil {
				return nil, a, internal("get_artifact", err)
			}
			return nil, a, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_artifacts", Description: listArtifactsDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listArtifactsIn) (*mcp.CallToolResult, listArtifactsOut, error) {
			list, err := artifact.List(ctx, s.db, operatorID, in.Channel)
			if err != nil {
				return nil, listArtifactsOut{}, internal("list_artifacts", err)
			}
			missing, err := s.missingArtifacts(ctx, operatorID, in.Channel)
			if err != nil {
				return nil, listArtifactsOut{}, internal("list_artifacts", err)
			}
			return nil, listArtifactsOut{Artifacts: list, Missing: missing}, nil
		})
}

// ─────────────────────────────── xử lý ───────────────────────────────

func (s *Server) missingArtifacts(ctx context.Context, operatorID, channel string) ([]string, error) {
	if channel == "" {
		channel = "zalo"
	}
	list, err := artifact.List(ctx, s.db, operatorID, channel)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, a := range list {
		have[a.Kind] = true
	}
	missing := []string{}
	for _, k := range s.template.Artifacts.Required {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	return missing, nil
}

func (s *Server) botSpec(ctx context.Context, operatorID string, in specIn) (*mcp.CallToolResult, botSpecOut, error) {
	channel := in.Channel
	if channel == "" {
		channel = "zalo"
	}
	if !slices.Contains(artifact.Channels, channel) {
		return nil, botSpecOut{}, errors.New("channel phải là: " + strings.Join(artifact.Channels, ", "))
	}
	t := s.template
	out := botSpecOut{Industry: t.Industry, TemplateVersion: t.Version, BotID: artifact.BotID(operatorID, channel),
		Channel: channel, Capabilities: t.Capabilities, CitationGuide: citationGuide}
	if out.Capabilities == nil {
		out.Capabilities = []kb.Capability{}
	}

	current := map[string]artifact.Summary{}
	list, err := artifact.List(ctx, s.db, operatorID, channel)
	if err != nil {
		return nil, out, internal("get_bot_spec", err)
	}
	for _, a := range list {
		current[a.Kind] = a
	}
	addReq := func(kinds []string, level string) {
		for _, k := range kinds {
			r := artifactReq{Kind: k, Level: level, Purpose: artifactPurpose[k], Status: "missing"}
			if a, ok := current[k]; ok {
				r.Status, r.Version = a.Status, a.Version
			}
			out.Artifacts = append(out.Artifacts, r)
		}
	}
	addReq(t.Artifacts.Required, "required")
	addReq(t.Artifacts.Recommended, "recommended")

	type cov struct{ own, defaults int }
	byTopic := map[string]cov{}
	rows, err := s.db.Query(ctx, `SELECT topic, count(*) FILTER (WHERE layer = 2),
			count(*) FILTER (WHERE layer = 1 AND NOT locked)
		FROM items WHERE status = 'active' AND (layer = 1 OR (layer = 2 AND operator_id = $1)) GROUP BY topic`, operatorID)
	if err != nil {
		return nil, out, internal("get_bot_spec", err)
	}
	for rows.Next() {
		var topic string
		var c cov
		if err := rows.Scan(&topic, &c.own, &c.defaults); err != nil {
			rows.Close()
			return nil, out, internal("get_bot_spec", err)
		}
		byTopic[topic] = c
	}
	rows.Close()
	missingRequired := 0
	for _, sec := range t.Sections {
		c := byTopic[sec.Topic]
		ss := sectionSpec{Topic: sec.Topic, Title: sec.Title, Level: sec.Level, Facts: sec.Facts,
			Questions: sec.Questions, Items: c.own, Coverage: "missing"}
		switch {
		case c.own > 0:
			ss.Coverage = "operator"
		case c.defaults > 0:
			ss.Coverage = "industry_default"
		}
		if ss.Coverage != "operator" {
			ss.Questions = sec.Questions // câu hỏi để hỏi nhà xe
			if sec.Level == "required" && ss.Coverage == "missing" {
				missingRequired++
			}
		} else {
			ss.Questions = nil
		}
		if ss.Facts == nil {
			ss.Facts = []string{}
		}
		out.Sections = append(out.Sections, ss)
	}

	rows, err = s.db.Query(ctx, `SELECT id::text, layer, key, text FROM items
		WHERE status = 'active' AND layer <= 1 AND locked ORDER BY layer, key`)
	if err != nil {
		return nil, out, internal("get_bot_spec", err)
	}
	defer rows.Close()
	out.LockedRules = []lockedRule{}
	for rows.Next() {
		var r lockedRule
		var layer int
		var key *string
		if err := rows.Scan(&r.ID, &layer, &key, &r.Text); err != nil {
			return nil, out, internal("get_bot_spec", err)
		}
		r.Layer = []string{"L0", "L1"}[layer]
		if key != nil {
			r.Key = *key
		}
		out.LockedRules = append(out.LockedRules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, out, internal("get_bot_spec", err)
	}

	out.NextActions = []string{"get_knowledge_pack(purpose=build)"}
	if missingRequired > 0 {
		out.NextActions = append(out.NextActions, "mục bắt buộc còn thiếu tri thức: hỏi nhà xe theo questions, "+
			"gửi bằng submit_knowledge; tạm thời viết vào fallbacks")
	}
	out.NextActions = append(out.NextActions, "viết từng artifact theo artifacts[].purpose rồi save_artifact")
	return nil, out, nil
}
