-- Snapshot của bot (M2, S2.4.1): Bot Definition lắp từ bản mới nhất của các artifact, chỉ khi chúng `valid`.
-- Đầu vào cho export_bot (và releases ở M4). Cùng bộ version artifact → dùng lại snapshot cũ.
CREATE TABLE snapshots (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id             TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    operator_id        TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    version            INTEGER NOT NULL CHECK (version >= 1),
    artifact_versions  JSONB NOT NULL,              -- {kind: version}
    knowledge_version  TEXT,
    definition         JSONB NOT NULL,
    status             TEXT NOT NULL DEFAULT 'assembled'
                       CHECK (status IN ('assembled', 'testing', 'passed', 'failed', 'published', 'retired')),
    created_by         TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (bot_id, version)
);
ALTER TABLE bots ADD CONSTRAINT bots_current_snapshot FOREIGN KEY (current_snapshot_id)
    REFERENCES snapshots(id) ON DELETE SET NULL;
