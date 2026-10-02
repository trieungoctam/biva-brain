// Tools /mcp/platform/ (M5, S5.1.1): list_operators, list_promotion_candidates, propose_l1_change.
// Endpoint chỉ dành cho lead (requirePlatform) — đổi L0/L1 là quyết định của lead, AI chỉ đề xuất.
package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/confirm"
	"github.com/trieungoctam/biva-brain/brain-api/internal/textnorm"
)

type operatorSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Items        int    `json:"items_active"`
	Bots         int    `json:"bots"`
	OpenReviews  int    `json:"open_reviews"`
	PublishedRel int    `json:"published_releases"`
}

type listOperatorsOut struct {
	Operators   []operatorSummary `json:"operators"`
	NextActions []string          `json:"next_actions"`
}

type promotionCandidate struct {
	Kind       string   `json:"kind"` // knowledge | logic
	Topic      string   `json:"topic,omitempty"`
	Capability string   `json:"capability,omitempty"`
	Operators  []string `json:"operators"`
	ReviewID   string   `json:"review_id,omitempty"` // đề xuất đã mở (knowledge)
	FamilyID   string   `json:"family_id,omitempty"` // họ logic đang là ứng viên
	Summary    string   `json:"summary"`
}

type promotionOut struct {
	Candidates  []promotionCandidate `json:"candidates"`
	NextActions []string             `json:"next_actions"`
}

type proposeL1In struct {
	Topic        string   `json:"topic" jsonschema:"topic thuộc template ngành"`
	Text         string   `json:"text" jsonschema:"nội dung thông lệ L1"`
	Key          string   `json:"key,omitempty" jsonschema:"mặc định l1.<topic>.<slug từ text>"`
	Operators    []string `json:"operators,omitempty" jsonschema:"nhà xe minh hoạ (lưu vào reason)"`
	ConfirmToken string   `json:"confirm_token,omitempty"`
}

const (
	listOperatorsDesc = "Danh sách nhà xe: số item active, bot, đề xuất đang chờ, bản phát hành " +
		"đang chạy — nhìn nhanh toàn danh mục."
	promotionDesc = "Ứng viên promote: tri thức (review PROMOTE đang chờ duyệt — observation giống " +
		"nhau ≥ 3 nhà xe) và logic (họ có ≥ 3 nhà xe hook/custom). Kèm danh sách nhà xe."
	proposeL1Desc = "Lead đề xuất sửa/thêm thông lệ L1 (tạo item L1 pending + review chờ duyệt như " +
		"mọi đề xuất). Cần confirm_token."
)

type proposeL1Out struct {
	ItemID      string   `json:"item_id"`
	ReviewID    string   `json:"review_id"`
	NextActions []string `json:"next_actions"`
}

