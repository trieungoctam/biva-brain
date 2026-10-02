// Tools /mcp/platform/ (M5, S5.1.1): list_operators, list_promotion_candidates, propose_l1_change.
// Endpoint chỉ dành cho lead (requirePlatform) — đổi L0/L1 là quyết định của lead, AI chỉ đề xuất.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/confirm"
	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
	"github.com/trieungoctam/biva-brain/brain-api/internal/release"
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

type impactIn struct {
	ItemID    string `json:"item_id,omitempty" jsonschema:"id item tri thức vừa đổi (nhận cả [[id]])"`
	ModuleID  string `json:"module_id,omitempty" jsonschema:"vd fare.standard — kể cả đổi version"`
	FeatureID string `json:"feature_id,omitempty" jsonschema:"vd fare.holiday_surcharge"`
}

type impacted struct {
	Kind   string `json:"kind"` // operator | artifact | profile | test | family
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type impactOut struct {
	Impacted    []impacted `json:"impacted"`
	Count       int        `json:"count"`
	NextActions []string   `json:"next_actions"`
}

const impactDesc = "Đổi tri thức/module/feature sẽ ảnh hưởng gì: nhà xe nào, artifact nào đang trích dẫn " +
	"(sẽ stale), hồ sơ logic dùng tham số từ item, test nào phụ thuộc, họ logic nào chứa feature. " +
	"Truyền đúng một trong item_id / module_id / feature_id."

func (s *Server) addImpactTool(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "impact_of_change", Description: impactDesc, Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in impactIn) (*mcp.CallToolResult, impactOut, error) {
			given := 0
			for _, v := range []string{in.ItemID, in.ModuleID, in.FeatureID} {
				if v != "" {
					given++
				}
			}
			if given != 1 {
				return nil, impactOut{}, errors.New("truyền đúng MỘT trong item_id / module_id / feature_id")
			}
			out := impactOut{Impacted: []impacted{}}
			add := func(kind, id, reason string) {
				out.Impacted = append(out.Impacted, impacted{Kind: kind, ID: id, Reason: reason})
			}
			switch {
			case in.ItemID != "":
				id := strings.Trim(in.ItemID, "[]")
				if !isUUID(id) {
					return nil, impactOut{}, errors.New("item_id phải là UUID (nhận cả [[id]])")
				}
				rows, err := s.db.Query(ctx, `
					SELECT b.operator_id, a.kind, a.status FROM artifact_citations c
					JOIN bot_artifacts a ON a.id = c.artifact_id
					JOIN bots b ON b.id = a.bot_id
					WHERE c.item_id = $1::uuid
					  AND a.id IN (SELECT DISTINCT ON (kind) id FROM bot_artifacts x
					               WHERE x.bot_id = a.bot_id AND x.kind = a.kind ORDER BY kind, version DESC)`,
					id)
				if err != nil {
					return nil, impactOut{}, internal("impact_of_change", err)
				}
				for rows.Next() {
					var op, kind, status string
					if err := rows.Scan(&op, &kind, &status); err != nil {
						rows.Close()
						return nil, impactOut{}, internal("impact_of_change", err)
					}
					add("artifact", op+"/"+kind, "đang trích dẫn item — sẽ stale khi item đổi (hiện "+status+")")
				}
				rows.Close()
				prows, err := s.db.Query(ctx, `
					SELECT DISTINCT p.operator_id, p.capability FROM logic_param_sources s
					JOIN logic_profiles p ON p.id = s.profile_id WHERE s.item_id = $1::uuid`, id)
				if err != nil {
					return nil, impactOut{}, internal("impact_of_change", err)
				}
				for prows.Next() {
					var op, cap string
					if err := prows.Scan(&op, &cap); err != nil {
						prows.Close()
						return nil, impactOut{}, internal("impact_of_change", err)
					}
					add("profile", op+"/"+cap, "tham số lấy nguồn từ item — hồ sơ sẽ stale")
				}
				prows.Close()
				trows, err := s.db.Query(ctx, `
					SELECT DISTINCT t.operator_id, t.capability FROM logic_tests t
					WHERE t.source_item_id = $1::uuid AND t.operator_id IS NOT NULL`, id)
				if err != nil {
					return nil, impactOut{}, internal("impact_of_change", err)
				}
				for trows.Next() {
					var op, cap string
					if err := trows.Scan(&op, &cap); err != nil {
						trows.Close()
						return nil, impactOut{}, internal("impact_of_change", err)
					}
					add("test", op+"/"+cap, "logic test lấy item làm nguồn — chạy lại để xác nhận")
				}
				trows.Close()
			case in.ModuleID != "":
				rows, err := s.db.Query(ctx, `
					SELECT p.operator_id, p.capability, p.module_version FROM logic_profiles p
					WHERE p.module_id = $1`, in.ModuleID)
				if err != nil {
					return nil, impactOut{}, internal("impact_of_change", err)
				}
				for rows.Next() {
					var op, cap string
					var ver int
					if err := rows.Scan(&op, &cap, &ver); err != nil {
						rows.Close()
						return nil, impactOut{}, internal("impact_of_change", err)
					}
					add("profile", op+"/"+cap, fmt.Sprintf("đang chạy %s@%d — kiểm tra tương thích khi nâng version", in.ModuleID, ver))
				}
				rows.Close()
			default: // feature
				rows, err := s.db.Query(ctx, `
					SELECT s.operator_id, s.capability FROM logic_specs s
					WHERE s.status = 'active' AND s.features::text LIKE '%' || $1 || '%'`, in.FeatureID)
				if err != nil {
					return nil, impactOut{}, internal("impact_of_change", err)
				}
				for rows.Next() {
					var op, cap string
					if err := rows.Scan(&op, &cap); err != nil {
						rows.Close()
						return nil, impactOut{}, internal("impact_of_change", err)
					}
					add("spec", op+"/"+cap, "spec chứa feature — so lại khi feature đổi")
				}
				rows.Close()
				frows, err := s.db.Query(ctx, `
					SELECT id, members::text FROM logic_families
					WHERE $1 = ANY(centroid_features) OR $1 = ANY(members)`, in.FeatureID)
				if err != nil {
					return nil, impactOut{}, internal("impact_of_change", err)
				}
				for frows.Next() {
					var fid, members string
					if err := frows.Scan(&fid, &members); err != nil {
						frows.Close()
						return nil, impactOut{}, internal("impact_of_change", err)
					}
					add("family", fid, "họ chứa feature — gom lại khi thay đổi")
				}
				frows.Close()
			}
			out.Count = len(out.Impacted)
			if out.Count > 0 {
				out.NextActions = []string{"thông báo nhà xe + run_tests lại bot liên quan"}
			} else {
				out.NextActions = []string{"không có ảnh hưởng nào được ghi nhận"}
			}
			return nil, out, nil
		})
}

