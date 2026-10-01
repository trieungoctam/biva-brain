-- Quên → stale (M2, S2.3.1–S2.3.2). Thiết kế: docs/architecture.md §3.1 bước 8, docs/mcp.md §4 "Quên → build lại".
--
-- Khi tri thức đổi, bản mới nhất của mỗi artifact bị ảnh hưởng chuyển 'stale', kèm stale_reasons chỉ đúng dòng:
--   item rời active (superseded / expired / retracted)  → mọi artifact trích dẫn item đó
--   item L2 (không phải data) active ở topic T          → artifact của nhà xe đang trích thông lệ L1 của topic T
--   rule locked L0/L1 mới active                        → system_prompt chưa trích rule đó
-- Chạy bằng constraint trigger hoãn tới cuối transaction (đọc lại item ở trạng thái cuối, vd superseded_by được
-- apply_review ghi sau status) — artifact stale ngay khi thay đổi tri thức commit, không cần job.

ALTER TABLE bot_artifacts
    ADD COLUMN stale_reasons JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN stale_at TIMESTAMPTZ;

-- Bản mới nhất của (bot, kind) — chỉ bản này bị đánh dấu; bản cũ là lịch sử.
CREATE FUNCTION artifact_is_latest(p_id UUID) RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT a.version = (SELECT max(b.version) FROM bot_artifacts b WHERE b.bot_id = a.bot_id AND b.kind = a.kind)
    FROM bot_artifacts a WHERE a.id = p_id
$$;

CREATE FUNCTION items_mark_stale() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    it items%ROWTYPE;
BEGIN
    SELECT * INTO it FROM items WHERE id = NEW.id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    IF it.status IN ('superseded', 'expired', 'retracted') THEN
        UPDATE bot_artifacts a
        SET status = 'stale', stale_at = COALESCE(a.stale_at, now()), updated_at = now(),
            stale_reasons = a.stale_reasons || (
                SELECT jsonb_agg(jsonb_build_object('line', c.line, 'item', c.item_id::text, 'reason', it.status,
                                                    'superseded_by', it.superseded_by::text) ORDER BY c.line)
                FROM artifact_citations c WHERE c.artifact_id = a.id AND c.item_id = it.id)
        WHERE a.id IN (SELECT artifact_id FROM artifact_citations WHERE item_id = it.id)
          AND a.status IN ('draft', 'valid', 'invalid', 'published', 'stale')
          AND artifact_is_latest(a.id)
          AND NOT a.stale_reasons @> jsonb_build_array(jsonb_build_object('item', it.id::text, 'reason', it.status));

    ELSIF it.status = 'active' AND it.layer = 2 AND it.kind <> 'data' THEN
        UPDATE bot_artifacts a
        SET status = 'stale', stale_at = COALESCE(a.stale_at, now()), updated_at = now(),
            stale_reasons = a.stale_reasons || (
                SELECT jsonb_agg(jsonb_build_object('line', c.line, 'item', c.item_id::text,
                                                    'reason', 'overridden_default', 'superseded_by', it.id::text)
                                 ORDER BY c.line)
                FROM artifact_citations c JOIN items d ON d.id = c.item_id
                WHERE c.artifact_id = a.id AND d.layer = 1 AND NOT d.locked AND d.topic = it.topic)
        WHERE a.operator_id = it.operator_id
          AND a.status IN ('draft', 'valid', 'invalid', 'published', 'stale')
          AND EXISTS (SELECT 1 FROM artifact_citations c JOIN items d ON d.id = c.item_id
                      WHERE c.artifact_id = a.id AND d.layer = 1 AND NOT d.locked AND d.topic = it.topic)
          AND artifact_is_latest(a.id)
          AND NOT a.stale_reasons @> jsonb_build_array(jsonb_build_object('superseded_by', it.id::text,
                                                                          'reason', 'overridden_default'));

    ELSIF it.status = 'active' AND it.layer <= 1 AND it.locked THEN
        UPDATE bot_artifacts a
        SET status = 'stale', stale_at = COALESCE(a.stale_at, now()), updated_at = now(),
            stale_reasons = a.stale_reasons || jsonb_build_array(
                jsonb_build_object('item', it.id::text, 'reason', 'new_locked_rule'))
        WHERE a.kind = 'system_prompt'
          AND a.status IN ('draft', 'valid', 'invalid', 'published', 'stale')
          AND NOT EXISTS (SELECT 1 FROM artifact_citations c WHERE c.artifact_id = a.id AND c.item_id = it.id)
          AND artifact_is_latest(a.id)
          AND NOT a.stale_reasons @> jsonb_build_array(jsonb_build_object('item', it.id::text,
                                                                          'reason', 'new_locked_rule'));
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER items_stale_insert AFTER INSERT ON items DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.status = 'active') EXECUTE FUNCTION items_mark_stale();
CREATE CONSTRAINT TRIGGER items_stale_update AFTER UPDATE ON items DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION items_mark_stale();

CREATE INDEX bot_artifacts_stale ON bot_artifacts (operator_id) WHERE status = 'stale';
