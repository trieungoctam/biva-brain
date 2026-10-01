-- confirm_token cho tool MCP ghi rủi ro (docs/mcp.md §8): lần gọi đầu trả preview + token; lần gọi sau mang token
-- mới thực thi. Token gắn với người gọi + hành động + tham số, dùng một lần, hết hạn sau vài phút.
-- Lưu ở DB (không ở RAM) để brain-api chạy nhiều instance vẫn đúng.
CREATE TABLE confirm_tokens (
    token_hash   BYTEA PRIMARY KEY,                -- SHA-256 của token
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operator_id  TEXT REFERENCES operators(id) ON DELETE CASCADE,
    action       TEXT NOT NULL,                    -- vd apply_review
    subject      TEXT NOT NULL,                    -- hash của tham số đã preview (review_id + decision…)
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX confirm_tokens_expiry ON confirm_tokens (expires_at);
