-- Review queue + apply có version (M1, S1.2.1–S1.2.3). Thiết kế: docs/architecture.md §3.1, docs/data-model.md.
--
-- Một review_item = một thay đổi đề xuất cho một key của nhà xe:
--   NEW       item mới (item_id: bản pending)
--   CHANGE    sửa item đang active (target_item_id) bằng bản pending (item_id)
--   REMOVE    bỏ item đang active (target_item_id)
--   DUPLICATE nhắc lại đúng nội dung đang active → chỉ tăng proof_count
--   CONFLICT  cùng một nguồn nói hai điều khác nhau cho cùng key → người duyệt chọn
-- Apply/reject nằm trong SQL (apply_review / reject_review): Go (MCP apply_review) và Python (tự apply rủi ro thấp)
-- dùng chung một logic.

-- Mỗi key của một scope: các bản active KHÔNG chồng khoảng hiệu lực. Cho phép "giá hiện tại tới 31/10" và
-- "giá mới từ 1/11" cùng active; tới ngày, job expire chuyển bản cũ sang expired (trục thời gian valid_*).
CREATE EXTENSION IF NOT EXISTS btree_gist;
ALTER TABLE items ADD CONSTRAINT items_scope_key_validity EXCLUDE USING gist (
    operator_id WITH =, (COALESCE(bot_id, '')) WITH =, layer WITH =, key WITH =,
    tstzrange(valid_from, valid_to) WITH &&
) WHERE (operator_id IS NOT NULL AND key IS NOT NULL AND status = 'active');

CREATE TABLE review_items (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id     TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    topic           TEXT NOT NULL,
    change_kind     TEXT NOT NULL CHECK (change_kind IN ('NEW', 'CHANGE', 'REMOVE', 'DUPLICATE', 'CONFLICT')),
    risk            TEXT NOT NULL CHECK (risk IN ('low', 'high')),
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'applied', 'rejected', 'stale')),
    item_id         UUID REFERENCES items(id) ON DELETE CASCADE,   -- bản đề xuất (pending)
    target_item_id  UUID REFERENCES items(id) ON DELETE CASCADE,   -- bản đang active bị ảnh hưởng
    before          JSONB,
    after           JSONB,
    reason          TEXT,
    document_id     UUID REFERENCES documents(id) ON DELETE SET NULL,
    operation_id    UUID,
    proposed_by     TEXT NOT NULL,                                 -- system:ingest | user:<id> | ai:<session>
    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((change_kind IN ('NEW', 'CHANGE', 'CONFLICT')) = (item_id IS NOT NULL)),
    CHECK ((change_kind IN ('CHANGE', 'REMOVE', 'DUPLICATE')) <= (target_item_id IS NOT NULL))
);
CREATE INDEX review_items_queue ON review_items (operator_id, status, risk, created_at);
CREATE INDEX review_items_key_open ON review_items (operator_id, key) WHERE status = 'open';

-- apply_review: áp dụng một review đang mở trong transaction của caller. Trả về JSONB:
--   {"status": "applied", "item_id": <item active sau khi áp dụng, null với REMOVE>}
--   {"status": "stale", "reason": ...}  trạng thái đã đổi từ lúc đề xuất → review bị đánh dấu stale (không lỗi,
--                                       để caller commit được dấu stale), cần ingest/đề xuất lại.
-- Review không tồn tại / không còn mở → RAISE. Review khác đang mở cho cùng key → 'stale' sau khi apply.
CREATE FUNCTION apply_review(p_id UUID, p_actor TEXT) RETURNS JSONB
LANGUAGE plpgsql AS $$
DECLARE
    r           review_items%ROWTYPE;
    v_item      UUID;
    v_new_from  TIMESTAMPTZ;
    v_old_from  TIMESTAMPTZ;
