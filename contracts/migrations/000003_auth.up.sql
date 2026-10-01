-- Xác thực & phân quyền (M0, S0.3.3). Thiết kế: docs/system-architecture.md §Bảo mật.
-- Người dùng là người của BIVA (builder/lead/ops) — không phải khách hàng của nhà xe.

CREATE TABLE users (
    id          TEXT PRIMARY KEY,                 -- ví dụ 'tam'
    email       TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    role        TEXT NOT NULL CHECK (role IN ('builder', 'lead', 'ops')),
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- builder chỉ làm việc với nhà xe được gán; lead/ops thấy mọi nhà xe.
CREATE TABLE user_operators (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operator_id  TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    granted_by   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, operator_id)
);
CREATE INDEX user_operators_operator ON user_operators (operator_id);

-- Token cá nhân cho MCP: chỉ lưu SHA-256, token gốc hiện đúng một lần khi cấp.
CREATE TABLE api_tokens (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT '',
    token_hash    BYTEA NOT NULL UNIQUE,
    prefix        TEXT NOT NULL,                  -- vài ký tự đầu để người dùng nhận ra token
    expires_at    TIMESTAMPTZ NOT NULL,           -- token luôn có hạn
    revoked_at    TIMESTAMPTZ,
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_tokens_user ON api_tokens (user_id);
