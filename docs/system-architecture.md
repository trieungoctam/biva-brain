# System Architecture

Tài liệu này mô tả kiến trúc **hệ thống** của BIVA Brain: thành phần, triển khai, luồng chạy, độ tin cậy,
bảo mật, quan sát và CI/CD. Thiết kế nghiệp vụ (tầng tri thức, luồng Brain, luồng build) ở
[architecture.md](architecture.md) và [build-flow.md](build-flow.md).

**Phạm vi**: **Brain + MCP** — AI dùng tri thức (nhà xe và logic) để build bot; Brain là nguồn sự thật và người kiểm tra.
Brain **không chạy bot**. Các runtime chạy bot hiện có (chưa thống nhất) nằm ngoài phạm vi; **Runtime Integration API (§3.5)
là thiết kế cho giai đoạn sau**, chưa nằm trong các mốc M0–M5.

## 1. Context — hệ thống và thế giới bên ngoài

```
                                   ╔════════════════════════╗
 Nhà xe ──update (Zalo/Excel)───►  ║                        ║ ──LLM API──►  LLM providers (chính + dự phòng)
 Builder ──MCP (AI client)──────►  ║       BIVA BRAIN       ║
 Lead/Ops ──Console web─────────►  ║                        ║ ──notify───►  Nhà xe (câu hỏi bổ sung)
                                   ╚═══════════╤════════════╝
                                               │ Runtime Integration API
                                               │ (snapshot · recall · data · feedback)
                                   ┌───────────▼────────────┐
                                   │ Runtime chạy bot        │  hiện có / sau này — NGOÀI PHẠM VI
                                   │ (Zalo, Messenger, web)  │◄──── Hành khách
                                   └─────────────────────────┘
```

| Tác nhân | Tương tác với Brain | Kênh |
|---|---|---|
| Nhà xe | gửi cập nhật, trả lời câu hỏi, UAT | Zalo, file Excel (v1); form (M2) |
| Builder | build bot bằng AI | MCP |
| Lead / Ops | duyệt L0/L1, phát hành production, giám sát | Console web |
| Runtime chạy bot | lấy snapshot, gọi recall/data, gửi feedback | Runtime Integration API |

## 2. Containers

```
                     ┌────────────────────────── Edge (Ingress) ───────────────────────────┐
 Builder (MCP) ─────►│ TLS · WAF · rate limit                                              │
 Console ───────────►│ /mcp/* /api/* /runtime/* → brain-api        / → console (static)    │
 Runtime (sau này) ─►│                                                                     │
                     └───────────────────────────────┬─────────────────────────────────────┘
                                                     │
                          ┌──────────────────────────▼───────────────────────────┐
                          │ brain-api (Go)                                        │
                          │ REST · MCP · Runtime API · authz · recall · review ·  │
                          │ enqueue · webhook dispatcher · scheduler (leader)     │
                          └───┬──────────────┬───────────────┬───────────────────┘
                              │              │               │
                     ┌────────▼──────┐ ┌─────▼──────────────────────┐ ┌──────────────────┐
                     │ Redis         │ │ PostgreSQL 16              │ │ Object storage   │
                     │ cache recall/ │ │ primary + read replica     │ │ file gốc nhà xe, │
                     │ embedding,    │ │ pgvector·pg_trgm·unaccent  │ │ artifact snapshot│
                     │ quota LLM,    │ │ queue · NOTIFY             │ └────────▲─────────┘
                     │ rate limit    │ └─────▲──────────────────────┘          │
                     └───────▲───────┘       │ claim / write                   │
                             │      ┌────────┴────────────────────────────────┴──────┐
                             └──────┤ ai-worker (Python) ×N                           │
                                    │ ingest · consolidate · promote · compile ·      │
                                    │ testgen · eval · learn · refresh_pages ·        │
                                    │ reference executor (test/sandbox)               │
                                    └───────┬───────────────────────────┬────────────┘
                                            ▼                           ▼
                              ┌──────────────────────────┐  ┌──────────────────────────────┐
                              │ TEI (Rust, CPU)           │  │ LLM providers (thư viện llm/: │
                              │ embed bge-m3 · rerank     │  │ quota Redis, fallback, cost)  │
                              └──────────────────────────┘  └──────────────────────────────┘

 Console (SPA tĩnh) ── gọi /api của brain-api
 Observability: OTel Collector → Grafana stack (Prometheus · Tempo · Loki)
```

