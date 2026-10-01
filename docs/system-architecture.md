# System Architecture

Tài liệu này mô tả kiến trúc **hệ thống** của BIVA Brain: thành phần, triển khai, luồng chạy, độ tin cậy,
bảo mật, quan sát và CI/CD. Thiết kế nghiệp vụ (tầng tri thức, luồng Brain, luồng build) ở
[architecture.md](architecture.md) và [build-flow.md](build-flow.md).

## 1. Context — hệ thống và thế giới bên ngoài

```
                    ┌──────────────────────┐
   Hành khách ─────►│  Kênh chat           │  Zalo OA · Messenger · Web widget · Hotline (voice)
                    └──────────┬───────────┘
                               │ webhook / websocket
                               ▼
 Nhà xe ──update──►  ╔═══════════════════════╗  ──tool call──►  Hệ thống đặt vé của nhà xe (nếu có)
 (Zalo/Excel/form)   ║      BIVA PLATFORM    ║  ──LLM API────►  LLM providers (chính + dự phòng)
                     ║                       ║  ──notify─────►  Nhóm Zalo / SĐT nhà xe (handoff, câu hỏi)
 Builder ──MCP────►  ║                       ║
 (Claude Code/       ║                       ║
  Desktop/agent)     ║                       ║
 Lead/Ops ─web────►  ╚═══════════════════════╝
 (Console duyệt)
```

| Tác nhân | Tương tác | Kênh |
|---|---|---|
| Hành khách | chat với bot | Zalo, Messenger, web, hotline |
| Nhà xe | gửi cập nhật, trả lời câu hỏi, UAT, nhận handoff | Zalo, form, file |
| Builder | build và vận hành bot bằng AI | MCP |
| Lead / Ops | duyệt thay đổi L0/L1, deploy, giám sát | Console web |

## 2. Containers

```
                        ┌──────────────────────── Edge ────────────────────────┐
 Kênh chat ────────────►│ Ingress (TLS, WAF, rate limit theo IP/kênh)          │
 Builder (MCP) ────────►│ /webhooks/*  → bot-runtime                           │
 Console ──────────────►│ /api/* /mcp/* → brain-api     /  → console (static)  │
                        └──────────────┬──────────────────────────┬────────────┘
                                       │                          │
          ┌────────────────────────────▼───┐      ┌───────────────▼────────────────────┐
          │ bot-runtime (Go)               │      │ brain-api (Go)                      │
          │ channels · conversation ·      │      │ REST · MCP · authz · recall ·       │
          │ snapshot cache · LLM stream ·  │      │ review · enqueue · scheduler(leader)│
          │ tools · recall (in-process)    │      └───────┬───────────────┬────────────┘
          └──┬────────┬────────┬───────────┘              │               │
             │        │        │                          │               │
             │   ┌────▼────┐   │      ┌───────────────────▼───┐   ┌───────▼────────┐
             │   │ Redis   │◄──┼──────┤ PostgreSQL 16         │   │ Object storage │
             │   │ session │   │      │ primary + read replica│   │ (S3/MinIO)     │
             │   │ ratelim │   │      │ pgvector·trgm·unaccent│   │ file gốc,      │
             │   │ cache   │   │      │ queue · NOTIFY        │   │ snapshot, export│
             │   └─────────┘   │      └───────────▲───────────┘   └───────▲────────┘
             │                 │                  │ claim / write         │
             │                 │      ┌───────────┴───────────────────────┴───────────┐
             │                 │      │ ai-worker (Python) ×N                          │
             │                 │      │ ingest · consolidate · promote · compile ·     │
             │                 │      │ testgen · eval · learn · refresh_pages         │
             │                 │      └───────┬───────────────────────────┬───────────┘
             ▼                 ▼              ▼                           ▼
       ┌──────────────────────────────┐  ┌─────────────────────────────────────────┐
       │ TEI (Rust) embed · rerank    │  │ LLM providers (qua LLM client chung:      │
       │ GPU node pool                │  │ quota, fallback, cost metering)           │
       └──────────────────────────────┘  └─────────────────────────────────────────┘

 Console (SPA tĩnh) ── gọi /api của brain-api
 Observability: OTel Collector → traces / metrics / logs backend
```

