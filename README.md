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
kb/         tri thức nền L0/L1 (YAML, review bằng PR) → brain-api kb sync
deploy/      docker-compose cho local
docs/        thiết kế
```

## Chạy local

Cần Docker (không cần GPU), Go ≥ 1.25, Python ≥ 3.11 và [uv](https://docs.astral.sh/uv/).

```bash
make up        # Postgres(pgvector), Redis, S3 (SeaweedFS), TEI (CPU), migrate, brain-api, ai-worker
curl localhost:8080/health/ready
make down      # dừng (giữ dữ liệu); make clean để xoá volume
```

Lần đầu `tei-embed` tải model bge-m3 nên mất vài phút.

## Test

```bash
make test      # go vet + go test, pytest
make lint      # gofmt, go vet, ruff

# chạy thêm test cần Postgres thật (migration, queue, scheduler, runner) — DB riêng cho test:
export BIVA_TEST_DATABASE_URL=postgres://user:pass@localhost:5432/biva_test?sslmode=disable
make test      # test-go chạy migration trước (nên chạy trước test-py)
```

## Kết nối AI qua MCP

```bash
# trong container brain-api (hoặc binary local với BIVA_DATABASE_URL)
brain-api operator add phuongnam "Nhà xe Phương Nam"
brain-api user add tam --email tam@biva.vn --name "Triệu Ngọc Tâm" --role builder
brain-api user grant tam phuongnam
brain-api token issue tam --name "claude-code laptop"     # token chỉ hiện một lần

claude mcp add --transport http biva-phuongnam http://localhost:8080/mcp/operator/phuongnam/ \
  --header "Authorization: Bearer <token>"
```

Với compose: `docker compose -f deploy/docker-compose.yml exec brain-api brain-api token issue tam`.
Tool hiện có (M0): `get_operation`. Danh mục đầy đủ: [docs/mcp.md](docs/mcp.md).

## LLM và index

- `contracts/llm/llm.yaml`: model Gemini theo tier (*nhỏ* 3.5 Flash → 3.1 Flash-Lite, *mạnh* 3.1 Pro → 2.5 Pro),
  quota theo purpose, bảng giá. Thư viện: `python/biva_worker/llm/` (fallback, quota Redis, structured output,
  ghi `llm_usage`). Cần `GEMINI_API_KEY` khi chạy job dùng LLM.
- Job `index.items`: ghi `search_text` (tìm được cả có dấu và không dấu) + embedding qua TEI cho item còn thiếu.

## Queue Go ⇄ Python

brain-api ghi job vào bảng `operations` (`queue.Enqueue`, chống trùng bằng `idempotency_key`); trigger phát
`NOTIFY biva_operations` để đánh thức ai-worker. ai-worker claim bằng `SKIP LOCKED`, giữ lease bằng heartbeat, thử lại
có backoff. Worker chết → lease hết → scheduler (chỉ instance leader, advisory lock) gọi
`operations_requeue_expired()` để trả job về hàng đợi. Cấu hình worker: `BIVA_WORKER_CONCURRENCY` (4),
`BIVA_WORKER_LEASE_SECONDS` (60).

CI (`.github/workflows/ci.yml`) chạy `make lint`, `make test` với Postgres pgvector, kiểm tra `go.mod`/`uv.lock` không lệch, và `make smoke` (compose thật: MCP → queue → worker, TEI CPU).

Fixture trong `contracts/fixtures/` và `contracts/textnorm/` được test ở **cả Go và Python**: hai bên phải cho cùng kết quả.
