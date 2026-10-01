package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/trieungoctam/biva-brain/brain-api/internal/authz"
	"github.com/trieungoctam/biva-brain/brain-api/internal/oauth"
)

// oauthServer: brain-api có cả MCP + authorization server, issuer = URL của httptest server.
func oauthServer(t *testing.T, f *fixture) string {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	o := oauth.New(f.pool, srv.URL)
	o.Mount(mux)
	New(f.pool, "test", []Topic{{"fare", "Giá vé", true}}).WithOAuth(o).Mount(mux)
	t.Cleanup(func() {
		ctx := context.Background()
		f.pool.Exec(ctx, `DELETE FROM oauth_clients WHERE client_id IN (
			SELECT client_id FROM oauth_tokens WHERE user_id IN ($1, $2)
			UNION SELECT client_id FROM oauth_codes WHERE user_id IN ($1, $2))`, f.builder, f.lead)
	})
	return srv.URL
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// userApproves giả lập builder: mở trang authorize, dán token cá nhân, bấm "Cho phép" → lấy code từ redirect.
func userApproves(personalToken string) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, err
		}
		if resp, err := http.Get(args.URL); err != nil || resp.StatusCode != 200 {
			return nil, errors.New("trang authorize không mở được")
		}
		form := u.Query()
		form.Set("personal_token", personalToken)
		resp, err := noRedirect.PostForm(u.Scheme+"://"+u.Host+u.Path, form)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			return nil, errors.New("không được cấp quyền: HTTP " + resp.Status)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
}

func oauthClient(t *testing.T, endpoint, personalToken string) (*mcp.ClientSession, error) {
	t.Helper()
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "Test connector",
				RedirectURIs: []string{"https://example.test/oauth/callback"},
				GrantTypes:   []string{"authorization_code", "refresh_token"},
			},
		},
		RedirectURL:              "https://example.test/oauth/callback",
		AuthorizationCodeFetcher: userApproves(personalToken),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "chatgpt-like", Version: "0"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, OAuthHandler: h, MaxRetries: -1,
	}, nil)
}

// AC DYN-115: client OAuth chuẩn (SDK MCP) đi trọn luồng discovery → register → authorize → token → gọi tool.
func TestOAuthFlowWithStandardClient(t *testing.T) {
	f := setup(t)
	base := oauthServer(t, f)
	endpoint := base + "/mcp/operator/" + f.opA + "/"

	// 401 kèm WWW-Authenticate trỏ tới metadata đúng endpoint.
	resp, _ := http.Post(endpoint, "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"),
		`resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp/operator/`+f.opA+`/"`) {
		t.Fatalf("401 = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	s, err := oauthClient(t, endpoint, f.builderTok)
	if err != nil {
		t.Fatalf("luồng OAuth thất bại: %v", err)
	}
	defer s.Close()
	if isErr, _, text := call(t, s, "list_knowledge", map[string]any{}); isErr {
		t.Fatalf("gọi tool bằng token OAuth: %s", text)
	}

	// Token OAuth mang đúng user; audit có lần cấp quyền.
	var n int
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_tokens WHERE user_id = $1 AND kind = 'access'`, f.builder).Scan(&n)
	if n != 1 {
		t.Fatalf("access token của builder = %d", n)
	}
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE actor = $1 AND action = 'oauth.authorize'`, "user:"+f.builder).Scan(&n)
	if n != 1 {
		t.Fatalf("audit oauth.authorize = %d", n)
	}

	// Thu hồi token cá nhân → token OAuth sinh ra từ nó mất hiệu lực ngay.
	if err := authz.RevokeToken(context.Background(), f.pool, f.builderTokID); err != nil {
		t.Fatal(err)
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_knowledge", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatal("token OAuth phải mất hiệu lực khi token cá nhân bị thu hồi")
	}
}

func TestOAuthAuthorizeRejectsOperatorOutsideScope(t *testing.T) {
	f := setup(t)
	base := oauthServer(t, f)
	// builder chỉ có nhà xe A → xin quyền vào B bị từ chối ở trang authorize.
	if _, err := oauthClient(t, base+"/mcp/operator/"+f.opB+"/", f.builderTok); err == nil {
		t.Fatal("builder không được cấp quyền vào nhà xe không được gán")
	}
}