type regressionOut struct {
	Operations  []map[string]string `json:"operations"`
	Count       int                 `json:"count"`
	NextActions []string            `json:"next_actions"`
}

const regressionDesc = "Chạy regression TOÀN BỘ nhà xe có snapshot (bot.tests cho từng nhà xe, chạy " +
	"song song qua queue). Trả danh sách operation_id theo nhà xe — theo dõi bằng get_operation."

func (s *Server) addRegressionTool(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "run_regression_all", Description: regressionDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, regressionOut, error) {
			rows, err := s.db.Query(ctx, `
				SELECT DISTINCT b.operator_id, b.channel FROM snapshots s
				JOIN bots b ON b.id = s.bot_id`)
			if err != nil {
				return nil, regressionOut{}, internal("run_regression_all", err)
			}
			var targets [][2]string
			for rows.Next() {
				var op, ch string
				if err := rows.Scan(&op, &ch); err != nil {
					rows.Close()
					return nil, regressionOut{}, internal("run_regression_all", err)
				}
				targets = append(targets, [2]string{op, ch})
			}
			rows.Close()
			out := regressionOut{Operations: []map[string]string{}}
			for _, t := range targets {
				opID, _, err := queue.Enqueue(ctx, s.db, queue.Job{
					Kind: "bot.tests", OperatorID: t[0],
					Payload: map[string]string{"channel": t[1]},
				})
				if err != nil {
					return nil, regressionOut{}, internal("run_regression_all", err)
				}
				out.Operations = append(out.Operations, map[string]string{
					"operator": t[0], "channel": t[1], "operation_id": opID})
			}
			out.Count = len(out.Operations)
			out.NextActions = []string{"get_operation(operation_id=…) cho từng nhà xe khi job xong"}
			return nil, out, nil
		})
}

