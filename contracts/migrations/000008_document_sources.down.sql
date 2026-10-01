ALTER TABLE documents DROP CONSTRAINT documents_source_check;
ALTER TABLE documents ADD CONSTRAINT documents_source_check CHECK (source IN ('zalo', 'excel', 'form', 'console'));