| Container | Ngôn ngữ | Stateless | Scale theo | Ghi chú |
|---|---|---|---|---|
| `brain-api` | Go | ✅ | phiên MCP, request console, Runtime API | scheduler chạy trên **1 instance leader** (pg advisory lock) |
| `ai-worker` | Python | ✅ | độ dài queue | concurrency tách theo loại job |
| `console` | TypeScript (React + Vite) | ✅ | – | chủ yếu để **duyệt** và xem |
| `tei-embed`, `tei-rerank` | Rust | ✅ | QPS | **chạy CPU** (không có GPU) |
| PostgreSQL | – | ❌ | dữ liệu, QPS đọc | primary (ghi + queue) + replica (đọc) |
| Redis | – | ❌ (dữ liệu tạm) | – | cache recall/embedding, quota LLM, rate limit |
| Object storage | – | ❌ | – | file gốc, artifact snapshot, export |

## 3. Components bên trong

### 3.1 brain-api (Go)

```
httpapi/         REST cho console: operators, review, snapshots, releases, feedback
mcp/             /mcp/operator/{id}/, /mcp/platform/ — tools, resources, prompts, confirm_token
artifacts/       lưu artifact, validate tĩnh (trích dẫn, locked, hardcoded data, coverage), stale, export
logic/           danh mục module, hồ sơ logic, ADR, impact_of_change
runtimeapi/      (giai đoạn sau) Runtime Integration API (§3.5)
webhooks/        phát snapshot.published tới runtime đã đăng ký (HMAC, retry, backoff)
authz/           OIDC cho người, token cho MCP, API key cho runtime; role builder|lead|ops; scope operator
services/        nghiệp vụ dùng chung cho REST, MCP và Runtime API (một lõi, nhiều giao diện)
recall/          4 arm, RRF, rerank (giới hạn), boost, pack; cache theo (snapshot_id, query)
queue/           enqueue job (idempotency_key), theo dõi operation
scheduler/       leader-only: expire (valid_to), requeue job hết lease, dọn TTL, cron refresh
store/           sqlc generated, pgxpool (primary + replica)
textnorm/        chuẩn hoá tiếng Việt cho query (cùng thuật toán với worker, kiểm bằng fixture chung)
```

### 3.2 ai-worker (Python)

```
runner/          claim job (SKIP LOCKED, lease, heartbeat), LISTEN để thức dậy, retry + backoff
jobs/            ingest · consolidate · promote · validate (LLM checks) · testgen · eval · learn ·
                 refresh_pages · index_code (code chunk theo commit) · logic_promote
executor/        reference executor: chạy một snapshot như runtime thật (LLM + gọi Runtime API)
                 — chỉ dùng cho test, eval, sandbox; không phục vụ khách
prompts/         toàn bộ prompt (có version, có test)
nlp/             chuẩn hoá tiếng Việt, tách từ, entity resolution, parse Excel
llm/             thư viện LLM (§3.4), structured output
index/           ghi search_text (không dấu + bigram), gọi TEI embed theo batch
```

### 3.3 console (React + Vite)

Review queue, duyệt L0/L1, duyệt phát hành production, Operator Profile pages, báo cáo test, giám sát job.
Không chứa nghiệp vụ — chỉ gọi REST của brain-api.

### 3.4 Thư viện LLM (không có gateway)

LLM được gọi trực tiếp từ service qua thư viện `llm/` — một bản Go (brain-api, khi cần) và một bản Python
(ai-worker), **cùng một quy ước**:

| Quy ước | Cách làm |
|---|---|
| Cấu hình provider | một file `llm.yaml` dùng chung: provider, model theo hạng (*nhỏ* / *mạnh*), thứ tự fallback, timeout |
| Quota | token bucket trong Redis theo `(provider, purpose)`; purpose = `build` · `eval` · `interactive` (MCP reflect) |
| Fallback | lỗi / timeout / 429 → provider kế tiếp; hết danh sách → lỗi rõ ràng, job retry theo backoff |
| Đo chi phí | mỗi lời gọi ghi metrics `tokens_in/out`, `cost`, nhãn `operator_id`, `purpose`, `model` |
| Kiểm thử | test hợp đồng chung: cùng cấu hình → cùng lựa chọn provider ở Go và Python |

Không dùng gateway riêng ở v1: ít thành phần vận hành hơn. Cân nhắc khi số provider tăng hoặc cần quản lý key tập trung.

### 3.5 Runtime Integration API (giai đoạn sau)

> Chưa nằm trong M0–M5. Hiện bot được **xuất** bằng `export_bot` (json · markdown · faq_csv); phần dưới là thiết kế
> để nối runtime khi cần.

