# Fixture cho JSON Schema

Mỗi thư mục con ứng với một schema (`jobs.ingest` → `schemas/jobs/ingest.schema.json`).
File `valid_*.json` phải hợp lệ, `invalid_*.json` phải bị từ chối. Test của **cả Go và Python** chạy trên cùng các file này.
