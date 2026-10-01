package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/authz"
	"github.com/trieungoctam/biva-brain/brain-api/internal/queue"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

type fixture struct {
	pool                *pgxpool.Pool
	url                 string
	opA, opB            string
	builder, lead       string // user id
	builderTok, leadTok string
	builderTokID, jobA  string
	jobB                string
}

// setup cần BIVA_TEST_DATABASE_URL; dữ liệu có hậu tố riêng và được dọn sau test.
func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.Pool(t)
	var err error
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	f := &fixture{pool: pool, opA: "a" + sfx, opB: "b" + sfx, builder: "bu" + sfx, lead: "le" + sfx}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'A'), ($2, 'B')`, f.opA, f.opB)
	must(err)
	must(authz.CreateUser(ctx, pool, f.builder, f.builder+"@t", "Builder", authz.RoleBuilder))
	must(authz.CreateUser(ctx, pool, f.lead, f.lead+"@t", "Lead", authz.RoleLead))
	must(authz.GrantOperator(ctx, pool, f.builder, f.opA, "test"))
	f.builderTokID, f.builderTok, _, err = authz.IssueToken(ctx, pool, f.builder, "t", time.Hour)
	must(err)
	_, f.leadTok, _, err = authz.IssueToken(ctx, pool, f.lead, "t", time.Hour)
	must(err)
	f.jobA, _, err = queue.Enqueue(ctx, pool, queue.Job{Kind: "test.mcp", OperatorID: f.opA})
	must(err)
	f.jobB, _, err = queue.Enqueue(ctx, pool, queue.Job{Kind: "test.mcp", OperatorID: f.opB})
	must(err)

	mux := http.NewServeMux()
	New(pool, "test", []Topic{{"fare", "Giá vé"}, {"pets", "Thú cưng"}, {"luggage", "Hành lý"}, {"children", "Trẻ em"}}).Mount(mux)
	srv := httptest.NewServer(mux)
	f.url = srv.URL

	t.Cleanup(func() {
		srv.Close()
		pool.Exec(ctx, `DELETE FROM audit_log WHERE actor IN ($1, $2)`, "user:"+f.builder, "user:"+f.lead)
		pool.Exec(ctx, `DELETE FROM users WHERE id IN ($1, $2)`, f.builder, f.lead)
		pool.Exec(ctx, `DELETE FROM operators WHERE id IN ($1, $2)`, f.opA, f.opB) // cascade operations
	})
	return f
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, endpoint, token string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
		MaxRetries: -1,
	}, nil)
}

// rawStatus gửi initialize thô để đọc mã HTTP (401/403/404).
func rawStatus(t *testing.T, endpoint, token string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func callGetOperation(t *testing.T, s *mcp.ClientSession, id string) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_operation", Arguments: map[string]any{"operation_id": id},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return res, out
}

func TestAuthentication(t *testing.T) {
	f := setup(t)
	ep := f.url + "/mcp/operator/" + f.opA + "/"
	if c := rawStatus(t, ep, ""); c != http.StatusUnauthorized {
		t.Errorf("không token: %d, muốn 401", c)
	}
	if c := rawStatus(t, ep, "biva_sai"); c != http.StatusUnauthorized {
		t.Errorf("token sai: %d, muốn 401", c)
	}
	if c := rawStatus(t, ep, f.builderTok); c != http.StatusOK {
		t.Errorf("token đúng: %d, muốn 200", c)
	}
	// Thu hồi → bị từ chối ngay ở request kế tiếp.
	if err := authz.RevokeToken(context.Background(), f.pool, f.builderTokID); err != nil {
		t.Fatal(err)
	}
	if c := rawStatus(t, ep, f.builderTok); c != http.StatusUnauthorized {
		t.Errorf("token đã thu hồi: %d, muốn 401", c)
	}
	// Hết hạn.
	f.pool.Exec(context.Background(), `UPDATE api_tokens SET expires_at = now() - interval '1 second' WHERE user_id = $1`, f.lead)
	if c := rawStatus(t, f.url+"/mcp/platform/", f.leadTok); c != http.StatusUnauthorized {
		t.Errorf("token hết hạn: %d, muốn 401", c)
	}
}

// AC S0.3.3: token sai phạm vi → bị từ chối; audit ghi actor.
func TestScopeDeniedAndAudited(t *testing.T) {
	f := setup(t)
	if c := rawStatus(t, f.url+"/mcp/operator/"+f.opB+"/", f.builderTok); c != http.StatusForbidden {
		t.Errorf("builder vào nhà xe không được gán: %d, muốn 403", c)
	}
	if c := rawStatus(t, f.url+"/mcp/platform/", f.builderTok); c != http.StatusForbidden {
		t.Errorf("builder vào platform: %d, muốn 403", c)
	}
	var n int
	f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_log
		WHERE actor = $1 AND action = 'mcp.denied' AND target IN ($2, 'platform')`,
		"user:"+f.builder, "operator:"+f.opB).Scan(&n)
	if n != 2 {
		t.Errorf("audit mcp.denied = %d dòng, muốn 2", n)
	}
	if c := rawStatus(t, f.url+"/mcp/operator/khong-ton-tai/", f.leadTok); c != http.StatusNotFound {
		t.Errorf("lead vào nhà xe không tồn tại: %d, muốn 404", c)
	}
}

