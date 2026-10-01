// Package confirm cấp và kiểm confirm_token cho tool MCP ghi rủi ro (preview → xác nhận).
//
// Token gắn với (người gọi, nhà xe, hành động, subject = hash tham số đã preview), dùng một lần, hết hạn sau TTL.
// Nhờ vậy AI không thể dùng token của một preview để thực thi một hành động khác.
package confirm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultTTL = 5 * time.Minute

var ErrInvalid = errors.New("confirm_token không hợp lệ, đã dùng, hết hạn hoặc không khớp hành động — hãy gọi lại để xem preview mới")

// Subject băm các tham số đã preview thành một chuỗi cố định.
func Subject(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

func hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func Issue(ctx context.Context, db *pgxpool.Pool, userID, operatorID, action, subject string, ttl time.Duration) (string, time.Time, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token := "ct_" + base64.RawURLEncoding.EncodeToString(b)
	var exp time.Time
	err := db.QueryRow(ctx, `
		INSERT INTO confirm_tokens (token_hash, user_id, operator_id, action, subject, expires_at)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, now() + make_interval(secs => $6))
		RETURNING expires_at`,
		hash(token), userID, operatorID, action, subject, ttl.Seconds(),
	).Scan(&exp)
	if err != nil {
		return "", time.Time{}, err
	}
	// Dọn token hết hạn từ lâu (rẻ, nhờ index expires_at).
	_, _ = db.Exec(ctx, `DELETE FROM confirm_tokens WHERE expires_at < now() - interval '1 day'`)
	return token, exp, nil
}

// Consume đánh dấu token đã dùng nếu khớp mọi điều kiện; một token chỉ consume được đúng một lần.
func Consume(ctx context.Context, db *pgxpool.Pool, token, userID, operatorID, action, subject string) error {
	tag, err := db.Exec(ctx, `
		UPDATE confirm_tokens SET used_at = now()
		WHERE token_hash = $1 AND user_id = $2 AND operator_id IS NOT DISTINCT FROM NULLIF($3, '')
		  AND action = $4 AND subject = $5 AND used_at IS NULL AND expires_at > now()`,
		hash(token), userID, operatorID, action, subject)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrInvalid
	}
	return nil
}
