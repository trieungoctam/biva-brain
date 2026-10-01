// Package authz: người dùng, token cá nhân cho MCP, role và phạm vi nhà xe.
//
//   - Token dạng "biva_<43 ký tự base64url>" (256 bit ngẫu nhiên); DB chỉ lưu SHA-256.
//   - Role: builder (chỉ nhà xe được gán) · lead (mọi nhà xe + platform) · ops (mọi nhà xe).
//   - Phạm vi nhà xe cố định theo URL MCP: /mcp/operator/{id}/.
package authz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tokenPrefix = "biva_"

var (
	ErrInvalidToken = errors.New("token không hợp lệ, hết hạn hoặc đã thu hồi")
	ErrNotFound     = errors.New("không tìm thấy")
)

type Role string

const (
	RoleBuilder Role = "builder"
	RoleLead    Role = "lead"
	RoleOps     Role = "ops"
)

func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case RoleBuilder, RoleLead, RoleOps:
		return r, nil
	}
	return "", fmt.Errorf("role %q không hợp lệ (builder | lead | ops)", s)
}

// Principal là người đang gọi, dựng từ token.
type Principal struct {
	UserID    string
	Role      Role
	TokenID   string
	ExpiresAt time.Time
	Operators []string // chỉ có ý nghĩa với builder
}

// Actor theo quy ước của audit_log.
func (p Principal) Actor() string { return "user:" + p.UserID }

// CanAccessOperator: lead/ops mọi nhà xe; builder chỉ nhà xe được gán.
func (p Principal) CanAccessOperator(operatorID string) bool {
	if p.Role == RoleLead || p.Role == RoleOps {
		return true
	}
	return slices.Contains(p.Operators, operatorID)
}

// CanAccessPlatform: tri thức L0/L1 toàn cục chỉ cho lead.
func (p Principal) CanAccessPlatform() bool { return p.Role == RoleLead }

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// NewToken sinh token ngẫu nhiên; trả về token gốc (chỉ hiện một lần) và prefix để nhận diện.
func NewToken() (token, prefix string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, token[:len(tokenPrefix)+6], nil
}

// Verify tra token → Principal. Mọi lý do từ chối đều trả cùng ErrInvalidToken.
func Verify(ctx context.Context, db *pgxpool.Pool, token string) (Principal, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return Principal{}, ErrInvalidToken
	}
	var p Principal
	err := db.QueryRow(ctx, `
		SELECT t.id::text, t.expires_at, u.id, u.role,
		       COALESCE(array_agg(uo.operator_id) FILTER (WHERE uo.operator_id IS NOT NULL), '{}')
		FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN user_operators uo ON uo.user_id = u.id
		WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND t.expires_at > now() AND u.status = 'active'
		GROUP BY t.id, u.id`, hashToken(token),
	).Scan(&p.TokenID, &p.ExpiresAt, &p.UserID, &p.Role, &p.Operators)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidToken
	}
	if err != nil {
		return Principal{}, fmt.Errorf("tra token: %w", err)
	}
	// Ghi lần dùng cuối, tối đa mỗi phút một lần để không ghi DB ở mọi request.
	_, _ = db.Exec(ctx, `
		UPDATE api_tokens SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, p.TokenID)
	return p, nil
}

// ─────────────────────────── quản trị (CLI) ───────────────────────────

func CreateUser(ctx context.Context, db *pgxpool.Pool, id, email, name string, role Role) error {
	_, err := db.Exec(ctx, `INSERT INTO users (id, email, name, role) VALUES ($1, $2, $3, $4)`, id, email, name, role)
	return err
}

func GrantOperator(ctx context.Context, db *pgxpool.Pool, userID, operatorID, grantedBy string) error {
	_, err := db.Exec(ctx, `
		INSERT INTO user_operators (user_id, operator_id, granted_by) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, userID, operatorID, grantedBy)
	return err
}

func RevokeOperator(ctx context.Context, db *pgxpool.Pool, userID, operatorID string) error {
	_, err := db.Exec(ctx, `DELETE FROM user_operators WHERE user_id = $1 AND operator_id = $2`, userID, operatorID)
	return err
}

// IssueToken cấp token mới; token gốc chỉ trả về ở đây.
func IssueToken(ctx context.Context, db *pgxpool.Pool, userID, name string, ttl time.Duration) (id, token string, expiresAt time.Time, err error) {
	if ttl <= 0 {
		return "", "", time.Time{}, errors.New("ttl phải > 0")
	}
	token, prefix, err := NewToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	err = db.QueryRow(ctx, `
		INSERT INTO api_tokens (user_id, name, token_hash, prefix, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))
		RETURNING id::text, expires_at`,
		userID, name, hashToken(token), prefix, ttl.Seconds(),
	).Scan(&id, &expiresAt)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("cấp token: %w", err)
	}
	return id, token, expiresAt, nil
}

func RevokeToken(ctx context.Context, db *pgxpool.Pool, tokenID string) error {
	tag, err := db.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, tokenID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LoadPrincipal dựng Principal cho một user đang active (dùng khi xác thực bằng token OAuth).
func LoadPrincipal(ctx context.Context, db *pgxpool.Pool, userID, tokenID string, expiresAt time.Time) (Principal, error) {
	p := Principal{UserID: userID, TokenID: tokenID, ExpiresAt: expiresAt}
	err := db.QueryRow(ctx, `
		SELECT u.role, COALESCE(array_agg(uo.operator_id) FILTER (WHERE uo.operator_id IS NOT NULL), '{}')
		FROM users u LEFT JOIN user_operators uo ON uo.user_id = u.id
		WHERE u.id = $1 AND u.status = 'active' GROUP BY u.id`, userID).Scan(&p.Role, &p.Operators)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidToken
	}
	return p, err
}
