// Package mcpserver mở tri thức Brain cho AI của builder qua MCP (streamable HTTP).
//
//	/mcp/operator/{operator_id}/  một nhà xe — tool không nhận operator_id (tránh ghi nhầm nhà xe)
//	/mcp/platform/                L0/L1 toàn cục — chỉ role lead
//
// Mỗi request: xác thực Bearer token (authz.Verify) → kiểm phạm vi theo URL → tool.
// Chế độ stateless: không giữ session trong RAM nên chạy nhiều instance brain-api sau load balancer được.
// Thiết kế tool: docs/mcp.md.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/audit"
	"github.com/trieungoctam/biva-brain/brain-api/internal/authz"
	"github.com/trieungoctam/biva-brain/brain-api/internal/entity"
	"github.com/trieungoctam/biva-brain/brain-api/internal/kb"
	"github.com/trieungoctam/biva-brain/brain-api/internal/oauth"
	"github.com/trieungoctam/biva-brain/brain-api/internal/pack"
	"github.com/trieungoctam/biva-brain/brain-api/internal/recall"
	"github.com/trieungoctam/biva-brain/brain-api/internal/storage"
)

const principalKey = "principal"

type Server struct {
	db        *pgxpool.Pool
	version   string
	oauth     *oauth.Server // nil = chỉ nhận token cá nhân
	topics    []Topic       // bộ topic của template L1 (kb/): kiểm đầu vào và hướng dẫn AI chọn topic
	recaller  *recall.Recaller
	packs     *pack.Builder
	publicURL string      // URL công khai (link form gửi nhà xe)
	template  kb.Template // template ngành (kb/L1/<ngành>/template.yaml): get_bot_spec, artifact bắt buộc
	storage   *storage.S3 // nil = export_bot trả nội dung, không lưu object storage (DYN-74)

	mu        sync.Mutex
	operators map[string]*mcp.Server // MCP server theo nhà xe, dựng một lần
	platform  *mcp.Server
}

// Topic của template ngành (id + tên hiển thị), lấy từ kb/L1/<ngành>/template.yaml.
type Topic struct {
	ID       string
	Title    string
	Required bool // mục bắt buộc của template (coverage)
}

func (s *Server) topicIDs() []string {
	ids := make([]string, len(s.topics))
	for i, t := range s.topics {
		ids[i] = t.ID
	}
	return ids
}

// topicGuide: "route (Tuyến & điểm dừng), fare (Giá vé), …" — đưa vào mô tả tool để AI chọn đúng topic.
func (s *Server) topicGuide() string {
	parts := make([]string, len(s.topics))
	for i, t := range s.topics {
		parts[i] = t.ID + " (" + t.Title + ")"
	}
	return strings.Join(parts, ", ") + ", other (không khớp mục nào)"
}

func New(db *pgxpool.Pool, version string, topics []Topic) *Server {
	s := &Server{db: db, version: version, topics: topics, operators: map[string]*mcp.Server{},
		recaller: &recall.Recaller{DB: db}}
	specs := make([]pack.TopicSpec, len(topics))
	for i, t := range topics {
		specs[i] = pack.TopicSpec{ID: t.ID, Title: t.Title, Required: t.Required}
	}
	s.packs = &pack.Builder{DB: db, Topics: specs}
	s.platform = s.newPlatformServer()
	return s
}

// WithOAuth bật token OAuth (ChatGPT connector) bên cạnh token cá nhân.
func (s *Server) WithOAuth(o *oauth.Server) *Server {
	s.oauth = o
	return s
}

// WithPublicURL: URL công khai của brain-api (link form cho nhà xe).
func (s *Server) WithPublicURL(u string) *Server {
	s.publicURL = u
	return s
}

// WithTemplate: template ngành cho get_bot_spec / artifact bắt buộc.
func (s *Server) WithTemplate(t kb.Template) *Server {
	s.template = t
	return s
}