Hợp đồng duy nhất giữa Brain và mọi runtime chạy bot. Định nghĩa bằng JSON Schema trong `contracts/schemas/`.
Xác thực bằng API key theo runtime, mỗi key chỉ được truy cập các bot được gán.

| Endpoint | Mục đích | Ngân sách |
|---|---|---|
| `GET /runtime/v1/bots/{bot_id}/snapshot/active` | Bot Definition đang phát hành ở stage tương ứng; `ETag` → `304` nếu không đổi | p95 < 20 ms |
| `POST /runtime/v1/bots/{bot_id}/recall` | `{query, valid_at, max_tokens}` → item có nhãn tầng + nguồn | p95 < 120 ms |
| `POST /runtime/v1/bots/{bot_id}/data/{kind}` | `kind` = routes · trips · fares · pickup_points; lọc hiệu lực theo ngày đi | p95 < 15 ms |
| `POST /runtime/v1/feedback` | 👎, handoff, sửa của nhân viên, câu không trả lời được (→ knowledge gap) | async |
| `POST /runtime/v1/transcripts` | log hội thoại **đã ẩn danh** (tuỳ chọn) để `learn`; TTL 30 ngày | async |
| Webhook `snapshot.published` | Brain → runtime khi có snapshot mới; ký HMAC, retry với backoff | < 5 s sau khi phát hành |

Quy ước: mỗi câu trả lời runtime gửi về (feedback/transcript) nên kèm `snapshot_id` và id các item đã dùng,
để `trace_answer` truy vết được.

## 4. Luồng chạy chính

### 4.1 Nhà xe gửi update → snapshot mới được phát hành

```
Builder (MCP) ──ingest──► brain-api ──INSERT operations + NOTIFY──► ai-worker: ingest
                                                                     → items(pending) + review_items
Builder (MCP) ──apply_review (preview → confirm_token)──► brain-api: apply trong 1 transaction
                                                          → enqueue consolidate → promote → compile
ai-worker: compile → snapshot(draft) → testgen + eval (reference executor) → snapshot(passed)
brain-api: check_release_gate
  staging: tự phát hành │ production: request → lead duyệt trên console
brain-api: releases(published) + NOTIFY → webhook snapshot.published → runtime đã đăng ký
```

### 4.2 Builder làm việc qua MCP

```
AI client ──streamable HTTP──► Ingress ──► brain-api /mcp/operator/{id}/
   authz: token → user, role; scope operator cố định theo URL
   tool đọc   → services → replica
   tool ghi   → services → primary + audit_log(actor=ai:<session>, approved_by=<user>)
   tool nặng  → enqueue → trả operation_id → AI poll get_operation
```

### 4.3 Runtime phục vụ một lượt chat (khi đã nối, ngoài phạm vi runtime)

```
Runtime ──GET snapshot/active (ETag, cache cục bộ)──► brain-api        (chỉ khi webhook báo / định kỳ)
Runtime ──POST recall {query, valid_at}──► brain-api: textnorm → 4 arm song song (replica, TEI)
                                                    → RRF → rerank top 30–50 (≤ 80 ms) → boost → pack
Runtime ──POST data/fares {route, date}──► brain-api: replica, prepared statement
Runtime ──POST feedback / transcripts (async)──► brain-api → enqueue learn
```

### 4.4 Rollback

```
Lead/Builder ──rollback(bot, snapshot)──► brain-api: phát hành lại snapshot trước
              ──webhook snapshot.published──► runtime. Không cần build lại code.
```

**Hai đường thay đổi tách biệt**: *deploy code* của Brain (container image qua CI/CD) và *phát hành bot*
(snapshot qua release gate). Thay đổi tri thức của nhà xe không bao giờ cần build lại code.

## 5. Lưu trữ dữ liệu

| Dữ liệu | Nơi lưu | Thời gian giữ |
|---|---|---|
| Tri thức (items, observations, entities, links) | Postgres | lâu dài; "quên" bằng status |
| Data vận hành (tuyến, chuyến, giá, điểm đón) | Postgres | lâu dài, có version |
| Queue `operations` | Postgres | job xong: 30 ngày rồi dọn |
| Snapshot (definition) | Postgres + artifact ở object storage | N bản gần nhất / bot + mọi bản từng được phát hành |
| File gốc nhà xe gửi | Object storage | theo hợp đồng nhà xe |
| Transcript ẩn danh từ runtime | Postgres | 30 ngày (`expires_at`) |
| Cache recall / embedding | Redis | theo `snapshot_id`; tự vô hiệu khi đổi snapshot |
| Audit log | Postgres (partition theo tháng) | ≥ 1 năm |

Backup: Postgres PITR (WAL archive) + snapshot hằng ngày; object storage bật versioning.