func (s *Server) addPlatformMgmt(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_operators", Description: listOperatorsDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listOperatorsOut, error) {
			rows, err := s.db.Query(ctx, `
				SELECT o.id, o.name,
				       (SELECT count(*) FROM items i WHERE i.operator_id = o.id AND i.status = 'active'
				         AND i.kind <> 'observation'),
				       (SELECT count(*) FROM bots b WHERE b.operator_id = o.id),
				       (SELECT count(*) FROM review_items r WHERE r.operator_id = o.id AND r.status = 'open'),
				       (SELECT count(*) FROM releases rel WHERE rel.operator_id = o.id
				         AND rel.stage = 'production' AND rel.status = 'published')
				FROM operators o ORDER BY o.id`)
			if err != nil {
				return nil, listOperatorsOut{}, internal("list_operators", err)
			}
			defer rows.Close()
			out := listOperatorsOut{Operators: []operatorSummary{}}
			for rows.Next() {
				var o operatorSummary
				if err := rows.Scan(&o.ID, &o.Name, &o.Items, &o.Bots, &o.OpenReviews, &o.PublishedRel); err != nil {
					return nil, listOperatorsOut{}, internal("list_operators", err)
				}
				out.Operators = append(out.Operators, o)
			}
			out.NextActions = []string{"list_promotion_candidates"}
			return nil, out, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "list_promotion_candidates", Description: promotionDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, promotionOut, error) {
			out := promotionOut{Candidates: []promotionCandidate{}}
			// Tri thức: review PROMOTE đang mở (job promote đã gom observation ≥ 3 nhà xe).
			rows, err := s.db.Query(ctx, `
				SELECT r.id::text, r.topic, coalesce(r.after->>'operators', '[]'), r.reason
				FROM review_items r WHERE r.change_kind = 'PROMOTE' AND r.status = 'open'
				ORDER BY r.created_at`)
			if err != nil {
				return nil, promotionOut{}, internal("list_promotion_candidates", err)
			}
			for rows.Next() {
				var c promotionCandidate
				var ops string
				if err := rows.Scan(&c.ReviewID, &c.Topic, &ops, &c.Summary); err != nil {
					rows.Close()
					return nil, promotionOut{}, internal("list_promotion_candidates", err)
				}
				c.Kind = "knowledge"
				c.Operators = splitJSONStrings(ops)
				out.Candidates = append(out.Candidates, c)
			}
			rows.Close()
			// Logic: họ promote_candidate.
			fams, err := s.db.Query(ctx, `
				SELECT id, capability, members::text FROM logic_families WHERE promote_candidate`)
			if err != nil {
				return nil, promotionOut{}, internal("list_promotion_candidates", err)
			}
			for fams.Next() {
				var c promotionCandidate
				var members string
				if err := fams.Scan(&c.FamilyID, &c.Capability, &members); err != nil {
					fams.Close()
					return nil, promotionOut{}, internal("list_promotion_candidates", err)
				}
				c.Kind = "logic"
				c.Operators = splitJSONStrings(members)
				c.Summary = "≥ 3 nhà xe cùng hook/custom — đề xuất đưa lên tham số/hook chuẩn"
				out.Candidates = append(out.Candidates, c)
			}
			fams.Close()
			out.NextActions = []string{"knowledge: apply_review sau khi đọc; logic: đề xuất thêm hook chuẩn vào kb/"}
			return nil, out, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "propose_l1_change", Description: proposeL1Desc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in proposeL1In) (*mcp.CallToolResult, proposeL1Out, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, proposeL1Out{}, err
			}
			if strings.TrimSpace(in.Text) == "" || in.Topic == "" {
				return nil, proposeL1Out{}, errors.New("cần topic và text")
			}
			key := in.Key
			if key == "" {
				key = "l1." + in.Topic + "." + slug(in.Text)
			}
			subject := confirm.Subject("propose_l1_change", in.Topic, key)
			if in.ConfirmToken == "" {
				token, exp, err := confirm.Issue(ctx, s.db, p.UserID, "", "propose_l1_change", subject, confirm.DefaultTTL)
				if err != nil {
					return nil, proposeL1Out{}, internal("propose_l1_change", err)
				}
				return nil, proposeL1Out{NextActions: []string{
					"preview: thêm thông lệ L1 " + key + " — \"" + clipText(in.Text, 80) + "\"",
					"đồng ý thì gọi lại với confirm_token=" + token + " (hết hạn " + exp.Format("15:04:05") + ")",
				}}, nil
			}
			if err := confirm.Consume(ctx, s.db, in.ConfirmToken, p.UserID, "", "propose_l1_change", subject); err != nil {
				return nil, proposeL1Out{}, err
			}
			tx, err := s.db.Begin(ctx)
			if err != nil {
				return nil, proposeL1Out{}, internal("propose_l1_change", err)
			}
			defer tx.Rollback(ctx)
			var itemID, reviewID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO items (layer, kind, topic, key, text, status, metadata)
				VALUES (1, 'policy', $1, $2, $3, 'pending', $4::jsonb) RETURNING id::text`,
				in.Topic, key, in.Text, map[string]any{"source": "propose_l1_change", "operators": in.Operators}).
				Scan(&itemID); err != nil {
				return nil, proposeL1Out{}, internal("propose_l1_change", err)
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO review_items (operator_id, key, topic, change_kind, risk, item_id, reason, proposed_by)
				VALUES ((SELECT id FROM operators ORDER BY id LIMIT 1), $1, $2, 'PROMOTE', 'high', $3::uuid, $4, $5)
				RETURNING id::text`,
				key, in.Topic, itemID, "lead đề xuất L1: "+clipText(in.Text, 120), p.Actor()).
				Scan(&reviewID); err != nil {
				return nil, proposeL1Out{}, internal("propose_l1_change", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, proposeL1Out{}, internal("propose_l1_change", err)
			}
			return nil, proposeL1Out{ItemID: itemID, ReviewID: reviewID,
				NextActions: []string{"review đã mở — apply_review khi chốt (hoặc để lead khác duyệt)"}}, nil
		})
}

func splitJSONStrings(s string) []string {
	s = strings.Trim(s, "[]\" ")
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.Trim(strings.TrimSpace(p), "\""); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(textnorm.Fold(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 24 {
		out = out[:24]
	}
	if out == "" {
		out = "x"
	}
	return out
}