// Kiểm từng bước bằng HTTP thô: PKCE, dùng lại code, refresh xoay vòng + phát hiện dùng lại, sai resource.
func TestOAuthTokenEndpointSecurity(t *testing.T) {
	f := setup(t)
	base := oauthServer(t, f)
	resA := base + "/mcp/operator/" + f.opA + "/"

	var reg struct {
		ClientID string `json:"client_id"`
	}
	body := `{"client_name":"raw","redirect_uris":["https://example.test/cb"]}`
	r, _ := http.Post(base+"/oauth/register", "application/json", strings.NewReader(body))
	json.NewDecoder(r.Body).Decode(&reg)
	if r.StatusCode != 201 || reg.ClientID == "" {
		t.Fatalf("register = %d", r.StatusCode)
	}
	if r, _ := http.Post(base+"/oauth/register", "application/json", strings.NewReader(`{"redirect_uris":["http://evil.test/cb"]}`)); r.StatusCode != 400 {
		t.Fatal("redirect_uri http (không phải localhost) phải bị từ chối")
	}

	verifier := "v-" + strings.Repeat("x", 60)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	getCode := func() string {
		form := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {"https://example.test/cb"},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"s1"}, "resource": {resA},
			"personal_token": {f.builderTok}}
		resp, err := noRedirect.PostForm(base+"/oauth/authorize", form)
		if err != nil || resp.StatusCode != 302 {
			t.Fatalf("authorize: %v %v", err, resp.Status)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if loc.Query().Get("state") != "s1" || loc.Query().Get("iss") != base {
			t.Fatalf("redirect = %s", loc)
		}
		return loc.Query().Get("code")
	}
	tokenReq := func(v url.Values) (int, map[string]any) {
		v.Set("client_id", reg.ClientID)
		resp, _ := http.PostForm(base+"/oauth/token", v)
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// PKCE sai → từ chối (và code đã bị tiêu, không thử lại được).
	code := getCode()
	if st, out := tokenReq(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://example.test/cb"},
		"code_verifier": {"sai"}}); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("PKCE sai: %d %v", st, out)
	}
	code = getCode()
	st, tok := tokenReq(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://example.test/cb"},
		"code_verifier": {verifier}})
	if st != 200 || !strings.HasPrefix(tok["access_token"].(string), "boa_") || tok["token_type"] != "Bearer" {
		t.Fatalf("đổi code: %d %v", st, tok)
	}
	// Dùng lại code → từ chối.
	if st, _ := tokenReq(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://example.test/cb"},
		"code_verifier": {verifier}}); st != 400 {
		t.Fatal("code phải dùng một lần")
	}

	// Access token cấp cho nhà xe A không dùng được ở endpoint nhà xe B (dù lead/role cho phép).
	callWith := func(endpoint, access string) int {
		req, _ := http.NewRequest("POST", endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
		req.Header.Set("Authorization", "Bearer "+access)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, _ := http.DefaultClient.Do(req)
		return resp.StatusCode
	}
	access := tok["access_token"].(string)
	if c := callWith(resA, access); c != 200 {
		t.Fatalf("token đúng resource: %d", c)
	}
	if c := callWith(base+"/mcp/platform/", access); c != 401 {
		t.Fatalf("token của resource nhà xe A dùng ở platform: %d, muốn 401", c)
	}

	// Refresh xoay vòng; dùng lại refresh cũ → thu hồi cả family (access mới cũng chết).
	refresh := tok["refresh_token"].(string)
	st, tok2 := tokenReq(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if st != 200 || tok2["refresh_token"] == refresh {
		t.Fatalf("refresh: %d %v", st, tok2)
	}
	if st, _ := tokenReq(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); st != 400 {
		t.Fatal("refresh token cũ phải bị từ chối")
	}
	if c := callWith(resA, tok2["access_token"].(string)); c != 401 {
		t.Fatalf("sau khi phát hiện refresh bị dùng lại, cả family phải bị thu hồi: %d", c)
	}
}

func TestOAuthMetadata(t *testing.T) {
	f := setup(t)
	base := oauthServer(t, f)
	var as oauthex.AuthServerMeta
	r, _ := http.Get(base + "/.well-known/oauth-authorization-server")
	json.NewDecoder(r.Body).Decode(&as)
	if as.Issuer != base || as.RegistrationEndpoint != base+"/oauth/register" || as.CodeChallengeMethodsSupported[0] != "S256" {
		t.Fatalf("AS metadata = %+v", as)
	}
	var prm oauthex.ProtectedResourceMetadata
	r, _ = http.Get(base + "/.well-known/oauth-protected-resource/mcp/operator/" + f.opA + "/")
	json.NewDecoder(r.Body).Decode(&prm)
	if prm.Resource != base+"/mcp/operator/"+f.opA+"/" || prm.AuthorizationServers[0] != base {
		t.Fatalf("PRM = %+v", prm)
	}
}