// AC S0.3.2: client MCP kết nối, liệt kê và gọi được get_operation.
func TestGetOperationScopedToOperator(t *testing.T) {
	f := setup(t)
	s, err := connect(t, f.url+"/mcp/operator/"+f.opA+"/", f.builderTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyWant := map[string]bool{"get_operation": true, "list_review_queue": true, "get_review_item": true,
		"list_knowledge": true, "ingest": false, "submit_knowledge": false, "propose_item": false, "apply_review": false}
	if len(tools.Tools) != len(readOnlyWant) {
		t.Fatalf("có %d tool, muốn %d", len(tools.Tools), len(readOnlyWant))
	}
	for _, tool := range tools.Tools {
		if (tool.Name == "submit_knowledge" || tool.Name == "propose_item") && !strings.Contains(tool.Description, "children (Trẻ em)") {
			t.Errorf("mô tả %s phải liệt kê topic kèm tên", tool.Name)
		}
		ro, ok := readOnlyWant[tool.Name]
		if !ok || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != ro {
			t.Errorf("tool %s: readOnlyHint sai hoặc tool lạ", tool.Name)
		}
	}

	res, out := callGetOperation(t, s, f.jobA)
	if res.IsError || out["status"] != "queued" || out["id"] != f.jobA {
		t.Fatalf("job của nhà xe mình: isError=%v out=%v", res.IsError, out)
	}
	// Job đã xong (có result dạng object) — output phải khớp output schema của tool.
	f.pool.Exec(context.Background(), `UPDATE operations SET status = 'done', result = '{"status":"done","summary":"pong"}' WHERE id = $1`, f.jobA)
	res, out = callGetOperation(t, s, f.jobA)
	if res.IsError || out["status"] != "done" || out["result"].(map[string]any)["summary"] != "pong" {
		t.Fatalf("job đã xong: isError=%v content=%v out=%v", res.IsError, res.Content, out)
	}
	// Job của nhà xe khác: như không tồn tại.
	if res, _ := callGetOperation(t, s, f.jobB); !res.IsError {
		t.Fatal("job của nhà xe khác lẽ ra phải bị ẩn")
	}
	if res, _ := callGetOperation(t, s, "khong-phai-uuid"); !res.IsError {
		t.Fatal("id không phải UUID lẽ ra phải lỗi")
	}
}

func TestPlatformForLead(t *testing.T) {
	f := setup(t)
	s, err := connect(t, f.url+"/mcp/platform/", f.leadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if res, out := callGetOperation(t, s, f.jobB); res.IsError || out["id"] != f.jobB {
		t.Fatalf("lead đọc job bất kỳ qua platform: %v %v", res.IsError, out)
	}
}
