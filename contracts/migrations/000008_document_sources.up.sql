-- Kênh nguồn mở rộng (khớp contracts/schemas/jobs/ingest.schema.json): AI phía builder (ChatGPT, coding agent)
-- đọc ảnh, file, ghi chú cuộc gọi… rồi gửi tri thức có cấu trúc (submit_knowledge).
ALTER TABLE documents DROP CONSTRAINT documents_source_check;
ALTER TABLE documents ADD CONSTRAINT documents_source_check
    CHECK (source IN ('zalo', 'excel', 'image', 'call', 'chat', 'form', 'console', 'other'));
