# BIVA Brain

Bộ tri thức (về nhà xe và về logic) để AI build chatbot cho nhiều nhà xe khách, dựa trên mô hình memory
của Hindsight (retain / recall / reflect + consolidation), mở cho builder dùng AI qua MCP.

Trạng thái: **thiết kế đã chốt**, đang làm **M0 — Nền móng** (xem [kế hoạch](docs/implementation-plan.md)). Trọng tâm: **Brain + MCP** — AI của builder dùng tri thức để viết bot; Brain là nguồn sự thật và người kiểm tra.

## Tài liệu

| Tài liệu | Nội dung |
|---|---|
| [docs/architecture.md](docs/architecture.md) | bài toán, phạm vi, phân tầng L0–L3, 5 loại tri thức, luồng Brain, "quên", kiến trúc Go + Python, lộ trình, quyết định đã chốt |
| [docs/system-architecture.md](docs/system-architecture.md) | kiến trúc hệ thống: containers, components, luồng chạy, triển khai (không GPU), độ tin cậy, bảo mật, observability, CI/CD |
| [docs/build-flow.md](docs/build-flow.md) | luồng build bot: khởi tạo → thu thập → duyệt → AI viết bot → kiểm tra → xuất & phát hành; vòng cập nhật |
| [docs/mcp.md](docs/mcp.md) | **giao diện chính**: AI dùng tri thức để build bot — knowledge pack, artifact có trích dẫn, validate, tool, prompts |
| [docs/logic-knowledge.md](docs/logic-knowledge.md) | tri thức logic (code) cho nhà xe: module chung, hồ sơ từng nhà xe, config → hook → custom, ADR |
| [docs/data-model.md](docs/data-model.md) | data model trên PostgreSQL |
| [docs/implementation-plan.md](docs/implementation-plan.md) | kế hoạch triển khai: milestone M0–M5, epic, story, AC, rủi ro, chỉ số thành công |

## Cấu trúc repo

```
contracts/   migrations (golang-migrate) · schemas (JSON Schema) · fixtures dùng chung Go ⇄ Python
go/          brain-api (Go): HTTP health, migrate; MCP, recall… ở các mốc sau
python/      ai-worker (Python): job dùng LLM/NLP
deploy/      docker-compose cho local
docs/        thiết kế
```

## Chạy local

Cần Docker (không cần GPU), Go ≥ 1.25, Python ≥ 3.11 và [uv](https://docs.astral.sh/uv/).

```bash
make up        # Postgres(pgvector), Redis, MinIO, TEI (CPU), migrate, brain-api, ai-worker
curl localhost:8080/health/ready
make down      # dừng (giữ dữ liệu); make clean để xoá volume
```

Lần đầu `tei-embed` tải model bge-m3 nên mất vài phút.

## Test

```bash
make test      # go vet + go test, pytest
make lint      # gofmt, go vet, ruff

# chạy thêm test migration với Postgres thật (DB trống, dùng riêng cho test):
BIVA_TEST_DATABASE_URL=postgres://user:pass@localhost:5432/biva_test?sslmode=disable make test-go
```

Fixture trong `contracts/fixtures/` được test ở **cả Go và Python**: hai bên phải cho cùng kết quả.
