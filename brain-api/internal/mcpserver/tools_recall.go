package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/recall"
)

// ─────────────────────────────── kiểu vào/ra ───────────────────────────────

type recallIn struct {
	Query     string   `json:"query,omitempty" jsonschema:"câu hỏi/chủ đề cần tìm, tiếng Việt có dấu hoặc không; bỏ trống thì phải có topics"`
	Topics    []string `json:"topics,omitempty" jsonschema:"chỉ lấy các topic này"`
	Kinds     []string `json:"kinds,omitempty" jsonschema:"chỉ lấy loại: data | policy | lesson | persona | observation"`
	ValidAt   string   `json:"valid_at,omitempty" jsonschema:"YYYY-MM-DD: tri thức có hiệu lực vào ngày này (vd ngày đi); mặc định hôm nay"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"ngân sách token của kết quả (mặc định 2000, tối đa 8000)"`
}

type recallOut struct {
	recall.Result
	NextActions []string `json:"next_actions"`
}

type queryDataIn struct {
	Topics          []string          `json:"topics,omitempty" jsonschema:"vd route, fare, schedule, pickup; bỏ trống = mọi data"`
	Match           string            `json:"match,omitempty" jsonschema:"các từ phải có đủ trong key/nội dung/facts, không phân biệt dấu, vd 'đà lạt giường nằm'"`
	Facts           map[string]string `json:"facts,omitempty" jsonschema:"lọc theo fact, vd {\"diem_den\": \"Đà Lạt\"}"`
	Date            string            `json:"date,omitempty" jsonschema:"YYYY-MM-DD: data có hiệu lực ngày này (vd ngày đi); mặc định hôm nay"`
	IncludeUpcoming bool              `json:"include_upcoming,omitempty" jsonschema:"thêm bản sẽ có hiệu lực sau ngày đó (giá/lịch mới đã chốt)"`
	Limit           int               `json:"limit,omitempty" jsonschema:"tối đa (mặc định 100, tối đa 200)"`
}

type sourceIn struct {
	ItemID string `json:"item_id" jsonschema:"id của item (từ recall_knowledge, query_data, list_knowledge, trích dẫn [[id]])"`
}

type overviewIn struct{}

const (
	recallDesc = "Tìm tri thức để viết/sửa bot: theo ngữ nghĩa + từ khoá, chỉ item đang hiệu lực tại valid_at, gồm " +
		"tầng nền tảng (L0), ngành (L1) và của nhà xe (L2). Mỗi kết quả có id (dùng để trích dẫn [[id]]), tầng, nhãn " +
		"và nguồn. Item L1 nhãn 'thông lệ chung' là mặc định của ngành, CHƯA được nhà xe xác nhận — khi dùng phải nói " +
		"rõ như vậy; item L0/L1 'bắt buộc' luôn phải tuân theo. Cần con số (giá, giờ, tuyến) thì dùng query_data."
	queryDataDesc = "Tra data vận hành của nhà xe (tuyến, giá vé, lịch chạy, điểm đón...) có hiệu lực vào một ngày — " +
		"lọc chính xác theo topic, từ khoá và facts. Dùng khi cần con số đúng; không đoán giá/giờ. Bot thật vẫn phải " +
		"gọi tool của nhà xe lúc chạy, không ghi cứng con số vào artifact."
	sourceDesc = "Nguồn gốc của một item: tin/tài liệu nhà xe gửi (nguyên văn, kênh, thời điểm, ai gửi), ai duyệt, " +
		"các lần nhà xe nhắc lại; item L0/L1 trả file kb/. Dùng khi cần kiểm chứng hoặc giải thích vì sao Brain biết."
	overviewDesc = "Tổng quan nhà xe: trạng thái, số item theo trạng thái, độ phủ theo template ngành (mục bắt buộc còn " +
		"thiếu), đề xuất chờ duyệt, job đang chạy/lỗi. Gọi đầu phiên làm việc để biết bước tiếp theo."
)

var itemKinds = []string{"data", "policy", "lesson", "persona", "observation"}

// ─────────────────────────────── đăng ký ───────────────────────────────

