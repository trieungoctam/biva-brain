-- ADR logic (M2, S2.5.5 record_decision): vì sao nhà xe cần hook/custom thay vì module chuẩn.
-- Custom bắt buộc có ADR (propose_logic_profile từ chối custom chưa có decision).

CREATE TABLE logic_decisions (
    id          TEXT PRIMARY KEY,            -- adr_<hex8>, AI/builder tham chiếu trong profile.yaml
    operator_id TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    capability  TEXT NOT NULL,
    title       TEXT NOT NULL,
    context     TEXT NOT NULL DEFAULT '',
    options     JSONB NOT NULL DEFAULT '[]', -- các phương án đã cân nhắc
    decision    TEXT NOT NULL,
    author      TEXT NOT NULL,               -- ai:<session> | user:<id>
    approved_by TEXT,                        -- lead duyệt (console, sau này)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX logic_decisions_operator ON logic_decisions (operator_id, capability);
