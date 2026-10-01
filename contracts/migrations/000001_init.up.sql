-- BIVA Brain — schema nền (M0, S0.1.2).
-- Nguồn sự thật duy nhất cho cả Go (brain-api) và Python (ai-worker). Thiết kế: docs/data-model.md.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- ─────────────────────────────── Scope ───────────────────────────────

CREATE TABLE operators (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'onboarding'
                CHECK (status IN ('onboarding', 'live', 'paused', 'offboarded')),
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE bots (
    id                   TEXT PRIMARY KEY,
    operator_id          TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    channel              TEXT NOT NULL CHECK (channel IN ('zalo', 'messenger', 'web')),
    current_snapshot_id  UUID,                         -- FK thêm khi có bảng snapshots
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX bots_operator ON bots (operator_id);

-- ─────────────────────────────── Nguồn ───────────────────────────────

CREATE TABLE documents (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    layer         SMALLINT NOT NULL CHECK (layer BETWEEN 0 AND 3),
    operator_id   TEXT REFERENCES operators(id) ON DELETE CASCADE,
    source        TEXT NOT NULL CHECK (source IN ('zalo', 'excel', 'form', 'console')),
    content       TEXT NOT NULL,
    content_hash  TEXT NOT NULL,
    submitted_by  TEXT,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((layer >= 2) = (operator_id IS NOT NULL))
);
CREATE UNIQUE INDEX documents_dedup ON documents (COALESCE(operator_id, ''), layer, content_hash);

-- ─────────────────────── Tri thức (fact + observation) ───────────────

CREATE TABLE items (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    layer            SMALLINT NOT NULL CHECK (layer BETWEEN 0 AND 3),
    operator_id      TEXT REFERENCES operators(id) ON DELETE CASCADE,
    bot_id           TEXT REFERENCES bots(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL CHECK (kind IN ('data', 'policy', 'lesson', 'persona', 'observation')),
    topic            TEXT NOT NULL,
    key              TEXT,
    text             TEXT NOT NULL,
    value            JSONB,
    search_text      TEXT NOT NULL DEFAULT '',
    tsv              TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple', search_text)) STORED,
    embedding        VECTOR(1024),
    occurred_start   TIMESTAMPTZ,
    occurred_end     TIMESTAMPTZ,
    mentioned_at     TIMESTAMPTZ,
    valid_from       TIMESTAMPTZ,
    valid_to         TIMESTAMPTZ,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('draft', 'pending', 'active', 'superseded', 'expired', 'retracted', 'rejected')),
    superseded_by    UUID REFERENCES items(id) ON DELETE SET NULL,
    locked           BOOLEAN NOT NULL DEFAULT false,
    proof_count      INTEGER NOT NULL DEFAULT 1,
    history          JSONB NOT NULL DEFAULT '[]',
    consolidated_at  TIMESTAMPTZ,
    document_id      UUID REFERENCES documents(id) ON DELETE CASCADE,
    metadata         JSONB NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((layer >= 2) = (operator_id IS NOT NULL)),
    CHECK ((layer = 3) = (bot_id IS NOT NULL)),
    CHECK (locked = false OR layer <= 1),
    CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to > valid_from)
);
CREATE INDEX items_scope_active ON items (operator_id, layer, kind) WHERE status = 'active';
CREATE INDEX items_key ON items (operator_id, key) WHERE status IN ('pending', 'active');
CREATE INDEX items_unconsolidated ON items (operator_id)
    WHERE consolidated_at IS NULL AND kind <> 'observation' AND status = 'active';
CREATE INDEX items_validity ON items (valid_to) WHERE status = 'active' AND valid_to IS NOT NULL;
CREATE INDEX items_tsv ON items USING gin (tsv);
CREATE INDEX items_embedding ON items USING hnsw (embedding vector_cosine_ops);

-- ─────────────────────────────── Entity ──────────────────────────────

CREATE TABLE entities (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    layer          SMALLINT NOT NULL CHECK (layer BETWEEN 0 AND 3),
    operator_id    TEXT REFERENCES operators(id) ON DELETE CASCADE,
    entity_type    TEXT NOT NULL,
    name           TEXT NOT NULL,
    name_norm      TEXT NOT NULL,
    aliases        TEXT[] NOT NULL DEFAULT '{}',
    mention_count  INTEGER NOT NULL DEFAULT 1,
    first_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX entities_name_trgm ON entities USING gin (name_norm gin_trgm_ops);
CREATE INDEX entities_aliases ON entities USING gin (aliases);

-- ─────────────────────── Queue Go ⇄ Python ───────────────────────────

CREATE TABLE operations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             TEXT NOT NULL,
    operator_id      TEXT REFERENCES operators(id) ON DELETE CASCADE,
    payload          JSONB NOT NULL DEFAULT '{}',
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    priority         SMALLINT NOT NULL DEFAULT 100,
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 5,
    idempotency_key  TEXT UNIQUE,
    locked_by        TEXT,
    lease_until      TIMESTAMPTZ,
    run_after        TIMESTAMPTZ NOT NULL DEFAULT now(),
    result           JSONB,
    error            TEXT,
    parent_id        UUID REFERENCES operations(id) ON DELETE SET NULL,
    trace_context    JSONB NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX operations_claim ON operations (priority, run_after) WHERE status = 'queued';
CREATE INDEX operations_lease ON operations (lease_until) WHERE status = 'running';

-- ─────────────────────────────── Audit ───────────────────────────────

CREATE TABLE audit_log (
    id           BIGSERIAL PRIMARY KEY,
    actor        TEXT NOT NULL,                    -- user:<id> | ai:<session> | system:<job>
    approved_by  TEXT,
    action       TEXT NOT NULL,
    target       TEXT,
    payload      JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_target ON audit_log (target, created_at);
