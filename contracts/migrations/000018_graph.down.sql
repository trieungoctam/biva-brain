DROP TABLE item_entities;
DROP INDEX IF EXISTS entities_ext;
ALTER TABLE entities DROP COLUMN IF EXISTS ext_id;
