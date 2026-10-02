-- Phát hành (M4, S4.1.2): bảng releases thật thay thiết kế trong data-model.
-- staging: gate pass là published ngay; production: requested → lead duyệt (approved_by) mới published.
-- rollback: giữ nguyên lịch sử — bản production đang published chuyển rolled_back, bản trước đó
-- đặt lại published (xem release.Rollback).

CREATE TABLE releases (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id   TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    bot_channel   TEXT NOT NULL DEFAULT 'zalo',
    snapshot_id   UUID NOT NULL REFERENCES snapshots(id),
    snapshot_ver  INTEGER NOT NULL,
    stage         TEXT NOT NULL CHECK (stage IN ('staging', 'production')),
    status        TEXT NOT NULL DEFAULT 'requested'
                  CHECK (status IN ('requested', 'approved', 'published', 'rolled_back', 'rejected')),
    requested_by  TEXT NOT NULL,
    approved_by   TEXT,
    requested_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,
    rolled_back_at TIMESTAMPTZ
);
CREATE INDEX releases_channel ON releases (operator_id, bot_channel, stage, status);