// WithPacks: dùng pack builder có sẵn (chia sẻ cache với job refresh_pages của scheduler).
func (s *Server) WithPacks(b *pack.Builder) *Server {
	if b != nil {
		s.packs = b
	}
	return s
}

// WithEntities: từ điển thực thể + alias (kb/L1/<ngành>/entities.yaml) cho recall và query_data.
func (s *Server) WithEntities(r *entity.Resolver) *Server {
	s.recaller.Entities = r
	return s
}

// WithReranker bật rerank của recall_knowledge (TEI bge-reranker-v2-m3, S3.1.3).
func (s *Server) WithReranker(r recall.Reranker) *Server {
	s.recaller.Reranker = r
	return s
}

// WithEmbedder bật nhánh semantic của recall_knowledge (TEI, cùng model với job index.items).
func (s *Server) WithEmbedder(e recall.Embedder) *Server {
	s.recaller.Embedder = e
	return s
}

// bearer: xác thực Bearer; 401 kèm WWW-Authenticate trỏ tới protected resource metadata của đúng endpoint
// (RFC 9728) để client OAuth tự tìm authorization server.
func (s *Server) bearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var opts *auth.RequireBearerTokenOptions
		if s.oauth != nil {
			opts = &auth.RequireBearerTokenOptions{ResourceMetadataURL: s.oauth.ResourceMetadataURL(r.URL.Path),
				Scopes: nil}
		}
		auth.RequireBearerToken(s.verify, opts)(next).ServeHTTP(w, r)
	})
}

// Mount gắn các endpoint MCP vào mux.
func (s *Server) Mount(mux *http.ServeMux) {
	bearer := s.bearer
	opts := &mcp.StreamableHTTPOptions{Stateless: true}

	operatorMCP := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.operatorServer(r.PathValue("operator_id"))
	}, opts)
	mux.Handle("/mcp/operator/{operator_id}/", bearer(s.requireOperator(operatorMCP)))

	platformMCP := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.platform }, opts)
	mux.Handle("/mcp/platform/", bearer(s.requirePlatform(platformMCP)))
}

// verify chuyển token → TokenInfo cho SDK: token cá nhân (biva_, coding agent) hoặc token OAuth (boa_, ChatGPT).
// Lỗi DB chỉ ghi log, không trả chi tiết ra ngoài.
func (s *Server) verify(ctx context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
	var p authz.Principal
	var err error
	if s.oauth != nil && strings.HasPrefix(token, oauth.AccessPrefix) {
		p, err = s.oauth.Verify(ctx, token, s.oauth.Issuer+r.URL.Path)
		if errors.Is(err, oauth.ErrInvalid) {
			err = authz.ErrInvalidToken
		}
	} else {
		p, err = authz.Verify(ctx, s.db, token)
	}
	if errors.Is(err, authz.ErrInvalidToken) {
		// Token sai phải để lại dấu ở LOG (không ghi audit_log — quét token sẽ khuếch đại
		// tải DB): WARN kèm IP + endpoint để phát hiện dò token khi rà nhật ký.
		slog.Warn("mcp: token không hợp lệ", "ip", clientIP(r), "xff", forwardedFor(r), "path", r.URL.Path)
		return nil, fmt.Errorf("%w", auth.ErrInvalidToken)
	}
	if err != nil {
		slog.Error("xác thực MCP lỗi", "err", err)
		return nil, errors.New("xác thực tạm thời không khả dụng")
	}
	return &auth.TokenInfo{
		UserID:     p.UserID,
		Expiration: p.ExpiresAt,
		Extra:      map[string]any{principalKey: p},
	}, nil
}

func principalFrom(ti *auth.TokenInfo) (authz.Principal, bool) {
	if ti == nil {
		return authz.Principal{}, false
	}
	p, ok := ti.Extra[principalKey].(authz.Principal)
	return p, ok
}