BEGIN
    SELECT * INTO r FROM review_items WHERE id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'review % không tồn tại', p_id USING ERRCODE = 'no_data_found';
    END IF;
    IF r.status <> 'open' THEN
        RAISE EXCEPTION 'review % đã ở trạng thái %', p_id, r.status USING ERRCODE = 'check_violation';
    END IF;

    -- Bản đang active phải còn đúng như lúc tạo review (không ai sửa key này trong lúc chờ duyệt).
    IF r.target_item_id IS NOT NULL THEN
        PERFORM 1 FROM items WHERE id = r.target_item_id AND status = 'active' FOR UPDATE;
        IF NOT FOUND THEN
            UPDATE review_items SET status = 'stale', decided_by = p_actor, decided_at = now() WHERE id = p_id;
            UPDATE items SET status = 'rejected', updated_at = now() WHERE id = r.item_id AND status = 'pending';
            RETURN jsonb_build_object('status', 'stale', 'reason', 'item gốc đã thay đổi từ lúc đề xuất');
        END IF;
    END IF;

    IF r.change_kind = 'DUPLICATE' THEN
        UPDATE items SET proof_count = proof_count + 1, mentioned_at = now(), updated_at = now()
        WHERE id = r.target_item_id;
        v_item := r.target_item_id;
    ELSIF r.change_kind = 'REMOVE' THEN
        UPDATE items SET status = 'retracted', updated_at = now() WHERE id = r.target_item_id;
        v_item := NULL;
    ELSE  -- NEW, CHANGE, CONFLICT (người duyệt chọn bản này)
        SELECT valid_from INTO v_new_from FROM items WHERE id = r.item_id;
        IF r.target_item_id IS NOT NULL THEN
            SELECT valid_from INTO v_old_from FROM items WHERE id = r.target_item_id;
            IF v_new_from > now() AND (v_old_from IS NULL OR v_old_from < v_new_from) THEN
                -- Bản mới chỉ có hiệu lực từ một ngày tương lai: bản cũ vẫn đúng tới ngày đó.
                UPDATE items SET valid_to = LEAST(COALESCE(valid_to, v_new_from), v_new_from), updated_at = now()
                WHERE id = r.target_item_id;
            ELSE
                UPDATE items SET status = 'superseded', updated_at = now() WHERE id = r.target_item_id;
            END IF;
        ELSE
            -- CONFLICT/NEW không có target nhưng key đã có bản active (do review khác vừa apply) → stale.
            PERFORM 1 FROM items
            WHERE operator_id = r.operator_id AND layer = 2 AND key = r.key AND status = 'active';
            IF FOUND THEN
                UPDATE review_items SET status = 'stale', decided_by = p_actor, decided_at = now() WHERE id = p_id;
                UPDATE items SET status = 'rejected', updated_at = now() WHERE id = r.item_id AND status = 'pending';
                RETURN jsonb_build_object('status', 'stale', 'reason', 'key đã có bản active từ lúc đề xuất');
            END IF;
        END IF;
        UPDATE items SET status = 'active', valid_from = COALESCE(valid_from, now()), updated_at = now()
        WHERE id = r.item_id AND status = 'pending';
        IF NOT FOUND THEN
            RAISE EXCEPTION 'review %: item đề xuất không còn ở trạng thái pending', p_id USING ERRCODE = 'check_violation';
        END IF;
        IF r.target_item_id IS NOT NULL THEN
            UPDATE items SET superseded_by = r.item_id WHERE id = r.target_item_id;
        END IF;
        v_item := r.item_id;
    END IF;

    UPDATE review_items SET status = 'applied', decided_by = p_actor, decided_at = now() WHERE id = p_id;

    -- Các đề xuất khác đang mở cho cùng key dựa trên trạng thái cũ → stale; bản pending của chúng bị bỏ.
    UPDATE items SET status = 'rejected', updated_at = now()
    WHERE id IN (SELECT item_id FROM review_items
                 WHERE operator_id = r.operator_id AND key = r.key AND status = 'open' AND item_id IS NOT NULL);
    UPDATE review_items SET status = 'stale'
    WHERE operator_id = r.operator_id AND key = r.key AND status = 'open';

    INSERT INTO audit_log (actor, action, target, payload)
    VALUES (p_actor, 'review.apply', 'review:' || p_id,
            jsonb_build_object('operator_id', r.operator_id, 'key', r.key, 'change_kind', r.change_kind,
                               'item_id', v_item, 'target_item_id', r.target_item_id));
    RETURN jsonb_build_object('status', 'applied', 'item_id', v_item);
END;
$$;

CREATE FUNCTION reject_review(p_id UUID, p_actor TEXT, p_reason TEXT) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    r review_items%ROWTYPE;
BEGIN
    SELECT * INTO r FROM review_items WHERE id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'review % không tồn tại', p_id USING ERRCODE = 'no_data_found';
    END IF;
    IF r.status <> 'open' THEN
        RAISE EXCEPTION 'review % đã ở trạng thái %', p_id, r.status USING ERRCODE = 'check_violation';
    END IF;
    IF r.item_id IS NOT NULL THEN
        UPDATE items SET status = 'rejected', updated_at = now() WHERE id = r.item_id AND status = 'pending';
    END IF;
    UPDATE review_items SET status = 'rejected', decided_by = p_actor, decided_at = now(),
                            reason = COALESCE(p_reason, reason)
    WHERE id = p_id;
    INSERT INTO audit_log (actor, action, target, payload)
    VALUES (p_actor, 'review.reject', 'review:' || p_id,
            jsonb_build_object('operator_id', r.operator_id, 'key', r.key, 'reason', p_reason));
END;
$$;
