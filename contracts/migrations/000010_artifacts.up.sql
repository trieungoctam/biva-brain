-- Knowledge pack + artifact của bot (M1, S1.4.2, S1.5.1). Thiết kế: docs/mcp.md §3–4, docs/data-model.md.

-- Version tri thức theo scope: '*' = L0/L1 (toàn cục), còn lại = operator_id. Tăng mỗi khi item active đổi theo
-- cách làm đổi knowledge pack (thêm/bỏ/sửa nội dung/hiệu lực) — KHÔNG tăng khi chỉ ghi search_text/embedding
-- (job index) hay proof_count. Cache knowledge pack theo (version '*', version nhà xe) nên tự vô hiệu khi đổi.
CREATE TABLE knowledge_versions (
    scope       TEXT PRIMARY KEY,
    version     BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE FUNCTION items_bump_knowledge_version() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_scope TEXT := COALESCE(CASE WHEN TG_OP = 'DELETE' THEN OLD.operator_id ELSE NEW.operator_id END, '*');
BEGIN
    INSERT INTO knowledge_versions (scope, version) VALUES (v_scope, 1)
    ON CONFLICT (scope) DO UPDATE SET version = knowledge_versions.version + 1, updated_at = now();
    RETURN NULL;
END $$;

CREATE TRIGGER items_version_insert AFTER INSERT ON items FOR EACH ROW
    WHEN (NEW.status = 'active') EXECUTE FUNCTION items_bump_knowledge_version();
CREATE TRIGGER items_version_update AFTER UPDATE ON items FOR EACH ROW
    WHEN ((OLD.status = 'active' OR NEW.status = 'active') AND (
        OLD.status IS DISTINCT FROM NEW.status OR OLD.text IS DISTINCT FROM NEW.text
        OR OLD.value IS DISTINCT FROM NEW.value OR OLD.kind IS DISTINCT FROM NEW.kind
        OR OLD.topic IS DISTINCT FROM NEW.topic OR OLD.key IS DISTINCT FROM NEW.key
        OR OLD.valid_from IS DISTINCT FROM NEW.valid_from OR OLD.valid_to IS DISTINCT FROM NEW.valid_to
        OR OLD.locked IS DISTINCT FROM NEW.locked OR OLD.metadata IS DISTINCT FROM NEW.metadata))
    EXECUTE FUNCTION items_bump_knowledge_version();
CREATE TRIGGER items_version_delete AFTER DELETE ON items FOR EACH ROW
    WHEN (OLD.status = 'active') EXECUTE FUNCTION items_bump_knowledge_version();

-- Các phần của bot do AI viết. Mỗi lần lưu = version mới (không ghi đè); version lớn nhất là bản hiện tại.
CREATE TABLE bot_artifacts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id             TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    operator_id        TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    kind               TEXT NOT NULL
                       CHECK (kind IN ('persona', 'system_prompt', 'faq', 'flows', 'tool_spec', 'fallbacks')),
    version            INTEGER NOT NULL CHECK (version >= 1),
    content            TEXT NOT NULL,
    content_hash       TEXT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'draft'
                       CHECK (status IN ('draft', 'valid', 'invalid', 'stale', 'published')),
    author             TEXT NOT NULL,                      -- user:<id> | ai:<session>
    note               TEXT,                               -- AI ghi đã sửa gì so với version trước
    knowledge_version  TEXT,                               -- version tri thức lúc lưu ('<global>.<operator>')
    validation         JSONB,                              -- kết quả validate_artifact (lỗi có vị trí)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (bot_id, kind, version)
);
CREATE INDEX bot_artifacts_operator ON bot_artifacts (operator_id, bot_id, kind, version DESC);

-- Trích dẫn [[item_id]] trong artifact → item; cơ sở để đánh dấu stale khi item rời active.
CREATE TABLE artifact_citations (
    artifact_id  UUID NOT NULL REFERENCES bot_artifacts(id) ON DELETE CASCADE,
    item_id      UUID NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    line         INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, item_id, line)
);
CREATE INDEX artifact_citations_item ON artifact_citations (item_id);