func (s *Server) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(auth.TokenInfoFromContext(r.Context()))
		operatorID := r.PathValue("operator_id")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !p.CanAccessOperator(operatorID) {
			s.deny(r, p, "operator:"+operatorID)
			http.Error(w, "forbidden: không có quyền với nhà xe này", http.StatusForbidden)
			return
		}
		var exists bool
		if err := s.db.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM operators WHERE id = $1)`, operatorID).Scan(&exists); err != nil {
			slog.Error("kiểm tra nhà xe lỗi", "err", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !exists {
			http.Error(w, "không có nhà xe này", http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requirePlatform(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(auth.TokenInfoFromContext(r.Context()))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !p.CanAccessPlatform() {
			s.deny(r, p, "platform")
			http.Error(w, "forbidden: endpoint platform chỉ dành cho lead", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// deny ghi audit mọi lần bị từ chối vì sai phạm vi (token hợp lệ nhưng vượt quyền).
func (s *Server) deny(r *http.Request, p authz.Principal, target string) {
	err := audit.Write(r.Context(), s.db, audit.Entry{
		Actor:   p.Actor(),
		Action:  "mcp.denied",
		Target:  target,
		Payload: map[string]any{"role": p.Role, "token_id": p.TokenID, "path": r.URL.Path},
	})
	if err != nil {
		slog.Error("ghi audit lỗi", "err", err)
	}
	slog.Warn("MCP từ chối truy cập", "user", p.UserID, "target", target)
}

func (s *Server) operatorServer(operatorID string) *mcp.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	if srv, ok := s.operators[operatorID]; ok {
		return srv
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "biva-brain/" + operatorID, Version: s.version}, &mcp.ServerOptions{
		Instructions: operatorInstructions(operatorID),
	})
	addOperatorTools(srv, s.db, operatorID)
	s.addRecallTools(srv, operatorID)
	s.addBuildTools(srv, operatorID)
	s.addGuide(srv, operatorID)
	s.addOnboardTools(srv, operatorID)
	s.addLogicTools(srv, operatorID)
	s.addPlanTools(srv, operatorID)
	s.addFamiliesTool(srv, operatorID)
	s.addRunExamplesTool(srv, operatorID)
	s.addLogicTestsTools(srv, operatorID)
	s.addLogicSearchTools(srv, operatorID)
	s.addSharedCatalogResources(srv)
	s.addOperatorCatalogResources(srv, operatorID)
	s.addTestTools(srv, operatorID)
	s.addReflectTools(srv, operatorID)
	s.addReleaseTools(srv, operatorID)
	s.addPublishTools(srv, operatorID)
	s.addLessonTool(srv, operatorID)
	s.addReviewTools(srv, operatorID)
	s.operators[operatorID] = srv
	return srv
}

func (s *Server) newPlatformServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "biva-brain/platform", Version: s.version}, &mcp.ServerOptions{
		Instructions: "BIVA Brain — tri thức L0/L1 (nền tảng, ngành). Chỉ dành cho lead.",
	})
	addPlatformTools(srv, s.db)
	s.addSharedCatalogResources(srv) // lead đọc L0/L1 từ chính endpoint platform
	s.addStaticDocResources(srv)     // + 2 guides + template ngành (DYN-111 duyệt template)
	s.addPlatformMgmt(srv)
	s.addImpactTool(srv)
	s.addRegressionTool(srv)
	s.addApprovePublish(srv)
	return srv
}

// WithStorage: bật lưu bản export_bot lên object storage (BIVA_S3_*).
func (s *Server) WithStorage(st *storage.S3) *Server {
	s.storage = st
	return s
}

// clientIP: RemoteAddr là CHÍNH (X-Forwarded-For do client kiểm soát hoàn toàn khi chưa
// cấu hình proxy tin cậy — dùng nó làm chân trị giúp kẻ dò token nguỵ trang IP, review q2).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedFor: tham chiếu thêm (nếu có) — đọc kèm RemoteAddr để đối chiếu khi rà log.
func forwardedFor(r *http.Request) string {
	return strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
}