## 6. Triển khai

### 6.1 Môi trường

| Môi trường | Mục đích |
|---|---|
| `local` | dev — docker-compose: Postgres, Redis, MinIO, TEI (CPU), brain-api, ai-worker |
| `staging` | test tích hợp, kiểm thử Runtime API với runtime giả lập |
| `production` | Brain thật; snapshot có stage riêng (staging · production) bên trong |

### 6.2 Chạy không có GPU

| Việc | Cách làm trên CPU |
|---|---|
| Embed query (Runtime API recall) | TEI CPU, câu ngắn ~10–30 ms; **cache embedding theo query đã chuẩn hoá** |
| Embed item (nền) | ai-worker gọi TEI theo batch; không ảnh hưởng đường đọc |
| Rerank trên đường đọc | chỉ **top 30–50** sau RRF, ngân sách **80 ms**; quá hạn → dùng điểm RRF + boost |
| Rerank sâu | top 300 chỉ ở đường nền: reflect, compile, testgen |
| Cache recall | theo `(snapshot_id, query chuẩn hoá)` — câu hỏi của khách nhà xe lặp lại nhiều |

Phương án thay thế nếu CPU không đủ: gọi embedding/rerank qua API của provider bằng thư viện `llm/`.

### 6.3 Topology production (Kubernetes)

```
namespace biva-brain
├── deploy/brain-api       HPA theo CPU                       min 2   (1 leader cho scheduler)
├── deploy/ai-worker       KEDA theo COUNT(operations queued) min 1, max 20
├── deploy/console         static                             2
├── deploy/tei-embed       CPU (4 vCPU, 4 GB)                 2
├── deploy/tei-rerank      CPU (4 vCPU, 4 GB)                 2
└── otel-collector         daemonset
Managed: PostgreSQL (primary + 1 replica, HA), Redis (HA), Object storage
```

Ước lượng ban đầu (≤ 50 nhà xe): 2 brain-api × (1 vCPU, 512 MB), 2 ai-worker × (1 vCPU, 1 GB),
TEI 2 × embed + 2 × rerank (4 vCPU, 4 GB), Postgres 4 vCPU / 16 GB. Tải Runtime API phụ thuộc runtime nối vào sau;
đo và điều chỉnh khi đó.

## 7. Độ tin cậy

### 7.1 Khi một thành phần hỏng

| Sự cố | Ảnh hưởng | Cách xử lý |
|---|---|---|
| ai-worker chết / chậm | ingest, build chậm | Runtime API **vẫn phục vụ** snapshot/recall/data; job requeue khi hết lease |
| TEI chết | recall mất semantic + rerank | recall chạy keyword + graph + temporal, điểm RRF thay rerank |
| LLM provider chính lỗi | job nền chậm | thư viện chuyển provider dự phòng; job retry theo backoff |
| Postgres replica lỗi | đọc chậm | đọc chuyển về primary (có giới hạn); cảnh báo |
| Postgres primary lỗi | không ghi được | failover managed; Runtime API đọc từ replica vẫn chạy |
| Redis lỗi | mất cache, quota | bỏ qua cache; quota fallback giới hạn cứng theo process |
| Webhook tới runtime lỗi | runtime chậm nhận snapshot mới | retry + backoff; runtime luôn có thể poll `snapshot/active` |
| Snapshot lỗi lọt qua test | bot trả lời sai | rollback bằng phát hành lại snapshot trước (< 1 phút) |

### 7.2 Nguyên tắc

- **Đường đọc không phụ thuộc đường nền**: Go không gọi đồng bộ sang Python.
- **Mọi job idempotent** (`idempotency_key`), có `max_attempts`, backoff mũ; `failed` → cảnh báo.
- **Timeout ở mọi lời gọi ra ngoài.**
- **Degrade có kiểm soát**: mất một arm của recall không làm hỏng request.

### 7.3 SLO đề xuất

| SLO | Mục tiêu |
|---|---|
| Runtime API khả dụng | 99.9% / 30 ngày |
| `recall` p95 | < 120 ms |
| Update rủi ro thấp → snapshot staging | p95 < 5 phút |
| Phát hành / rollback → webhook tới runtime | < 5 s |

## 8. Bảo mật & multi-tenant

