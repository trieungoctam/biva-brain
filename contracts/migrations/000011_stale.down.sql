DROP INDEX IF EXISTS bot_artifacts_stale;
DROP TRIGGER IF EXISTS items_stale_update ON items;
DROP TRIGGER IF EXISTS items_stale_insert ON items;
DROP FUNCTION IF EXISTS items_mark_stale();
DROP FUNCTION IF EXISTS artifact_is_latest(UUID);
ALTER TABLE bot_artifacts DROP COLUMN IF EXISTS stale_at, DROP COLUMN IF EXISTS stale_reasons;
