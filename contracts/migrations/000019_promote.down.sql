DROP TABLE observation_sources;

DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'review_items'::regclass AND contype = 'c' LOOP
        EXECUTE format('ALTER TABLE review_items DROP CONSTRAINT %I', c);
    END LOOP;
    ALTER TABLE review_items ADD CONSTRAINT review_items_change_kind_check
        CHECK (change_kind IN ('NEW', 'CHANGE', 'REMOVE', 'DUPLICATE', 'CONFLICT'));
    ALTER TABLE review_items ADD CONSTRAINT review_items_item_required
        CHECK ((change_kind IN ('NEW', 'CHANGE', 'CONFLICT')) = (item_id IS NOT NULL));
    ALTER TABLE review_items ADD CONSTRAINT review_items_target_required
        CHECK ((change_kind IN ('CHANGE', 'REMOVE', 'DUPLICATE')) <= (target_item_id IS NOT NULL));
END $$;
