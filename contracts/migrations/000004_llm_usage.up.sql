-- Đo chi phí LLM theo nhà xe / purpose / model (M1, S1.1.1). Mỗi lần gọi model (kể cả lỗi) = 1 dòng.
CREATE TABLE llm_usage (
    id                  BIGSERIAL PRIMARY KEY,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    operator_id         TEXT,                     -- không FK: giữ số liệu chi phí cả khi nhà xe bị xoá
    operation_id        UUID,                     -- job sinh ra lời gọi (nếu có)
    purpose             TEXT NOT NULL,
    provider            TEXT NOT NULL,
    model               TEXT NOT NULL,
    served_model        TEXT,                     -- model thực sự trả lời (khác model nếu fallback phía server)
    ok                  BOOLEAN NOT NULL,
    error_code          TEXT,
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd            NUMERIC(12, 6) NOT NULL DEFAULT 0,
    latency_ms          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX llm_usage_operator_time ON llm_usage (operator_id, created_at);
CREATE INDEX llm_usage_purpose_time ON llm_usage (purpose, created_at);