| Mặt | Thiết kế |
|---|---|
| Xác thực | OIDC cho console; token cá nhân cho MCP (có hạn, thu hồi được); API key theo runtime |
| Phân quyền | role `builder` · `lead` · `ops`; builder được gán danh sách nhà xe; MCP ẩn tool vượt quyền; API key runtime chỉ thấy bot được gán |
| Cách ly nhà xe | scope `operator_id` cố định theo URL MCP; service layer luôn lọc theo scope; **Postgres RLS** là lớp bảo vệ thứ hai |
| Webhook ra ngoài | ký HMAC theo runtime; không chứa secret |
| Secret | secret manager (API key LLM, khoá webhook); không nằm trong snapshot |
| Dữ liệu cá nhân | không lưu memory khách; transcript phải được ẩn danh **trước khi** gửi về Brain, Brain kiểm và redact lại; TTL 30 ngày |
| Prompt injection | nội dung nhà xe/khách luôn là *data*, bọc trong vùng dữ liệu của prompt; MCP yêu cầu confirm cho thao tác ghi quan trọng |
| Audit | mọi thao tác ghi: actor (người / `ai:<session>` / job / runtime), người duyệt, diff |
| Thay đổi rủi ro cao | L0/L1, phát hành production: quy tắc 2 người |

## 9. Observability

- **OpenTelemetry** cho Go và Python → Grafana stack (Prometheus, Tempo, Loki).
  Trace context đi qua `operations.trace_context`: một trace nối MCP call (Go) → job (Python) → compile → webhook.

| Nhóm metric | Ví dụ |
|---|---|
| Runtime API | latency recall/data/snapshot, tỉ lệ cache hit, lỗi theo runtime |
| Build | độ dài queue theo `kind`, thời gian job, tỉ lệ fail, tỉ lệ test pass mỗi compile |
| Chất lượng (khi runtime gửi feedback) | tỉ lệ 👎, handoff, knowledge gap mới theo nhà xe |
| Chi phí | token LLM theo nhà xe / theo loại job |
| Hạ tầng | Postgres (kết nối, replica lag), TEI QPS/latency, Redis |

Cảnh báo chính: recall p95 vượt SLO, queue tồn đọng, job `failed`, webhook lỗi liên tục, replica lag,
chi phí LLM vượt ngân sách ngày.

## 10. CI/CD

```
PR ──► lint + unit (Go, Python) ──► contract tests (fixture chung Go ⇄ Python, schema Runtime API)
   ──► migration check (apply lên DB trống + DB bản sao staging)
   ──► prompt tests (bộ case cố định cho từng prompt)
   ──► build images
main ─► deploy staging tự động ──► smoke + regression trên bot mẫu ──► duyệt ──► production (rolling)
```

- Migration chạy trước khi rollout code, luôn tương thích ngược một phiên bản (expand → migrate → contract).
- Thay đổi prompt là thay đổi code: qua PR, prompt tests, regression bot mẫu.
- Runtime API có version (`/runtime/v1`); thay đổi phá vỡ hợp đồng → `v2`, giữ `v1` trong thời gian chuyển đổi.
- Phát hành snapshot **không** đi qua CI/CD code — đi qua release gate của Brain.

## 11. Tech stack

| Lớp | Lựa chọn |
|---|---|
| API, MCP, Runtime API | Go 1.24+, `pgx`/`sqlc`, `modelcontextprotocol/go-sdk`, `errgroup` |
| Worker | Python 3.12, asyncio + uvloop, `asyncpg`, Pydantic, underthesea |
| Embedding / rerank | HF TEI trên CPU: bge-m3, bge-reranker-v2-m3 |
| Database | PostgreSQL 16 + pgvector, pg_trgm, unaccent; migration bằng golang-migrate |
| Cache / quota | Redis |
| Object storage | S3-compatible (MinIO ở local) |
| Console | React + Vite + TypeScript, TanStack Query, shadcn/ui |
| Hạ tầng | Kubernetes, KEDA, Helm; docker-compose cho local |
| Quan sát | OpenTelemetry → Grafana stack: Prometheus, Tempo, Loki |

## 12. Quyết định đã chốt

| # | Chủ đề | Quyết định |
|---|---|---|
| S1 | Hạ tầng tính toán | **không có GPU**; TEI chạy CPU, rerank giới hạn trên đường đọc (§6.2) |
| S2 | Gọi LLM | **qua thư viện** trong từng service, quy ước chung (§3.4); không có gateway ở v1 |
| S3 | Hotline (voice) | **ngoài phạm vi v1** |
| S4 | Observability | **OpenTelemetry → Grafana stack** (Prometheus, Tempo, Loki) |
| S5 | Console | **React + Vite + TypeScript**, TanStack Query, shadcn/ui |
| S6 | Chạy bot | **ngoài phạm vi**: Brain không chạy bot; mở Runtime Integration API (§3.5); nối runtime hiện có làm sau |
