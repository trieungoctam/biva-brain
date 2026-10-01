-- OAuth 2.1 cho MCP (DYN-115): brain-api là authorization server cho client như ChatGPT (connector).
-- Client tự đăng ký (RFC 7591), authorization code + PKCE S256, access/refresh token dạng opaque (chỉ lưu SHA-256).
-- Builder đăng nhập ở trang authorize bằng token cá nhân (api_tokens); mọi token OAuth gắn với token cá nhân đó:
-- thu hồi/hết hạn token cá nhân → token OAuth sinh ra từ nó mất hiệu lực ngay.

CREATE TABLE oauth_clients (
    client_id           TEXT PRIMARY KEY,
    client_name         TEXT NOT NULL DEFAULT '',
    redirect_uris       TEXT[] NOT NULL,
    auth_method         TEXT NOT NULL DEFAULT 'none'
                        CHECK (auth_method IN ('none', 'client_secret_post', 'client_secret_basic')),
    client_secret_hash  BYTEA,                    -- chỉ với client_secret_*
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((auth_method = 'none') = (client_secret_hash IS NULL))
);

CREATE TABLE oauth_codes (
    code_hash       BYTEA PRIMARY KEY,
    client_id       TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    login_token_id  UUID NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    redirect_uri    TEXT NOT NULL,
    code_challenge  TEXT NOT NULL,
    resource        TEXT NOT NULL DEFAULT '',
    scope           TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ
);

CREATE TABLE oauth_tokens (
    token_hash      BYTEA PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
    family          UUID NOT NULL,              -- một lần đăng nhập; refresh xoay vòng trong cùng family
    client_id       TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    login_token_id  UUID NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    resource        TEXT NOT NULL DEFAULT '',
    scope           TEXT NOT NULL DEFAULT '',
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    used_at         TIMESTAMPTZ,                -- refresh token đã dùng (xoay vòng)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX oauth_tokens_family ON oauth_tokens (family);
CREATE INDEX oauth_tokens_expiry ON oauth_tokens (expires_at);
