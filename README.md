# BIVA Brain

Bộ tri thức (về nhà xe và về logic) để AI build chatbot cho nhiều nhà xe khách, dựa trên mô hình memory
của Hindsight (retain / recall / reflect + consolidation), mở cho builder dùng AI qua MCP.

Trạng thái (10/2026): **toàn bộ story code của lộ trình M0–M5 đã xong, CI xanh** — 46 tool MCP, 12 resource (endpoint nhà xe),
6 prompt; tri thức logic trong repo riêng [biva-integrations](https://github.com/trieungoctam/biva-brain#logic);
vòng phát hành (gate → publish → rollback) và platform cho lead. Các AC đo lường đang chờ: dữ liệu 3 nhà xe
pilot thật, GEMINI key, URL https (xem cuối README). Kế hoạch: [docs/implementation-plan.md](docs/implementation-plan.md).
Brain là **nguồn sự thật** và **người kiểm tra**; AI của builder dùng tri thức để viết bot.

## Tài liệu

| Tài liệu | Nội dung |
|---|---|
| [docs/architecture.md](docs/architecture.md) | bài toán, phạm vi, phân tầng L0–L3, 5 loại tri thức, luồng Brain, "quên", kiến trúc Go + Python, lộ trình, quyết định đã chốt |
| [docs/system-architecture.md](docs/system-architecture.md) | kiến trúc hệ thống: containers, components, luồng chạy, triển khai (không GPU), độ tin cậy, bảo mật, observability, CI/CD |
| [docs/build-flow.md](docs/build-flow.md) | luồng build bot: khởi tạo → thu thập → duyệt → AI viết bot → kiểm tra → xuất & phát hành; vòng cập nhật |
| [docs/mcp.md](docs/mcp.md) | **giao diện chính**: 46 tool · 12 resource (endpoint nhà xe) · 6 prompt — knowledge pack, artifact có trích dẫn, validate, logic, phát hành |
| [docs/logic-knowledge.md](docs/logic-knowledge.md) | tri thức logic (code) cho nhà xe: module chung, hồ sơ từng nhà xe, config → hook → custom, ADR |
| [docs/data-model.md](docs/data-model.md) | data model trên PostgreSQL (23 migration) |
| [docs/runbook.md](docs/runbook.md) | runbook 8 sự cố + backup/khôi phục (`deploy/backup.sh` có verify) |
| [docs/implementation-plan.md](docs/implementation-plan.md) | kế hoạch triển khai: milestone M0–M5, epic, story, AC, rủi ro, chỉ số thành công |

## Cấu trúc repo

```
contracts/   migrations (golang-migrate) · schemas (JSON Schema) · fixtures dùng chung Go ⇄ Python
brain-api/   service Go: MCP (operator + platform), REST, scheduler, migrate, kb sync, recall 5 nhánh,
             export lên object storage, release gate + publish/rollback
ai-worker/   service Python: queue runner — index (TEI) · ingest (Gemini) · consolidate · promote ·
             logic (sync repo, spec, propose PR, chạy ví dụ) · sandbox · reference executor · validate LLM
kb/          tri thức nền L0/L1 (YAML, review bằng PR): rules, template onboarding, entities, feature catalog
deploy/      docker-compose local · backup + verify · demo promote toàn cục · đo SLO
docs/        thiết kế
```

## Chạy local

Cần Docker (không cần GPU), Go ≥ 1.25, Python ≥ 3.11 và [uv](https://docs.astral.sh/uv/).

```bash
make up        # Postgres(pgvector), Redis, S3 (SeaweedFS), TEI (CPU), migrate, brain-api, ai-worker
curl localhost:8080/health/ready
make down      # dừng (giữ dữ liệu); make clean để xoá volume
```

Lần đầu `tei-embed` tải model bge-m3 nên mất vài phút. Máy < 16GB RAM có thể bỏ TEI
(xem `docs/runbook.md` — recall tự chạy keyword + graph + temporal).

## Test

```bash
make test      # go vet + go test, pytest
make lint      # gofmt, go vet, ruff

# chạy thêm test cần Postgres thật (migration, queue, scheduler, runner) — DB riêng cho test:
export BIVA_TEST_DATABASE_URL=postgres://user:pass@localhost:5432/biva_test?sslmode=disable
make test      # test-go chạy migration trước (nên chạy trước test-py)
```

Test LLM thật (CONTRADICTION, executor) đặt thêm `BIVA_TEST_GEMINI_API_KEY`; test S3 thật đặt
`BIVA_TEST_S3_URL`. Không có thì tự skip.

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

ChatGPT (connector): đặt `BIVA_PUBLIC_URL` là URL https công khai rồi thêm connector
`<BIVA_PUBLIC_URL>/mcp/operator/<id>/` (OAuth 2.1, brain-api là authorization server); builder dán token
cá nhân ở trang cấp quyền — xem [docs/mcp.md](docs/mcp.md#2-kết-nối).

### Vòng đời một bot

1. **Tri thức**: AI phía builder đọc tin/file/ảnh rồi `submit_knowledge` (hoặc `ingest` để ai-worker trích
   bằng LLM) → diff → phần vô hại tự áp dụng (item từ form công khai thì LUÔN chờ duyệt);
   giá/giờ/huỷ luôn chờ `apply_review` (preview → `confirm_token`).
   Consolidate gom observation; ≥ 3 nhà xe giống nhau → đề xuất PROMOTE lên L1 cho lead duyệt.
2. **Build**: prompt `/onboard_operator` → `/build_bot` — AI viết artifact có trích dẫn `[[id]]`,
   `validate_artifact` kiểm tĩnh (6 mã lỗi có dòng) + CONTRADICTION bằng LLM chạy nền.
3. **Logic** (repo [biva-integrations](https://github.com/trieungoctam/biva-brain)): `/implement_operator_logic`
   — spec từ tri thức → tìm nhà xe tương tự → `run_examples_against` chạy ví dụ thật trong sandbox →
   profile/hook qua PR (custom bắt buộc ADR).
4. **Phát hành**: `run_tests` (reference executor) → `check_release_gate` → `request_publish`
   (staging tự động; production chờ lead `approve_publish`) → `rollback_release` < 1 phút.

## Logic

Tri thức logic (code) sống ở repo riêng **biva-integrations** (module chuẩn `fare.standard`, `booking.hold`,
`schedule.sync_excel` + hồ sơ từng nhà xe, CI chạy ví dụ mỗi PR). Brain đồng bộ qua job `index_code`
(≤ 1 phút sau merge) và đối chiếu tham số với tri thức — tham số lệch nguồn → hồ sơ stale.

## LLM và index

- `contracts/llm/llm.yaml`: model Gemini theo tier, quota theo purpose, bảng giá. Thư viện
  `ai-worker/biva_worker/llm/` (fallback, quota Redis, structured output, ghi `llm_usage`).
  Cần `GEMINI_API_KEY` khi chạy job dùng LLM.
- Job `index.items`: `search_text` (tìm được cả có dấu và không dấu) + embedding qua TEI +
  nhận diện entity cho graph arm của recall.

## Queue Go ⇄ Python

brain-api ghi job vào bảng `operations` (chống trùng bằng `idempotency_key`); trigger `NOTIFY` đánh thức
ai-worker, claim bằng `SKIP LOCKED`, lease + heartbeat, backoff; scheduler (leader qua advisory lock)
requeue job hết lease. Cấu hình: `BIVA_WORKER_CONCURRENCY` (4), `BIVA_WORKER_LEASE_SECONDS` (60).

CI (`.github/workflows/ci.yml`): `make lint`, `make test` với Postgres pgvector + Redis, kiểm tra
`go.mod`/`uv.lock` không lệch, `make smoke` (compose thật: MCP → queue → worker, TEI CPU).
Fixture trong `contracts/` được test ở **cả Go và Python** — hai bên phải cho cùng kết quả.

## Đo lường và vận hành

```bash
deploy/slo.sh                    # đo p50/p95 tool chính so mục tiêu SLO (docs/runbook.md §7.3)
deploy/backup.sh                 # backup Postgres; BIVA_RESTORE_VERIFY=1 → khôi phục thử + so khớp
deploy/demo.sh                   # demo promote toàn cục (3 nhà xe → L1), không cần GEMINI key
```

Baseline 10/2026 trên stack local: recall p95 14ms (< 150), pack p95 3ms (< 800), validate tĩnh 4ms (< 300),
stale < 2ms — mọi SLO đạt.

## Nạp dữ liệu nhà xe (pilot)

`deploy/pilot-kit/` — 3 template CSV (giá · lịch chạy · chính sách) + loader nạp qua MCP
`submit_knowledge` (không cần key LLM), kèm `--coverage` xem độ phủ mục bắt buộc và thiếu gì.
Chi tiết: `deploy/pilot-kit/README.md`.

## Đang chờ (ngoài code)

| Cần | Mở khóa |
|---|---|
| Chọn 3 nhà xe pilot + dữ liệu thật | demo M5 trên nhà xe thật; đo mọi AC số liệu |
| `GEMINI_API_KEY` | run_tests/publish (executor chạy bot bằng LLM), CONTRADICTION (recall ≥ 90%), reflect, ingest ảnh/tin thô |
| URL https công khai | ChatGPT connector (OAuth đã sẵn) |

Không cần GEMINI key để onboard: pilot kit (`deploy/pilot-kit/`) nạp CSV trực tiếp, và form
thu thập (`create_form` → nhà xe điền link) trích deterministic theo topic — đã kiểm chứng
end-to-end. Artifact/validate/gate đều tĩnh; đường chờ key bắt đầu từ bot test.
