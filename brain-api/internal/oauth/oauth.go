// Package oauth: brain-api làm authorization server OAuth 2.1 cho MCP — để client như ChatGPT (connector) kết nối.
//
//	GET  /.well-known/oauth-protected-resource[/<đường dẫn MCP>]   RFC 9728 (resource = đúng URL MCP)
//	GET  /.well-known/oauth-authorization-server                  RFC 8414
//	POST /oauth/register                                          RFC 7591 (đăng ký client động)
//	GET  /oauth/authorize, POST /oauth/authorize                  authorization code + PKCE S256 (RFC 7636)
//	POST /oauth/token                                             authorization_code | refresh_token (xoay vòng)
//
// Builder đăng nhập ở trang authorize bằng token cá nhân (brain-api token issue); token OAuth gắn với token cá nhân
// đó nên thu hồi nó là cắt luôn quyền của ChatGPT. Access/refresh token opaque, DB chỉ lưu SHA-256.
// Coding agent vẫn dùng thẳng token cá nhân (Authorization: Bearer biva_...).
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/trieungoctam/biva-brain/brain-api/internal/audit"
	"github.com/trieungoctam/biva-brain/brain-api/internal/authz"
)

const (
	AccessPrefix  = "boa_" // access token OAuth (phân biệt với token cá nhân biva_)
	refreshPrefix = "bor_"
	codePrefix    = "boc_"
	Scope         = "mcp"
)

var ErrInvalid = errors.New("token OAuth không hợp lệ, hết hạn hoặc đã thu hồi")

type Server struct {
	DB         *pgxpool.Pool
	Issuer     string // URL công khai của brain-api, không có "/" cuối (vd https://brain.biva.vn)
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	CodeTTL    time.Duration
}

func New(db *pgxpool.Pool, issuer string) *Server {
	return &Server{DB: db, Issuer: strings.TrimRight(issuer, "/"), AccessTTL: time.Hour,
		RefreshTTL: 30 * 24 * time.Hour, CodeTTL: 5 * time.Minute}
}

func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/{path...}", s.resourceMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorizeForm)
	mux.HandleFunc("POST /oauth/authorize", s.authorizeSubmit)
	mux.HandleFunc("POST /oauth/token", s.token)
}

// ResourceMetadataURL: URL metadata cho một đường dẫn MCP (đưa vào WWW-Authenticate khi trả 401).
func (s *Server) ResourceMetadataURL(path string) string {
	return s.Issuer + "/.well-known/oauth-protected-resource/" + strings.TrimLeft(path, "/")
}

// ─────────────────────────────── metadata ───────────────────────────────

func (s *Server) authServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, oauthex.AuthServerMeta{
		Issuer:                                     s.Issuer,
		AuthorizationEndpoint:                      s.Issuer + "/oauth/authorize",
		TokenEndpoint:                              s.Issuer + "/oauth/token",
		RegistrationEndpoint:                       s.Issuer + "/oauth/register",
		ScopesSupported:                            []string{Scope},
		ResponseTypesSupported:                     []string{"code"},
		GrantTypesSupported:                        []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:          []string{"none", "client_secret_post", "client_secret_basic"},
		CodeChallengeMethodsSupported:              []string{"S256"},
		AuthorizationResponseIssParameterSupported: true,
	})
}

func (s *Server) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	resource := s.Issuer
	if p := r.PathValue("path"); p != "" {
		resource = s.Issuer + "/" + p
		if strings.HasSuffix(r.URL.Path, "/") && !strings.HasSuffix(resource, "/") {
			resource += "/"
		}
	}
	writeJSON(w, http.StatusOK, oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{s.Issuer},
		ScopesSupported:        []string{Scope},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "BIVA Brain",
	})
}