| Container | Ngôn ngữ | Stateless | Scale theo | Ghi chú |
|---|---|---|---|---|
| `bot-runtime` | Go | ✅ (session ở Redis) | hội thoại đồng thời | hot path, không phụ thuộc Python |
| `brain-api` | Go | ✅ | phiên MCP, request console | scheduler chạy trên **1 instance leader** (pg advisory lock) |
| `ai-worker` | Python | ✅ | độ dài queue | concurrency tách theo loại job |
| `console` | TS (SPA) | ✅ | – | chủ yếu để **duyệt** và xem |
| `tei-embed`, `tei-rerank` | Rust | ✅ | QPS | GPU; có thể CPU ở môi trường nhỏ |
| PostgreSQL | – | ❌ | dữ liệu, QPS đọc | primary (ghi + queue) + replica (recall, đọc console) |
| Redis | – | ❌ (dữ liệu tạm) | – | session, rate limit, cache recall/embedding, quota LLM |
| Object storage | – | ❌ | – | file gốc, artifact snapshot, export |

## 3. Components bên trong

### 3.1 bot-runtime (Go)

```
channels/        adapter từng kênh: verify chữ ký webhook, chuẩn hoá message, gửi trả (stream/nút)
conversation/    state hội thoại tạm (Redis, TTL), idempotency theo message_id, khoá theo conversation
snapshot/        tải Bot Definition, validate schema, atomic swap; nghe NOTIFY snapshot_ready
orchestrator/    vòng lặp lượt chat: prompt từ snapshot → LLM → tool calls song song → LLM → stream
tools/           query_data (Postgres replica), check_seats (API nhà xe, circuit breaker), handoff
recall/          package dùng chung với brain-api: 4 arm, RRF, rerank, boost, pack
llm/             client chung: provider chính + dự phòng, timeout, quota (Redis token bucket)
telemetry/       log hội thoại ẩn danh (buffer → batch insert), metrics, traces
```

### 3.2 brain-api (Go)

```
httpapi/         REST cho console: operators, review, snapshots, deployments, feedback
mcp/             2 endpoint (/mcp/operator/{id}/, /mcp/platform/), tools/resources/prompts, confirm_token
authz/           xác thực (OIDC cho người, token cho MCP), role builder|lead|ops, scope theo operator
services/        nghiệp vụ dùng chung cho REST và MCP (một lõi, hai giao diện)
recall/          (dùng chung)
queue/           enqueue job (idempotency_key), theo dõi operation
scheduler/       leader-only: expire (valid_to), requeue job hết lease, dọn chat_logs TTL, cron refresh
store/           sqlc generated, pgxpool (primary + replica)
```

### 3.3 ai-worker (Python)

```
runner/          claim job (SKIP LOCKED, lease, heartbeat), LISTEN để thức dậy, retry + backoff
jobs/            ingest · consolidate · promote · compile · testgen · eval · learn · refresh_pages
prompts/         toàn bộ prompt (có version, có test)
nlp/             chuẩn hoá tiếng Việt, tách từ, entity resolution, parse Excel/ảnh
llm/             client chung (cùng quy ước quota/fallback với Go), structured output
index/           ghi search_text (không dấu + bigram), gọi TEI embed theo batch
```

## 4. Luồng chạy chính

### 4.1 Một lượt chat của hành khách (hot path)

```
Kênh ──webhook──► Ingress ──► bot-runtime
  1. verify chữ ký, dedupe message_id (Redis SETNX)                         ~2ms
  2. khoá conversation (tránh 2 tin đến cùng lúc xử lý song song)
  3. snapshot từ RAM                                                         ~0ms
  4. LLM #1 (stream) ──► quyết định tool
  5. tools song song: query_data(replica) ║ recall(in-proc → TEI, replica) ║ check_seats(API nhà xe)
  6. LLM #2 (stream) ──► trả token dần về kênh
  7. ghi chat_log ẩn danh (async batch), metrics, trace
```
Không có bước nào gọi ai-worker. Ngân sách: recall p95 < 120 ms, time-to-first-token < 1.2 s.

