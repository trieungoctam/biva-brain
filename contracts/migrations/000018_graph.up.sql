-- Graph arm của recall (M3, S3.1.1): nối item với thực thể nó nhắc tới.
-- entities.ext_id: id ổn định từ kb/ (vd "hcm") để kb sync upsert không sinh bản trùng;
-- item_entities điền bởi job index.items (so khớp tên/alias sau textnorm, ranh giới token).

ALTER TABLE entities ADD COLUMN ext_id TEXT UNIQUE;
CREATE INDEX entities_ext ON entities (ext_id) WHERE ext_id IS NOT NULL;

CREATE TABLE item_entities (
    item_id   UUID NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    entity_id UUID NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    PRIMARY KEY (item_id, entity_id)
);
CREATE INDEX item_entities_entity ON item_entities (entity_id);
