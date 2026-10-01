DROP TABLE IF EXISTS artifact_citations;
DROP TABLE IF EXISTS bot_artifacts;
DROP TRIGGER IF EXISTS items_version_delete ON items;
DROP TRIGGER IF EXISTS items_version_update ON items;
DROP TRIGGER IF EXISTS items_version_insert ON items;
DROP FUNCTION IF EXISTS items_bump_knowledge_version();
DROP TABLE IF EXISTS knowledge_versions;
