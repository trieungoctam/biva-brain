-- Operator Profile pages dựng sẵn (M2, S2.1.4 refresh_pages): trang Tổng quan / Chính sách / Khác thông lệ /
-- Còn thiếu / Logic cho mỗi nhà xe, lắp từ knowledge pack. Job refresh_pages (scheduler leader) dựng lại khi
-- version tri thức của scope đổi (trigger 000010); resource MCP đọc từ đây, thiếu thì dựng tại chỗ.
CREATE TABLE operator_pages (
    operator_id       TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    slug              TEXT NOT NULL CHECK (slug IN ('tong-quan', 'chinh-sach', 'khac-thong-le', 'con-thieu', 'logic')),
    title             TEXT NOT NULL,
    markdown          TEXT NOT NULL,
    knowledge_version TEXT NOT NULL,               -- "<L0/L1>.<nhà xe>" lúc dựng (bảng knowledge_versions)
    refreshed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (operator_id, slug)
);
