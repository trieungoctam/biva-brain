-- Mọi đường apply (MCP apply_review VÀ auto-apply rủi ro thấp trong worker) đều phải chạy
-- consolidate sau khi tri thức đổi — đưa enqueue vào chính hàm SQL thay vì wrapper Go.

-- Hoàn lại apply_review như bản 000023 (guard chỉ theo L2 của operator).
CREATE OR REPLACE FUNCTION apply_review(p_id UUID, p_actor TEXT) RETURNS JSONB
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

    -- E3.2: tri thức của scope đổi → gom lại observation. Enqueue nằm TRONG hàm để mọi đường apply
    -- (MCP apply_review, auto-apply rủi ro thấp của worker) đều chạy consolidate.
    INSERT INTO operations (kind, operator_id, idempotency_key, payload)
    VALUES ('consolidate', r.operator_id, 'consolidate:' || r.operator_id || ':' || p_id, '{}')
    ON CONFLICT (idempotency_key) DO NOTHING;

    INSERT INTO audit_log (actor, action, target, payload)
    VALUES (p_actor, 'review.apply', 'review:' || p_id,
            jsonb_build_object('operator_id', r.operator_id, 'key', r.key, 'change_kind', r.change_kind,
                               'item_id', v_item, 'target_item_id', r.target_item_id));
    RETURN jsonb_build_object('status', 'applied', 'item_id', v_item);
END;
$$;
