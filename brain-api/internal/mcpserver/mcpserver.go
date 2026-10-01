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
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trieungoctam/biva-brain/brain-api/internal/audit"
	"github.com/trieungoctam/biva-brain/brain-api/internal/authz"
)

const principalKey = "principal"

type Server struct {
	db      *pgxpool.Pool
	version string

	mu        sync.Mutex
	operators map[string]*mcp.Server // MCP server theo nhà xe, dựng một lần
	platform  *mcp.Server
}

func New(db *pgxpool.Pool, version string) *Server {
	s := &Server{db: db, version: version, operators: map[string]*mcp.Server{}}
	s.platform = s.newPlatformServer()
	return s
}

// Mount gắn các endpoint MCP vào mux.
func (s *Server) Mount(mux *http.ServeMux) {
	bearer := auth.RequireBearerToken(s.verify, nil)
	opts := &mcp.StreamableHTTPOptions{Stateless: true}

	operatorMCP := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.operatorServer(r.PathValue("operator_id"))
	}, opts)
	mux.Handle("/mcp/operator/{operator_id}/", bearer(s.requireOperator(operatorMCP)))

	platformMCP := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.platform }, opts)
	mux.Handle("/mcp/platform/", bearer(s.requirePlatform(platformMCP)))
}

// verify chuyển token → TokenInfo cho SDK. Lỗi DB chỉ ghi log, không trả chi tiết ra ngoài.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	p, err := authz.Verify(ctx, s.db, token)
	if errors.Is(err, authz.ErrInvalidToken) {
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
		Instructions: "BIVA Brain — tri thức của nhà xe " + operatorID + " để build bot. " +
			"Mọi tool đã cố định trong phạm vi nhà xe này.",
	})
	addOperatorTools(srv, s.db, operatorID)
	s.operators[operatorID] = srv
	return srv
}

func (s *Server) newPlatformServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "biva-brain/platform", Version: s.version}, &mcp.ServerOptions{
		Instructions: "BIVA Brain — tri thức L0/L1 (nền tảng, ngành). Chỉ dành cho lead.",
	})
	addPlatformTools(srv, s.db)
	return srv
}