### 4.2 Nhà xe gửi update → bot mới lên production

```
Builder (MCP) ──ingest──► brain-api ──INSERT operations + NOTIFY──► ai-worker: ingest
                                                                     → items(pending) + review_items
Builder (MCP) ──apply_review (preview → confirm_token)──► brain-api: apply trong 1 transaction
                                                          → enqueue consolidate → promote → compile
ai-worker: compile → snapshot(draft) → testgen + eval → snapshot(passed)
brain-api: check_release_gate → request_deploy
  staging: tự động │ canary/prod: lead duyệt trên console
brain-api: deployments.live + NOTIFY snapshot_ready ──► mọi bot-runtime: tải + atomic swap (< 1s)
```

### 4.3 Builder làm việc qua MCP

```
AI client ──streamable HTTP──► Ingress ──► brain-api /mcp/operator/{id}/
   authz: token → user, role; scope operator cố định theo URL
   tool đọc   → services → replica
   tool ghi   → services → primary + audit_log(actor=ai:<session>, approved_by=<user>)
   tool nặng  → enqueue → trả operation_id → AI poll get_operation
```

### 4.4 Rollback

```
Lead/Builder ──rollback(bot, snapshot)──► brain-api: deployments.rolled_back, bots.active_snapshot_id
              ──NOTIFY snapshot_ready──► bot-runtime swap. Không cần build hay deploy lại code.
```

**Hai đường deploy tách biệt**: *deploy code* (container image qua CI/CD) và *deploy bot*
(snapshot qua Brain). Thay đổi tri thức của nhà xe không bao giờ cần build lại code.

## 5. Lưu trữ dữ liệu

| Dữ liệu | Nơi lưu | Thời gian giữ |
|---|---|---|
| Tri thức (items, observations, entities, links) | Postgres | lâu dài; "quên" bằng status |
| Data vận hành (tuyến, chuyến, giá, điểm đón) | Postgres | lâu dài, có version |
| Queue `operations` | Postgres | job xong: 30 ngày rồi dọn |
| Snapshot (definition) | Postgres + bản sao artifact ở object storage | giữ N bản gần nhất / bot + mọi bản từng `active` |
| File gốc nhà xe gửi | Object storage (đường dẫn trong `documents.metadata`) | theo hợp đồng nhà xe |
| Session hội thoại | Redis | TTL 30 phút không hoạt động |
| Chat logs ẩn danh | Postgres | 30 ngày (`expires_at`) |
| Cache recall / embedding | Redis | theo `snapshot_id`; tự vô hiệu khi đổi snapshot |
| Audit log | Postgres (partition theo tháng) | ≥ 1 năm |

Backup: Postgres PITR (WAL archive) + snapshot hằng ngày; object storage bật versioning.

## 6. Triển khai

### 6.1 Môi trường

| Môi trường | Mục đích | Ghi chú |
|---|---|---|
| `local` | dev | docker-compose: Postgres, Redis, MinIO, TEI (CPU), 3 service |
| `staging` | test tích hợp, sandbox chat, UAT nhà xe | dữ liệu nhà xe thật, kênh test |
| `production` | bot thật | |

Snapshot có `stage` riêng (staging · canary · production) **bên trong** môi trường production —
sandbox/UAT của nhà xe chạy trên hạ tầng production nhưng bằng snapshot draft, kênh test.

### 6.2 Topology production (Kubernetes)

