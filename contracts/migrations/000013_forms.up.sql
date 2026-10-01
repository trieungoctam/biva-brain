-- Form hỏi nhà xe (M2, S2.1.3): builder tạo link (token bí mật, chỉ lưu SHA-256), nhà xe mở trên điện thoại,
-- trả lời → job ingest (source=form) → review queue như mọi nguồn khác.
CREATE TABLE forms (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id   TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL UNIQUE,
    title         TEXT NOT NULL,
    questions     JSONB NOT NULL,              -- [{topic, question}]
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'submitted', 'closed')),
    answers       JSONB,                       -- [{topic, question, answer}]
    operation_id  UUID,                        -- job ingest của câu trả lời
    created_by    TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    submitted_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX forms_operator ON forms (operator_id, created_at DESC);