// ─────────────────────────────── đăng ký client (RFC 7591) ───────────────────────────────

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" {
		return false
	}
	host := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1"))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	// BIVA_OAUTH_REGISTRATION_SECRET (tuỳ chọn, RFC 7591 §5 initial access token): đặt thì
	// register yêu cầu Bearer đúng giá trị — production đóng cửa đăng ký tự do.
	if sec := os.Getenv("BIVA_OAUTH_REGISTRATION_SECRET"); sec != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(sec)) != 1 {
			oauthError(w, http.StatusUnauthorized, "invalid_token", "cần initial access token để đăng ký client")
			return
		}
	}
	var m oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&m); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body phải là JSON metadata của client")
		return
	}
	if len(m.RedirectURIs) == 0 || len(m.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "cần 1–10 redirect_uris")
		return
	}
	for _, u := range m.RedirectURIs {
		if !validRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri phải là https (hoặc http://localhost): "+u)
			return
		}
	}
	method := m.TokenEndpointAuthMethod
	if method == "" {
		method = "none"
	}
	if !slices.Contains([]string{"none", "client_secret_post", "client_secret_basic"}, method) {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method không hỗ trợ: "+method)
		return
	}
	clientID := "boc-client-" + randomToken(16)
	var secret string
	var secretHash []byte
	if method != "none" {
		secret = randomToken(32)
		secretHash = sha(secret)
	}
	if _, err := s.DB.Exec(r.Context(), `INSERT INTO oauth_clients (client_id, client_name, redirect_uris, auth_method, client_secret_hash)
		VALUES ($1, $2, $3, $4, $5)`, clientID, truncate(m.ClientName, 200), m.RedirectURIs, method, secretHash); err != nil {
		internalError(w, "register", err)
		return
	}
	// Tự dựng response: RFC 7591 §3.2.1 yêu cầu client_id_issued_at / client_secret_expires_at là SỐ giây
	// (kiểu oauthex.ClientRegistrationResponse serialize thành chuỗi thời gian).
	resp := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                m.ClientName,
		"redirect_uris":              m.RedirectURIs,
		"token_endpoint_auth_method": method,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0 // không hết hạn
	}
	writeJSON(w, http.StatusCreated, resp)
}

// ─────────────────────────────── authorize ───────────────────────────────

type authRequest struct {
	ClientID, ClientName, RedirectURI, State, Challenge, Method, Scope, Resource string
}

// parseAuthRequest kiểm tham số. Lỗi về client/redirect_uri KHÔNG được redirect (RFC 6749 §4.1.2.1) → trang lỗi.
func (s *Server) parseAuthRequest(ctx context.Context, v url.Values) (authRequest, string) {
	a := authRequest{ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state"),
		Challenge: v.Get("code_challenge"), Method: v.Get("code_challenge_method"), Scope: v.Get("scope"),
		Resource: v.Get("resource")}
	var uris []string
	err := s.DB.QueryRow(ctx, `SELECT client_name, redirect_uris FROM oauth_clients WHERE client_id = $1`, a.ClientID).
		Scan(&a.ClientName, &uris)
	if err != nil {
		return a, "client_id không hợp lệ"
	}
	if !slices.Contains(uris, a.RedirectURI) {
		return a, "redirect_uri không khớp client đã đăng ký"
	}
	switch {
	case v.Get("response_type") != "code":
		return a, "response_type phải là code"
	case a.Method != "S256" || len(a.Challenge) < 43:
		return a, "cần PKCE: code_challenge_method=S256 và code_challenge"
	case a.Resource == "":
		return a, "cần tham số resource (RFC 8707): URL endpoint MCP muốn truy cập"
	case !s.validResource(a.Resource):
		return a, "resource không hợp lệ (phải là máy chủ này, /mcp/platform/ hoặc /mcp/operator/<nhà xe>/)"
	}
	return a, ""
}

// validResource (RFC 8707): chỉ nhận đúng issuer, endpoint platform, hoặc endpoint của MỘT nhà xe
// (id không rỗng — resource "/mcp/operator/" rỗng từng match prefix mọi endpoint, giờ từ chối).
func (s *Server) validResource(resource string) bool {
	if resource == s.Issuer {
		return true
	}
	if rest, ok := strings.CutPrefix(resource, s.Issuer+"/"); ok {
		if rest == "mcp/platform/" {
			return true
		}
		if op, ok2 := strings.CutPrefix(rest, "mcp/operator/"); ok2 {
			op = strings.TrimSuffix(op, "/")
			// một segment, không chứa ".." / "/" (chặn /mcp/operator/../platform/)
			return op != "" && !strings.ContainsAny(op, "/.") && strings.HasSuffix(rest, "/")
		}
	}
	return false
}