```
namespace biva
├── deploy/bot-runtime     HPA theo CPU + số kết nối      min 3   (trải trên ≥ 2 zone)
├── deploy/brain-api       HPA theo CPU                   min 2   (1 leader cho scheduler)
├── deploy/ai-worker       KEDA theo COUNT(operations queued) min 1, max 20
├── deploy/console         static                         2
├── deploy/tei-embed       node pool GPU                  1–2
├── deploy/tei-rerank      node pool GPU                  1–2
└── otel-collector         daemonset
Managed: PostgreSQL (primary + 1 replica, HA), Redis (HA), Object storage
```

Ước lượng ban đầu (≤ 50 nhà xe, ≤ 2k hội thoại đồng thời): 3 bot-runtime × (1 vCPU, 512 MB),
2 brain-api × (1 vCPU, 512 MB), 2 ai-worker × (1 vCPU, 1 GB), 1 GPU nhỏ dùng chung cho TEI,
Postgres 4 vCPU / 16 GB. Điều chỉnh sau khi đo tải thật.

## 7. Độ tin cậy

### 7.1 Khi một thành phần hỏng

| Sự cố | Ảnh hưởng | Cách xử lý |
|---|---|---|
| ai-worker chết / chậm | build, ingest chậm | **bot vẫn chạy** bằng snapshot hiện tại; job requeue khi hết lease |
| TEI chết | recall mất semantic + rerank | recall chạy keyword + graph + temporal, điểm RRF thay rerank |
| LLM provider chính lỗi / quá tải | bot không trả lời | chuyển provider dự phòng; quá ngưỡng → handoff kèm câu xin lỗi |
| API đặt vé nhà xe lỗi | không tra được ghế | circuit breaker; bot nói "nhân viên sẽ xác nhận", tạo handoff |
| Postgres replica chậm/lỗi | recall/data chậm | đọc chuyển về primary (có giới hạn); cảnh báo |
| Postgres primary lỗi | không ghi được | failover managed; bot vẫn trả lời (snapshot trong RAM + replica) |
| Redis lỗi | mất session tạm, cache | session fallback in-memory theo pod (sticky theo conversation); cache bỏ qua |
| Snapshot lỗi lọt qua test | bot trả lời sai | canary + auto rollback theo tỉ lệ 👎/handoff |
| Webhook kênh gửi lặp | trả lời 2 lần | dedupe theo `message_id` |

### 7.2 Nguyên tắc

- **Hot path không phụ thuộc vào background**: Go không gọi đồng bộ sang Python.
- **Mọi job idempotent** (`idempotency_key`), có `max_attempts`, backoff mũ, dead-letter = `failed` + cảnh báo.
- **Timeout ở mọi lời gọi ra ngoài**, kèm context deadline của lượt chat.
- **Degrade có kiểm soát**: mất một arm của recall không làm hỏng lượt chat.

### 7.3 SLO đề xuất

| SLO | Mục tiêu |
|---|---|
| Bot trả lời thành công (không lỗi hệ thống) | 99.5% / 30 ngày |
| Time-to-first-token p95 | < 1.5 s |
| Update rủi ro thấp → có trên bot | p95 < 5 phút |
| Rollback snapshot | < 1 phút |

## 8. Bảo mật & multi-tenant

| Mặt | Thiết kế |
|---|---|
| Xác thực người | OIDC (Google Workspace / SSO) cho console; token cá nhân cho MCP, có hạn, thu hồi được |
| Phân quyền | role `builder` · `lead` · `ops`; builder được gán danh sách nhà xe; MCP ẩn tool vượt quyền |
| Cách ly nhà xe | scope `operator_id` cố định theo URL MCP; service layer luôn lọc theo scope; bật **Postgres RLS** trên các bảng có `operator_id` như lớp bảo vệ thứ hai |
| Webhook kênh | verify chữ ký từng kênh; secret riêng theo bot |
| Secret | secret manager (API key LLM, token kênh, API nhà xe); không nằm trong snapshot |
| Dữ liệu cá nhân | không lưu memory khách; chat log ẩn danh (bỏ SĐT, tên) trước khi ghi; TTL 30 ngày |
| Prompt injection | nội dung nhà xe/khách luôn là *data*: bọc trong vùng dữ liệu của prompt, không cho phép tool ghi từ nội dung đó; MCP yêu cầu confirm cho thao tác ghi quan trọng |
| Audit | mọi thao tác ghi: actor (người / `ai:<session>` / job), người duyệt, diff |
| Thay đổi rủi ro cao | L0/L1, deploy canary/prod: quy tắc 2 người (người yêu cầu ≠ người duyệt) |