type approvePublishIn struct {
	ReleaseID    string `json:"release_id,omitempty" jsonschema:"id bản production đang requested"`
	ConfirmToken string `json:"confirm_token,omitempty"`
}

type pendingRelease struct {
	Operator    string `json:"operator"`
	Channel     string `json:"channel"`
	ReleaseID   string `json:"release_id"`
	SnapshotVer int    `json:"snapshot_version"`
	RequestedBy string `json:"requested_by"`
	RequestedAt string `json:"requested_at"`
}

const approvePublishDesc = "Lead duyệt bản phát hành production đang chờ (request_publish stage=production). " +
	"Bỏ trống release_id → xem danh sách chờ duyệt + confirm_token; gửi lại kèm release_id để duyệt " +
	"— bản cũ tự rolled_back."

type approvePublishOut struct {
	Pending     []pendingRelease `json:"pending,omitempty"`
	Approved    string           `json:"approved_release,omitempty"`
	NextActions []string         `json:"next_actions"`
}

func (s *Server) addApprovePublish(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "approve_publish", Description: approvePublishDesc,
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}},
		func(ctx context.Context, req *mcp.CallToolRequest, in approvePublishIn) (*mcp.CallToolResult, approvePublishOut, error) {
			p, err := callerOf(req)
			if err != nil {
				return nil, approvePublishOut{}, err
			}
			if in.ConfirmToken == "" {
				rows, err := s.db.Query(ctx, `
					SELECT r.id::text, r.operator_id, r.bot_channel, r.snapshot_ver::int, r.requested_by,
					       to_char(r.requested_at, 'YYYY-MM-DD HH24:MI')
					FROM releases r WHERE r.stage = 'production' AND r.status = 'requested'
					ORDER BY r.requested_at`)
				if err != nil {
					return nil, approvePublishOut{}, internal("approve_publish", err)
				}
				defer rows.Close()
				out := approvePublishOut{Pending: []pendingRelease{}}
				for rows.Next() {
					var pr pendingRelease
					if err := rows.Scan(&pr.ReleaseID, &pr.Operator, &pr.Channel, &pr.SnapshotVer,
						&pr.RequestedBy, &pr.RequestedAt); err != nil {
						return nil, approvePublishOut{}, internal("approve_publish", err)
					}
					out.Pending = append(out.Pending, pr)
				}
				token, exp, err := confirm.Issue(ctx, s.db, p.UserID, "", "approve_publish", "*", confirm.DefaultTTL)
				if err != nil {
					return nil, approvePublishOut{}, internal("approve_publish", err)
				}
				out.NextActions = []string{"duyệt: gọi lại với release_id + confirm_token=" + token +
					" (hết hạn " + exp.Format("15:04:05") + ")"}
				return nil, out, nil
			}
			if in.ReleaseID == "" {
				return nil, approvePublishOut{}, errors.New("có confirm_token thì phải kèm release_id")
			}
			if err := confirm.Consume(ctx, s.db, in.ConfirmToken, p.UserID, "", "approve_publish", "*"); err != nil {
				return nil, approvePublishOut{}, err
			}
			status, err := release.Approve(ctx, s.db, in.ReleaseID, p.Actor())
			if err != nil {
				return nil, approvePublishOut{}, internal("approve_publish", err)
			}
			return nil, approvePublishOut{Approved: in.ReleaseID,
				NextActions: []string{"bản " + status + " — bản production cũ tự rolled_back; rollback_release nếu cần"}}, nil
		})
}
