-- Test bot (M3, E3.5): test case chạy bằng reference executor + kết quả mỗi lần chạy.
-- Sinh từ policy/data (S3.5.2), regression L0/L1, 👎 từ sandbox/UAT (S3.5.3).

CREATE TABLE test_cases (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id    TEXT REFERENCES operators(id) ON DELETE CASCADE,
    layer          SMALLINT NOT NULL DEFAULT 2 CHECK (layer BETWEEN 0 AND 2),
    bot_channel    TEXT NOT NULL DEFAULT 'zalo',
    name           TEXT NOT NULL,
    input          TEXT NOT NULL,               -- câu hỏi/tin nhắn của khách
    expected       JSONB NOT NULL DEFAULT '{}', -- {must_mention[], must_not_say[], must_call_tool[]}
    source_item_id UUID REFERENCES items(id) ON DELETE SET NULL,
    origin         TEXT NOT NULL DEFAULT 'generated'
                   CHECK (origin IN ('generated', 'policy', 'data', 'regression', 'thumbs_down', 'manual')),
    status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX test_cases_lookup ON test_cases (operator_id, status, layer);

CREATE TABLE test_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id  TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    bot_channel  TEXT NOT NULL,
    snapshot_id  UUID,
    total        INTEGER NOT NULL DEFAULT 0,
    passed       INTEGER NOT NULL DEFAULT 0,
    report       JSONB NOT NULL DEFAULT '[]',  -- [{case, input, expected, got, pass, reason}]
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