## 9. Observability

- **OpenTelemetry** cho Go và Python. Trace context đi qua `operations.trace_context`, nên một trace nối được
  MCP call (Go) → job (Python) → compile → NOTIFY → hot-reload (Go).
- **Mọi câu trả lời của bot** gắn `snapshot_id`, các item/observation được dùng, tool đã gọi → `trace_answer`.

| Nhóm metric | Ví dụ |
|---|---|
| Hot path | TTFT, thời gian từng phần (LLM / recall / tool), tỉ lệ cache hit, lỗi theo kênh |
| Chất lượng bot | tỉ lệ 👎, handoff, câu hỏi lặp, knowledge gap mới theo nhà xe |
| Build | độ dài queue theo `kind`, thời gian job, tỉ lệ fail, tỉ lệ test pass mỗi compile |
| Chi phí | token LLM theo nhà xe / theo loại job / theo bot |
| Hạ tầng | Postgres (kết nối, replica lag), TEI QPS/latency, Redis |

Cảnh báo chính: TTFT p95 vượt SLO, tỉ lệ 👎/handoff tăng đột biến sau deploy, queue tồn đọng, job `failed`,
replica lag, chi phí LLM vượt ngân sách ngày.

## 10. CI/CD

```
PR ──► lint + unit (Go, Python) ──► contract tests (fixture chung Go ⇄ Python)
   ──► migration check (apply lên DB trống + DB bản sao staging)
   ──► prompt tests (bộ case cố định cho từng prompt)
   ──► build images
main ─► deploy staging tự động ──► smoke + regression trên bot mẫu ──► duyệt ──► production (rolling)
```

- Migration chạy **trước** khi rollout code, luôn tương thích ngược một phiên bản (expand → migrate → contract).
- Thay đổi prompt ở worker là thay đổi code: qua PR, prompt tests, và chạy regression của các bot mẫu.
- Deploy bot (snapshot) **không** đi qua CI/CD code — đi qua release gate của Brain (xem 4.2).

## 11. Tech stack

| Lớp | Lựa chọn |
|---|---|
| Hot path, API, MCP | Go 1.24+, `pgx`/`sqlc`, `modelcontextprotocol/go-sdk`, `errgroup` |
| Worker | Python 3.12, asyncio + uvloop, `asyncpg`, Pydantic, underthesea |
| Embedding / rerank | HF TEI: bge-m3, bge-reranker-v2-m3 |
| Database | PostgreSQL 16 + pgvector, pg_trgm, unaccent |
| Cache / session | Redis |
| Object storage | S3-compatible (MinIO ở local) |
| Console | SPA TypeScript (framework chốt sau) |
| Hạ tầng | Kubernetes, KEDA, Helm; docker-compose cho local |
| Quan sát | OpenTelemetry → backend traces/metrics/logs (chốt sau) |

## 12. Điểm cần chốt (bổ sung cho architecture.md §9)

| # | Câu hỏi | Đề xuất |
|---|---|---|
| S1 | Cloud / nơi chạy (cloud VN hay quốc tế, có GPU không) | ảnh hưởng TEI và độ trễ tới LLM provider |
| S2 | LLM client: thư viện trong từng service hay một LLM gateway riêng | v1 dùng thư viện + quota chung ở Redis; gateway riêng khi nhiều provider/tenant |
| S3 | Hotline (voice) có trong phạm vi v1 không | nếu có: thêm STT/TTS, ngân sách latency khác |
| S4 | Backend observability | chọn theo hạ tầng sẵn có của team |
| S5 | Framework console | chọn theo năng lực team frontend |