var page = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="vi"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>BIVA Brain — cấp quyền</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem;color:#1a1a1a}
input[type=password]{width:100%;padding:.6rem;font:inherit;box-sizing:border-box}button{padding:.6rem 1.2rem;font:inherit}
.err{color:#b00020}.muted{color:#555;font-size:.9rem}</style></head><body>
<h1>Kết nối BIVA Brain</h1>
{{if .Fatal}}<p class="err">{{.Fatal}}</p>{{else}}
<p><b>{{.Req.ClientName}}</b> <span class="muted">({{.Req.ClientID}})</span> muốn truy cập tri thức BIVA Brain{{if .Operator}} của nhà xe <b>{{.Operator}}</b>{{end}} thay mặt bạn.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/oauth/authorize">
<label for="t">Token cá nhân (biva_…, cấp bởi quản trị bằng <code>brain-api token issue</code>)</label>
<p><input id="t" name="personal_token" type="password" autocomplete="off" required></p>
{{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<button type="submit">Cho phép</button>
</form>
<p class="muted">Quyền của ứng dụng giống quyền của bạn và mất hiệu lực khi token cá nhân bị thu hồi.</p>{{end}}
</body></html>`))

type pageData struct {
	Req      authRequest
	Operator string
	Error    string
	Fatal    string
	Hidden   map[string]string
}

func operatorOf(resource, issuer string) string {
	rest, ok := strings.CutPrefix(resource, issuer+"/mcp/operator/")
	if !ok {
		return ""
	}
	return strings.Trim(rest, "/")
}

func (s *Server) render(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY") // chống clickjacking trang cấp quyền
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.WriteHeader(status)
	_ = page.Execute(w, d)
}

func hidden(v url.Values) map[string]string {
	h := map[string]string{}
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "state", "code_challenge",
		"code_challenge_method", "scope", "resource"} {
		if x := v.Get(k); x != "" {
			h[k] = x
		}
	}
	return h
}

func (s *Server) authorizeForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a, problem := s.parseAuthRequest(r.Context(), q)
	if problem != "" {
		s.render(w, http.StatusBadRequest, pageData{Fatal: problem})
		return
	}
	s.render(w, http.StatusOK, pageData{Req: a, Operator: operatorOf(a.Resource, s.Issuer), Hidden: hidden(q)})
}

func (s *Server) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, http.StatusBadRequest, pageData{Fatal: "form không hợp lệ"})
		return
	}
	a, problem := s.parseAuthRequest(r.Context(), r.PostForm)
	if problem != "" {
		s.render(w, http.StatusBadRequest, pageData{Fatal: problem})
		return
	}
	retry := func(msg string) {
		s.render(w, http.StatusUnauthorized, pageData{Req: a, Operator: operatorOf(a.Resource, s.Issuer),
			Error: msg, Hidden: hidden(r.PostForm)})
	}
	p, err := authz.Verify(r.Context(), s.DB, strings.TrimSpace(r.PostForm.Get("personal_token")))
	if errors.Is(err, authz.ErrInvalidToken) {
		retry("Token không hợp lệ, hết hạn hoặc đã bị thu hồi.")
		return
	}
	if err != nil {
		internalError(w, "authorize", err)
		return
	}
	if op := operatorOf(a.Resource, s.Issuer); op != "" && !p.CanAccessOperator(op) {
		retry("Tài khoản của bạn không có quyền với nhà xe " + op + ".")
		return
	}

	code := codePrefix + randomToken(32)
	if _, err := s.DB.Exec(r.Context(), `INSERT INTO oauth_codes (code_hash, client_id, user_id, login_token_id,
		redirect_uri, code_challenge, resource, scope, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(secs => $9))`,
		sha(code), a.ClientID, p.UserID, p.TokenID, a.RedirectURI, a.Challenge, a.Resource, a.Scope,
		s.CodeTTL.Seconds()); err != nil {
		internalError(w, "authorize", err)
		return
	}
	_ = audit.Write(r.Context(), s.DB, audit.Entry{Actor: p.Actor(), Action: "oauth.authorize",
		Target: "oauth_client:" + a.ClientID, Payload: map[string]any{"client_name": a.ClientName, "resource": a.Resource}})

	u, _ := url.Parse(a.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	q.Set("iss", s.Issuer) // RFC 9207
	if a.State != "" {
		q.Set("state", a.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// ─────────────────────────────── token ───────────────────────────────

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

// clientAuth xác thực client ở token endpoint theo phương thức đã đăng ký.
func (s *Server) clientAuth(ctx context.Context, r *http.Request) (string, bool) {
	clientID, secret, basic := r.BasicAuth()
	if !basic {
		clientID, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	var method string
	var hash []byte
	if err := s.DB.QueryRow(ctx, `SELECT auth_method, client_secret_hash FROM oauth_clients WHERE client_id = $1`,
		clientID).Scan(&method, &hash); err != nil {
		return "", false
	}
	if method == "none" {
		return clientID, true
	}
	return clientID, secret != "" && subtle.ConstantTimeCompare(sha(secret), hash) == 1
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "form không hợp lệ")
		return
	}
	clientID, ok := s.clientAuth(r.Context(), r)
	if !ok {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client không hợp lệ")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, clientID)
	case "refresh_token":
		s.refresh(w, r, clientID)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "chỉ hỗ trợ authorization_code, refresh_token")
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	f := r.PostForm
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		internalError(w, "token", err)
		return
	}
	defer tx.Rollback(ctx)
	var userID, loginToken, redirect, challenge, resource, scope string
	err = tx.QueryRow(ctx, `UPDATE oauth_codes SET used_at = now()
		WHERE code_hash = $1 AND client_id = $2 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id, login_token_id::text, redirect_uri, code_challenge, resource, scope`,
		sha(f.Get("code")), clientID).Scan(&userID, &loginToken, &redirect, &challenge, &resource, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code không hợp lệ, hết hạn hoặc đã dùng")
		return
	}
	if err != nil {
		internalError(w, "token", err)
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	switch {
	case f.Get("redirect_uri") != redirect:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri không khớp")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != challenge:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier không khớp (PKCE)")
		return
	case f.Get("resource") != "" && f.Get("resource") != resource:
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource không khớp lúc authorize")
		return
	}
	resp, err := s.issue(ctx, tx, uuid.New(), clientID, userID, loginToken, resource, scope)
	if err != nil {
		internalError(w, "token", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, "token", err)
		return
	}
	writeTokens(w, resp)
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, clientID string) {
	ctx := r.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		internalError(w, "token", err)
		return
	}
	defer tx.Rollback(ctx)
	var family uuid.UUID
	var userID, loginToken, resource, scope string
	var used *time.Time
	err = tx.QueryRow(ctx, `SELECT t.family, t.user_id, t.login_token_id::text, t.resource, t.scope, t.used_at
		FROM oauth_tokens t JOIN api_tokens a ON a.id = t.login_token_id JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND t.kind = 'refresh' AND t.client_id = $2 AND t.revoked_at IS NULL
		  AND t.expires_at > now() AND a.revoked_at IS NULL AND a.expires_at > now() AND u.status = 'active'
		FOR UPDATE OF t`, sha(r.PostForm.Get("refresh_token")), clientID).
		Scan(&family, &userID, &loginToken, &resource, &scope, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token không hợp lệ")
		return
	}
	if err != nil {
		internalError(w, "token", err)
		return
	}
	if used != nil {
		// Refresh token bị dùng lại → có thể đã lộ: thu hồi cả family (OAuth 2.1 §4.3.1).
		// Ghi trong cùng transaction (đang giữ khoá dòng) rồi commit — ghi bằng kết nối khác sẽ tự chờ khoá này.
		if _, err := tx.Exec(ctx, `UPDATE oauth_tokens SET revoked_at = now() WHERE family = $1 AND revoked_at IS NULL`, family); err != nil {
			internalError(w, "token", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			internalError(w, "token", err)
			return
		}
		slog.Warn("oauth: refresh token bị dùng lại, đã thu hồi cả family", "user", userID, "client", clientID)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token đã được dùng")
		return
	}
	if _, err := tx.Exec(ctx, `UPDATE oauth_tokens SET used_at = now() WHERE token_hash = $1`, sha(r.PostForm.Get("refresh_token"))); err != nil {
		internalError(w, "token", err)
		return
	}
	resp, err := s.issue(ctx, tx, family, clientID, userID, loginToken, resource, scope)
	if err != nil {
		internalError(w, "token", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, "token", err)
		return
	}
	writeTokens(w, resp)
}

func (s *Server) issue(ctx context.Context, tx pgx.Tx, family uuid.UUID, clientID, userID, loginToken, resource, scope string) (tokenResponse, error) {
	access := AccessPrefix + randomToken(32)
	refresh := refreshPrefix + randomToken(32)
	for _, t := range []struct {
		token, kind string
		ttl         time.Duration
	}{{access, "access", s.AccessTTL}, {refresh, "refresh", s.RefreshTTL}} {
		if _, err := tx.Exec(ctx, `INSERT INTO oauth_tokens (token_hash, kind, family, client_id, user_id, login_token_id,
			resource, scope, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now() + make_interval(secs => $9))`,
			sha(t.token), t.kind, family, clientID, userID, loginToken, resource, scope, t.ttl.Seconds()); err != nil {
			return tokenResponse{}, err
		}
	}
	if scope == "" {
		scope = Scope
	}
	return tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(s.AccessTTL.Seconds()),
		RefreshToken: refresh, Scope: scope}, nil
}

// ─────────────────────────────── kiểm access token ───────────────────────────────

// Verify kiểm access token OAuth cho một request tới requestURL (URL công khai đầy đủ, vd .../mcp/operator/x/).
// Token phải được cấp cho đúng resource đó (hoặc cho cả máy chủ) — chống dùng token của resource khác.
func (s *Server) Verify(ctx context.Context, token, requestURL string) (authz.Principal, error) {
	var userID, loginToken, resource string
	var exp time.Time
	err := s.DB.QueryRow(ctx, `SELECT t.user_id, t.login_token_id::text, t.resource, t.expires_at
		FROM oauth_tokens t JOIN api_tokens a ON a.id = t.login_token_id
		WHERE t.token_hash = $1 AND t.kind = 'access' AND t.revoked_at IS NULL AND t.expires_at > now()
		  AND a.revoked_at IS NULL AND a.expires_at > now()`, sha(token)).Scan(&userID, &loginToken, &resource, &exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return authz.Principal{}, ErrInvalid
	}
	if err != nil {
		return authz.Principal{}, err
	}
	if resource != "" && resource != s.Issuer && !strings.HasPrefix(requestURL, strings.TrimRight(resource, "/")+"/") &&
		requestURL != resource {
		return authz.Principal{}, ErrInvalid
	}
	p, err := authz.LoadPrincipal(ctx, s.DB, userID, "oauth:"+loginToken, exp)
	if errors.Is(err, authz.ErrInvalidToken) {
		return p, ErrInvalid
	}
	return p, err
}

// ─────────────────────────────── tiện ích ───────────────────────────────

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand không lỗi trên các nền tảng hỗ trợ
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func sha(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeTokens(w http.ResponseWriter, t tokenResponse) {
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, t)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func internalError(w http.ResponseWriter, where string, err error) {
	slog.Error("oauth lỗi", "endpoint", where, "err", err)
	oauthError(w, http.StatusInternalServerError, "server_error", "lỗi tạm thời, thử lại sau")
}