func (s *Server) addRecallTools(srv *mcp.Server, operatorID string) {
	topicLine := " Topic: " + s.topicGuide() + "."
	mcp.AddTool(srv, &mcp.Tool{Name: "recall_knowledge", Description: recallDesc + topicLine, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in recallIn) (*mcp.CallToolResult, recallOut, error) {
			return s.recallKnowledge(ctx, operatorID, in)
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "query_data", Description: queryDataDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in queryDataIn) (*mcp.CallToolResult, recall.DataResult, error) {
			if err := s.checkTopics(in.Topics); err != nil {
				return nil, recall.DataResult{}, err
			}
			at, until, err := parseDate("date", in.Date)
			if err != nil {
				return nil, recall.DataResult{}, err
			}
			res, err := recall.QueryData(ctx, s.db, recall.DataQuery{OperatorID: operatorID, Topics: in.Topics,
				Match: in.Match, Facts: in.Facts, At: at, Until: until, IncludeUpcoming: in.IncludeUpcoming, Limit: in.Limit})
			if err != nil {
				return nil, res, internal("query_data", err)
			}
			return nil, res, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_source", Description: sourceDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in sourceIn) (*mcp.CallToolResult, recall.SourceResult, error) {
			id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(in.ItemID), "[["), "]]")
			if !isUUID(id) {
				return nil, recall.SourceResult{}, errors.New("item_id phải là UUID")
			}
			res, err := recall.GetSource(ctx, s.db, operatorID, id)
			if errors.Is(err, recall.ErrNotFound) {
				return nil, res, err
			}
			if err != nil {
				return nil, res, internal("get_source", err)
			}
			return nil, res, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "get_operator_overview", Description: overviewDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ overviewIn) (*mcp.CallToolResult, recall.Overview, error) {
			specs := make([]recall.TopicSpec, len(s.topics))
			for i, t := range s.topics {
				specs[i] = recall.TopicSpec{ID: t.ID, Title: t.Title, Required: t.Required}
			}
			ov, err := recall.GetOverview(ctx, s.db, operatorID, specs)
			if err != nil {
				return nil, ov, internal("get_operator_overview", err)
			}
			return nil, ov, nil
		})
}

// ─────────────────────────────── xử lý ───────────────────────────────

func (s *Server) recallKnowledge(ctx context.Context, operatorID string, in recallIn) (*mcp.CallToolResult, recallOut, error) {
	if err := s.checkTopics(in.Topics); err != nil {
		return nil, recallOut{}, err
	}
	for _, k := range in.Kinds {
		if !slices.Contains(itemKinds, k) {
			return nil, recallOut{}, errors.New("kinds chỉ gồm: " + strings.Join(itemKinds, ", "))
		}
	}
	at, until, err := parseDate("valid_at", in.ValidAt)
	if err != nil {
		return nil, recallOut{}, err
	}
	if in.MaxTokens > 8000 {
		in.MaxTokens = 8000
	}
	res, err := s.recaller.Recall(ctx, recall.Query{OperatorID: operatorID, Text: in.Query, Topics: in.Topics,
		Kinds: in.Kinds, ValidAt: at, ValidUntil: until, MaxTokens: in.MaxTokens})
	if errors.Is(err, recall.ErrEmptyQuery) {
		return nil, recallOut{}, err
	}
	if err != nil {
		return nil, recallOut{}, internal("recall_knowledge", err)
	}
	next := []string{"trích dẫn [[id]] khi dùng một item trong artifact"}
	if res.Truncated {
		next = append(next, "thu hẹp bằng topics/kinds hoặc tăng max_tokens để xem phần bị cắt")
	}
	if len(res.Hits) == 0 {
		next = []string{"thử query khác hoặc bỏ bớt bộ lọc", "get_operator_overview để xem mục còn thiếu"}
	}
	return nil, recallOut{Result: res, NextActions: next}, nil
}

func (s *Server) checkTopics(topics []string) error {
	ids := s.topicIDs()
	for _, t := range topics {
		if t != "other" && !slices.Contains(ids, t) {
			return errors.New("topic không hợp lệ: " + t + ". Topic: " + s.topicGuide())
		}
	}
	return nil
}

var ict = time.FixedZone("ICT", 7*3600)

// parseDate: "YYYY-MM-DD" → cả ngày đó theo giờ VN [00:00, 23:59:59.999999]: item còn hiệu lực lúc nào đó trong
// ngày là khớp (giá mới từ 01/11 khớp ngày 01/11; bản cũ hết hạn 31/10 thì không); trống → bây giờ.
func parseDate(field, v string) (time.Time, time.Time, error) {
	if strings.TrimSpace(v) == "" {
		now := time.Now()
		return now, now, nil
	}
	d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(v), ict)
	if err != nil {
		return d, d, errors.New(field + " phải dạng YYYY-MM-DD")
	}
	return d, d.Add(24*time.Hour - time.Microsecond), nil
}
