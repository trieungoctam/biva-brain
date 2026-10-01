-- Consolidate & promote (M3, E3.2): observation gom từ item của scope + đề xuất promote lên L1.

-- review_items nhận thêm PROMOTE: đề xuất đưa observation của >= 3 nhà xe lên rule L1.
-- operator_id của PROMOTE = nhà xe đại diện (đầu danh sách); danh sách đầy đủ nằm trong after.
DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'review_items'::regclass AND contype = 'c' LOOP
        EXECUTE format('ALTER TABLE review_items DROP CONSTRAINT %I', c);
    END LOOP;
    ALTER TABLE review_items ADD CONSTRAINT review_items_change_kind_check
        CHECK (change_kind IN ('NEW', 'CHANGE', 'REMOVE', 'DUPLICATE', 'CONFLICT', 'PROMOTE'));
    ALTER TABLE review_items ADD CONSTRAINT review_items_item_required
        CHECK ((change_kind IN ('NEW', 'CHANGE', 'CONFLICT', 'PROMOTE')) = (item_id IS NOT NULL));
    ALTER TABLE review_items ADD CONSTRAINT review_items_target_required
        CHECK ((change_kind IN ('CHANGE', 'REMOVE', 'DUPLICATE')) <= (target_item_id IS NOT NULL));
END $$;

-- Nguồn của observation: item nào đã gom vào observation nào, kèm trích đoạn.
CREATE TABLE observation_sources (
    observation_id UUID NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    source_item_id UUID NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    quote          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (observation_id, source_item_id)
);
CREATE INDEX observation_sources_source ON observation_sources (source_item_id);
