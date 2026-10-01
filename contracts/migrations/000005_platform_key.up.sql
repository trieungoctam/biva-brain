-- Tri thức nền L0/L1 nạp từ kb/ (brain-api kb sync): mỗi key chỉ có đúng một bản active.
-- Bản cũ khi đổi nội dung → superseded (giữ lịch sử, trích dẫn cũ thành stale); bị bỏ khỏi kb/ → retracted.
CREATE UNIQUE INDEX items_platform_key_active ON items (layer, key)
    WHERE operator_id IS NULL AND key IS NOT NULL AND status = 'active';
