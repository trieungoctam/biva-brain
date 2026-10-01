-- Tri thức logic (M2, S2.5.2 index_code): đồng bộ từ repo biva-integrations — module.yaml,
-- profile.yaml, tests/cases.yaml, code chunk theo hàm/lớp gắn commit.
-- Thiết kế: docs/logic-knowledge.md §3.2; chú thích cột: docs/data-model.md "Tri thức logic".

CREATE TABLE logic_modules (
    id             TEXT NOT NULL,                       -- fare.standard | phuongnam.transfer_q7
    version        INTEGER NOT NULL CHECK (version >= 1),
    layer          TEXT NOT NULL CHECK (layer IN ('L1', 'L2')),
    operator_id    TEXT REFERENCES operators(id) ON DELETE CASCADE,  -- NULL với module L1
    capability     TEXT NOT NULL,
    summary        TEXT NOT NULL,
    entrypoint     TEXT NOT NULL,                       -- calculator.py:calculate
    features       TEXT[] NOT NULL DEFAULT '{}',
    params_schema  JSONB NOT NULL DEFAULT '{}',
    hooks          JSONB NOT NULL DEFAULT '[]',
    required_tests TEXT[] NOT NULL DEFAULT '{}',
    deprecated_by  TEXT,
    repo           TEXT NOT NULL,
    path           TEXT NOT NULL,                       -- modules/fare/standard
    commit         TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deprecated', 'retired')),
    proof_count    INTEGER NOT NULL DEFAULT 1,
    synced_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, version)                           -- version mới không đè bản cũ (impact_of_change)
);
CREATE INDEX logic_modules_operator ON logic_modules (operator_id) WHERE operator_id IS NOT NULL;
CREATE INDEX logic_modules_capability ON logic_modules (capability);

-- Hồ sơ logic nhà xe theo capability; upsert theo (operator, capability) — lịch sử nằm trong git.
CREATE TABLE logic_profiles (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id  TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    capability   TEXT NOT NULL,
    mode         TEXT NOT NULL CHECK (mode IN ('config', 'hook', 'custom')),
    module_id    TEXT NOT NULL,                          -- fare.standard (không kèm version)
    module_version INTEGER NOT NULL DEFAULT 1,           -- phần @version trong profile.yaml
    params       JSONB NOT NULL DEFAULT '{}',
    hooks        JSONB NOT NULL DEFAULT '{}',
    entrypoint   TEXT,
    decision_id  TEXT,                                  -- ADR trong profile.yaml
    derived_from TEXT,
    commit       TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('pending', 'active', 'stale', 'retired')),
    synced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (operator_id, capability)
);

CREATE TABLE logic_param_sources (
    profile_id UUID NOT NULL REFERENCES logic_profiles(id) ON DELETE CASCADE,
    param_path TEXT NOT NULL,                           -- holiday_surcharge.tet
    item_id    UUID NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    PRIMARY KEY (profile_id, param_path, item_id)
);

-- Danh mục feature L1 (seed ở S2.5.3; index_code đếm operator_count mỗi lần sync).
CREATE TABLE logic_features (
    id             TEXT PRIMARY KEY,                    -- fare.holiday_surcharge
    capability     TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    params_schema  JSONB NOT NULL DEFAULT '{}',
    operator_count INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'active', 'deprecated'))
);

-- Ví dụ input → output: từ tests/cases.yaml của module (operator NULL) và của nhà xe.
CREATE TABLE logic_tests (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id    TEXT REFERENCES operators(id) ON DELETE CASCADE,
    module_id      TEXT,
    capability     TEXT NOT NULL,
    input          JSONB NOT NULL,
    expected       JSONB,                               -- {out: …} | {error: …}
    note           TEXT,
    source_item_id TEXT,                               -- UUID item nguồn; không FK — case module L1 có thể trỏ item chưa có trong Brain
    commit         TEXT NOT NULL,
    synced_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX logic_tests_lookup ON logic_tests (operator_id, capability);

-- Code chunk theo hàm/lớp gắn commit; commit mới → chunk của commit cũ bị xoá (cơ chế "quên" của code).
CREATE TABLE code_chunks (
    id          BIGSERIAL PRIMARY KEY,
    repo        TEXT NOT NULL,
    commit      TEXT NOT NULL,
    path        TEXT NOT NULL,
    symbol      TEXT NOT NULL,                          -- calculate | TransferQ7.handle
    text        TEXT NOT NULL,
    summary     TEXT NOT NULL DEFAULT '',
    search_text TEXT NOT NULL DEFAULT '',
    tsv         tsvector GENERATED ALWAYS AS (to_tsvector('simple', search_text)) STORED,
    embedding   vector,
    module_id   TEXT,
    operator_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX code_chunks_commit ON code_chunks (repo, commit);
CREATE INDEX code_chunks_tsv ON code_chunks USING gin (tsv);

-- HEAD đã đồng bộ theo repo: job index_code no-op khi commit không đổi.
CREATE TABLE logic_syncs (
    repo      TEXT PRIMARY KEY,
    commit    TEXT NOT NULL,
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
