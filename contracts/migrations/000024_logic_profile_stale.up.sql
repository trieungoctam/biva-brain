-- Stale cho logic profile (M2 bổ sung sau review chất lượng). Thiết kế: docs/data-model.md
-- logic_param_sources — "mỗi tham số có source là item tri thức; đây là chỗ Brain gắn stale
-- khi tri thức đổi". Trước đây chỉ bot_artifacts được đánh stale; profile logic giữ status
-- 'active' và giá cũ dù item nguồn đã bị thay/hết hạn/rút → executor vẫn chạy theo giá cũ.
--
-- Mở rộng items_mark_stale (000011): nhánh item rời active giờ cũng chuyển mọi profile lấy
-- tham số từ item đó sang 'stale'. logic_sync upsert đã set status='active' khi đồng bộ lại
-- (merge PR sửa profile) nên đường hồi phục đã có sẵn.

CREATE OR REPLACE FUNCTION items_mark_stale() RETURNS trigger LANGUAGE plpgsql AS $$
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

        -- Profile logic lấy tham số từ item này không còn hợp lệ nữa.
        UPDATE logic_profiles p
        SET status = 'stale'
        WHERE p.id IN (SELECT profile_id FROM logic_param_sources WHERE item_id = it.id)
          AND p.status = 'active';

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
