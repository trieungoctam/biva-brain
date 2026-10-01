# System Architecture

Tài liệu này mô tả kiến trúc **hệ thống** của BIVA Brain: thành phần, triển khai, luồng chạy, độ tin cậy,
bảo mật, quan sát và CI/CD. Thiết kế nghiệp vụ ở [architecture.md](architecture.md), [build-flow.md](build-flow.md),
[mcp.md](mcp.md) và [logic-knowledge.md](logic-knowledge.md).

**Phạm vi**: **Brain + MCP** — AI của builder dùng tri thức (nhà xe và logic) để viết bot; Brain là nguồn sự thật
và người kiểm tra. Brain **không chạy bot**; bot được xuất ra định dạng trung lập. Runtime Integration API (§3.6)
là thiết kế cho **giai đoạn sau**, chưa nằm trong M0–M5.

## 1. Context — hệ thống và thế giới bên ngoài

```
 Nhà xe ──update (Zalo/Excel), trả lời câu hỏi, UAT──┐
                                                      ▼
 Builder ── AI client (Claude Code/Desktop/agent) ──MCP──►  ╔══════════════════╗ ──LLM API──► LLM providers
                                                            ║    BIVA BRAIN    ║               (chính + dự phòng)
 Lead/Ops ── Console web ─────────────────────────REST──►  ╚════════╤═════════╝
                                                                     │ export_bot: json · markdown · faq_csv
                                                                     ▼
                                                    Runtime chạy bot (hiện có) — NGOÀI PHẠM VI
                                                    (giai đoạn sau: Runtime Integration API)
 Git (repo code tích hợp) ◄──index theo commit── Brain
```

| Tác nhân | Tương tác với Brain | Kênh |
|---|---|---|
| Builder (qua AI) | đọc tri thức, viết bot và hồ sơ logic, validate, test, xuất bot | MCP |
| Lead / Ops | duyệt L0/L1, promote, phát hành production, giám sát | Console web |
| Nhà xe | gửi cập nhật, trả lời câu hỏi, UAT trong sandbox | Zalo, Excel (v1); form (M2) |
| Git | nguồn code tích hợp; Brain đọc để index tri thức logic | read-only |

## 2. Containers

```
                     ┌──────────────────────── Edge (Ingress) ────────────────────────┐
 AI client (MCP) ───►│ TLS · WAF · rate limit                                         │
 Console ───────────►│ /mcp/* /api/* → brain-api            / → console (static)      │
                     └───────────────────────────┬────────────────────────────────────┘
                                                 │
                     ┌───────────────────────────▼─────────────────────────────────┐
                     │ brain-api (Go)                                               │
                     │ MCP · REST · authz · recall · knowledge pack · artifacts +   │
                     │ validate tĩnh · logic · review · enqueue · scheduler (leader)│
                     └───┬───────────────┬────────────────────┬────────────────────┘
                         │               │                    │
                ┌────────▼──────┐ ┌──────▼─────────────────────┐ ┌──────────────────┐
                │ Redis         │ │ PostgreSQL 16              │ │ Object storage   │
                │ cache recall, │ │ primary + read replica     │ │ file gốc nhà xe, │
                │ pack, embed;  │ │ pgvector·pg_trgm·unaccent  │ │ export bot,      │
                │ quota LLM;    │ │ queue · NOTIFY             │ │ snapshot         │
                │ rate limit    │ └──────▲─────────────────────┘ └────────▲─────────┘
                └───────▲───────┘        │ claim / write                  │
                        │       ┌────────┴────────────────────────────────┴───────┐
                        └───────┤ ai-worker (Python) ×N                            │
                                │ ingest · consolidate · promote · mark_stale ·    │
                                │ validate (LLM) · run_tests · index_code ·        │
                                │ refresh_pages · reference executor (test/sandbox)│
                                └───────┬───────────────────────────┬─────────────┘
                                        ▼                           ▼
                          ┌──────────────────────────┐  ┌──────────────────────────────┐
                          │ TEI (Rust, CPU)           │  │ LLM providers (thư viện llm/: │
                          │ embed bge-m3 · rerank     │  │ quota Redis, fallback, cost)  │
                          └──────────────────────────┘  └──────────────────────────────┘

 Observability: OTel Collector → Grafana stack (Prometheus · Tempo · Loki)
```

| Container | Ngôn ngữ | Stateless | Scale theo | Ghi chú |
|---|---|---|---|---|
| `brain-api` | Go | ✅ | phiên MCP, request console | scheduler chạy trên **1 instance leader** (pg advisory lock) |
| `ai-worker` | Python | ✅ | độ dài queue | concurrency tách theo loại job |
| `console` | TypeScript (React + Vite) | ✅ | – | chủ yếu để **duyệt** và xem |
| `tei-embed`, `tei-rerank` | Rust | ✅ | QPS | **chạy CPU** (không có GPU) |
| PostgreSQL | – | ❌ | dữ liệu, QPS đọc | primary (ghi + queue) + replica (đọc) |
| Redis | – | ❌ (dữ liệu tạm) | – | cache recall / knowledge pack / embedding, quota LLM, rate limit |
| Object storage | – | ❌ | – | file gốc, export bot, snapshot |

## 3. Components bên trong

### 3.1 brain-api (Go)

```
mcp/             /mcp/operator/{id}/, /mcp/platform/ — tools, resources, prompts, confirm_token
httpapi/         REST cho console: operators, review, releases, test reports, pages
authz/           OIDC cho người, token cá nhân cho MCP; role builder|lead|ops; scope operator
services/        nghiệp vụ dùng chung cho MCP và REST (một lõi, hai giao diện)
recall/          4 arm, RRF, rerank (giới hạn), boost, pack
pack/            knowledge pack: merge tầng, lọc hiệu lực, cắt theo budget_tokens; cache theo version tri thức của scope
artifacts/       lưu artifact (version), validate tĩnh (trích dẫn, locked, nhãn L1, hardcoded data,
                 capability, coverage), stale, lắp snapshot, export
logic/           danh mục module/feature, hồ sơ, logic spec, họ logic, find_similar_operators (spec + code),
                 plan_logic_implementation, impact_of_change
queue/           enqueue job (idempotency_key), theo dõi operation
scheduler/       leader-only: expire (valid_to), requeue job hết lease, dọn TTL, cron refresh
store/           sqlc generated, pgxpool (primary + replica)
textnorm/        chuẩn hoá tiếng Việt cho query (cùng thuật toán với worker, kiểm bằng fixture chung)
```

### 3.2 ai-worker (Python)

```
runner/          claim job (SKIP LOCKED, lease, heartbeat), LISTEN để thức dậy, retry + backoff
jobs/            ingest · consolidate · promote (tri thức + logic) · mark_stale · validate (LLM:
                 mâu thuẫn) · run_tests · index_code (đồng bộ git) · extract_logic_spec ·
                 run_examples_against (sandbox) · refresh_pages
executor/        reference executor: chạy một snapshot như bot thật (LLM + tool tra tri thức/data của Brain)
                 — chỉ cho test và sandbox/UAT; không phục vụ khách
prompts/         toàn bộ prompt (có version, có test)
nlp/             chuẩn hoá tiếng Việt, tách từ, entity resolution, parse Excel
llm/             thư viện LLM (§3.4), structured output
index/           ghi search_text (không dấu + bigram), gọi TEI embed theo batch
```

### 3.3 console (React + Vite)

Review queue, duyệt L0/L1 và promote, duyệt phát hành production, Operator Profile pages, báo cáo validate/test,
giám sát job. Không chứa nghiệp vụ — chỉ gọi REST của brain-api.

### 3.4 Thư viện LLM (không có gateway)

LLM được gọi trực tiếp từ service qua thư viện `llm/` — bản Python (ai-worker) và bản Go (brain-api, chỉ khi cần),
**cùng một quy ước**:

| Quy ước | Cách làm |
|---|---|
| Cấu hình provider | một file `llm.yaml` dùng chung: provider, model theo hạng (*nhỏ* / *mạnh*), thứ tự fallback, timeout |
| Quota | token bucket trong Redis theo `(provider, purpose)`; purpose = `ingest` · `knowledge` · `validate` · `test` · `interactive` |
| Fallback | lỗi / timeout / 429 → provider kế tiếp; hết danh sách → lỗi rõ ràng, job retry theo backoff |
| Đo chi phí | mỗi lời gọi ghi metrics `tokens_in/out`, `cost`, nhãn `operator_id`, `purpose`, `model` |
| Kiểm thử | test hợp đồng chung: cùng cấu hình → cùng lựa chọn provider ở Go và Python |

Lưu ý: phần lớn LLM của quy trình build chạy **ở phía AI client của builder** (AI viết artifact). LLM của Brain chỉ dùng
cho ingest, consolidate, validate phần mâu thuẫn, reflect và reference executor.

### 3.5 Tích hợp git (tri thức logic)

- Repo code tích hợp chứa code **và manifest**: `modules/*/module.yaml`, `operators/*/profile.yaml`,
  `operators/*/tests/cases.yaml` ([logic-knowledge.md §3](logic-knowledge.md#3-lưu-ở-đâu)).
- Brain đọc repo (read-only); webhook push hoặc job định kỳ trên nhánh chính → `index_code`: đồng bộ manifest,
  hồ sơ, ví dụ; cắt code theo hàm/lớp thành `code_chunks` gắn commit; cập nhật logic spec và họ logic.
- Code **không** được ghi qua Brain: AI/builder sửa trong repo qua PR, CI chạy logic test, merge xong Brain đồng bộ.
  `propose_logic_profile` tạo PR thay vì ghi thẳng.
- **Sandbox** cho `run_examples_against`: container riêng trong ai-worker, không mạng, giới hạn CPU/RAM/thời gian,
  chỉ chạy hàm thuần với input giả lập; adapter gọi API thật không chạy kiểu này.

### 3.6 Runtime Integration API (giai đoạn sau)

> Chưa nằm trong M0–M5. Hiện bot được **xuất** bằng `export_bot`. Phần dưới là thiết kế để nối runtime khi cần.

| Endpoint | Mục đích |
|---|---|
| `GET /runtime/v1/bots/{bot_id}/snapshot/active` | Bot Definition đang phát hành; `ETag` → `304` nếu không đổi |
| `POST /runtime/v1/bots/{bot_id}/recall` | `{query, valid_at, max_tokens}` → item có nhãn tầng + nguồn |
| `POST /runtime/v1/bots/{bot_id}/data/{kind}` | routes · trips · fares · pickup_points; lọc hiệu lực theo ngày đi |
| `POST /runtime/v1/feedback`, `POST /runtime/v1/transcripts` | 👎, handoff, câu không trả lời được, log đã ẩn danh |
| Webhook `snapshot.published` | Brain → runtime khi có bản phát hành mới; ký HMAC |

Xác thực bằng API key theo runtime (bảng `runtime_clients`), mỗi key chỉ thấy bot được gán.

## 4. Luồng chạy chính

### 4.1 Builder làm việc qua MCP

```
AI client ──streamable HTTP──► Ingress ──► brain-api /mcp/operator/{id}/
   authz: token → user, role; scope operator cố định theo URL
   tool đọc   (pack, recall, query_data, logic…) → services → replica (+ cache Redis)
   tool ghi   (save_artifact, propose_*, apply_review…) → services → primary
              + audit_log(actor=ai:<session>, approved_by=<user>)
   tool nặng  (ingest, run_tests, validate phần LLM) → enqueue → operation_id → AI poll get_operation
```

### 4.2 Nhà xe gửi update → bot được cập nhật

```
AI (MCP) ──ingest──► brain-api ──INSERT operations + NOTIFY──► ai-worker: ingest
                                                                → items(pending) + review_items
AI (MCP) ──apply_review (preview → confirm_token, builder đồng ý)──► brain-api: apply trong 1 transaction
                                                                    → enqueue mark_stale → consolidate → promote
ai-worker: mark_stale → bot_artifacts / logic_profiles trích dẫn item cũ → stale (kèm vị trí)
AI (MCP) ──/refresh_bot: list_stale → sửa đúng đoạn → save_artifact → validate_artifact──► brain-api
brain-api: lắp snapshot từ artifact valid → check_release_gate → export_bot / request_publish
```

### 4.3 AI build bot cho nhà xe mới

```
AI ──get_bot_spec, get_knowledge_pack, get_operator_logic──► brain-api (replica + cache)
AI viết artifact (LLM ở phía AI client) ──save_artifact──► brain-api
AI ──validate_artifact──► brain-api: kiểm tĩnh đồng bộ (< 300 ms) + enqueue kiểm mâu thuẫn (ai-worker, LLM)
AI ──run_tests / sandbox_chat──► enqueue → ai-worker: reference executor
AI ──export_bot──► brain-api: snapshot → json · markdown · faq_csv (object storage, link tải)
```

### 4.4 Rollback

```
Lead/Builder ──rollback(bot, snapshot)──► brain-api: đánh dấu snapshot trước là bản phát hành; export lại
```

**Hai đường thay đổi tách biệt**: *deploy code* của Brain (container image qua CI/CD) và *phát hành bot*
(snapshot qua release gate). Thay đổi tri thức của nhà xe không bao giờ cần build lại code của Brain.

## 5. Lưu trữ dữ liệu

| Dữ liệu | Nơi lưu | Thời gian giữ |
|---|---|---|
| Tri thức (items, observations, entities, links) | Postgres | lâu dài; "quên" bằng status |
| Data vận hành (tuyến, chuyến, giá, điểm đón) | Postgres | lâu dài, có version |
| Artifact + trích dẫn, snapshot | Postgres; bản export ở object storage | mọi version (artifact nhỏ); export giữ N bản/bot + mọi bản từng phát hành |
| Tri thức logic (module, hồ sơ, ADR, test) | Postgres | lâu dài |
| Code chunk index | Postgres | chỉ commit hiện hành của nhánh chính; commit cũ dọn sau 30 ngày |
| Queue `operations` | Postgres | job xong: 30 ngày rồi dọn |
| File gốc nhà xe gửi | Object storage | theo hợp đồng nhà xe |
| Transcript sandbox/UAT (ẩn danh) | Postgres | 30 ngày (`expires_at`) |
| Cache recall / knowledge pack / embedding | Redis | vô hiệu khi version tri thức của scope đổi |
| Audit log | Postgres (partition theo tháng) | ≥ 1 năm |

Backup: Postgres PITR (WAL archive) + snapshot hằng ngày; object storage bật versioning.

## 6. Triển khai

### 6.1 Môi trường

| Môi trường | Mục đích |
|---|---|
| `local` | dev — docker-compose: Postgres, Redis, S3 (SeaweedFS), TEI (CPU), brain-api, ai-worker, console |
| `staging` | test tích hợp, thử MCP với AI client thật, dữ liệu nhà xe mẫu |
| `production` | Brain thật cho builder; snapshot có stage phát hành riêng (staging · production) |

### 6.2 Chạy không có GPU

| Việc | Cách làm trên CPU |
|---|---|
| Embed query (recall) | TEI CPU, câu ngắn ~10–30 ms; **cache embedding theo query đã chuẩn hoá** |
| Embed item, code chunk (nền) | ai-worker gọi TEI theo batch |
| Rerank mặc định | chỉ **top 30–50** sau RRF, ngân sách **80 ms**; quá hạn → điểm RRF + boost |
| Rerank sâu | top 300 cho `reflect` và khi dựng knowledge pack |
| Cache | recall và knowledge pack theo `(scope, version tri thức, query/purpose)` |

Phương án thay thế nếu CPU không đủ: gọi embedding/rerank qua API của provider bằng thư viện `llm/`.

### 6.3 Topology production (Kubernetes)

```
namespace biva-brain
├── brain-api       HPA theo CPU                       min 2   (1 leader cho scheduler)
├── ai-worker       KEDA theo COUNT(operations queued) min 1, max 20
├── console         static                             2
├── tei-embed       CPU (4 vCPU, 4 GB)                 2
├── tei-rerank      CPU (4 vCPU, 4 GB)                 1–2
└── otel-collector  daemonset
Managed: PostgreSQL (primary + 1 replica, HA), Redis (HA), Object storage
```

Ước lượng ban đầu (≤ 50 nhà xe, ≤ 30 builder): 2 brain-api × (1 vCPU, 512 MB), 2 ai-worker × (1 vCPU, 1 GB),
TEI 2 × embed + 1–2 × rerank (4 vCPU, 4 GB), Postgres 4 vCPU / 16 GB. Đo và điều chỉnh khi có tải thật.

## 7. Độ tin cậy

### 7.1 Khi một thành phần hỏng

| Sự cố | Ảnh hưởng | Cách xử lý |
|---|---|---|
| ai-worker chết / chậm | ingest, consolidate, test chậm | MCP **vẫn** đọc tri thức, lưu và validate tĩnh artifact, export; job requeue khi hết lease |
| TEI chết | recall mất semantic + rerank | recall chạy keyword + graph + temporal, điểm RRF thay rerank |
| LLM provider chính lỗi | job nền chậm | thư viện chuyển provider dự phòng; job retry theo backoff |
| Postgres replica lỗi | đọc chậm | đọc chuyển về primary (có giới hạn); cảnh báo |
| Postgres primary lỗi | không ghi được | failover managed; tool đọc vẫn chạy từ replica |
| Redis lỗi | mất cache, quota | bỏ qua cache; quota fallback giới hạn cứng theo process |
| Git không truy cập được | index code cũ | tri thức logic dùng index commit gần nhất; cảnh báo |
| Bản phát hành có lỗi | bot xuất ra sai | rollback = đánh dấu lại snapshot trước, export lại (< 1 phút) |

### 7.2 Nguyên tắc

- **Đường đọc không phụ thuộc đường nền**: Go không gọi đồng bộ sang Python.
- **Mọi job idempotent** (`idempotency_key`), có `max_attempts`, backoff mũ; `failed` → cảnh báo.
- **Timeout ở mọi lời gọi ra ngoài.**
- **Degrade có kiểm soát**: mất một arm của recall không làm hỏng request.

### 7.3 SLO đề xuất

| SLO | Mục tiêu |
|---|---|
| MCP khả dụng (giờ làm việc) | 99.5% / 30 ngày |
| `recall_knowledge` p95 | < 150 ms |
| `get_knowledge_pack` p95 | < 800 ms |
| `validate_artifact` phần tĩnh p95 | < 300 ms |
| Update rủi ro thấp → artifact liên quan được đánh dấu stale | p95 < 1 phút |

## 8. Bảo mật & multi-tenant

| Mặt | Thiết kế |
|---|---|
| Xác thực | OIDC cho console; token cá nhân cho MCP (có hạn, thu hồi được) |
| Phân quyền | role `builder` · `lead` · `ops`; builder được gán danh sách nhà xe; MCP ẩn tool vượt quyền |
| Cách ly nhà xe | scope `operator_id` cố định theo URL MCP; service layer luôn lọc theo scope; **Postgres RLS** là lớp bảo vệ thứ hai |
| Secret | secret manager (API key LLM, token git); không nằm trong tri thức, artifact hay export |
| Dữ liệu cá nhân | không lưu memory khách; transcript sandbox/UAT ẩn danh, TTL 30 ngày |
| Prompt injection | nội dung nhà xe gửi và nội dung code index luôn là *data*, bọc trong vùng dữ liệu của prompt; MCP yêu cầu confirm cho thao tác ghi quan trọng |
| Audit | mọi thao tác ghi: actor (người / `ai:<session>` / job), người duyệt, diff |
| Thay đổi rủi ro cao | L0/L1, promote, phát hành production: quy tắc 2 người |

## 9. Observability

- **OpenTelemetry** cho Go và Python → Grafana stack (Prometheus, Tempo, Loki).
  Trace context đi qua `operations.trace_context`: một trace nối MCP call (Go) → job (Python) → stale → refresh.

| Nhóm metric | Ví dụ |
|---|---|
| MCP | latency theo tool, tỉ lệ lỗi, số phiên, tỉ lệ cache hit của pack/recall |
| Chất lượng build | số lỗi validate theo mã, số vòng validate đến khi sạch, tỉ lệ artifact stale, coverage theo nhà xe |
| Job nền | độ dài queue theo `kind`, thời gian job, tỉ lệ fail, tỉ lệ test pass |
| Chi phí | token LLM theo nhà xe / theo purpose |
| Hạ tầng | Postgres (kết nối, replica lag), TEI QPS/latency, Redis |

Cảnh báo chính: tool MCP vượt SLO, queue tồn đọng, job `failed`, artifact stale lâu chưa xử lý,
replica lag, chi phí LLM vượt ngân sách ngày.

## 10. CI/CD

```
PR ──► lint + unit (Go, Python) ──► contract tests (fixture chung Go ⇄ Python, JSON Schema)
   ──► migration check (apply lên DB trống + DB bản sao staging)
   ──► prompt tests (bộ case cố định cho từng prompt)
   ──► MCP tests (kịch bản workflow trên nhà xe mẫu)
   ──► build images
main ─► deploy staging tự động ──► smoke + /build_bot trên nhà xe mẫu ──► duyệt ──► production (rolling)
```

- Migration chạy trước khi rollout code, luôn tương thích ngược một phiên bản (expand → migrate → contract).
- Thay đổi prompt hoặc tool MCP là thay đổi code: qua PR, prompt tests, MCP tests.
- Phát hành bot (snapshot) **không** đi qua CI/CD code — đi qua release gate của Brain.

## 11. Tech stack

| Lớp | Lựa chọn |
|---|---|
| API, MCP | Go 1.24+, `pgx`/`sqlc`, `modelcontextprotocol/go-sdk`, `errgroup` |
| Worker | Python 3.12, asyncio + uvloop, `asyncpg`, Pydantic, underthesea |
| Embedding / rerank | HF TEI trên CPU: bge-m3, bge-reranker-v2-m3 |
| Database | PostgreSQL 16 + pgvector, pg_trgm, unaccent; migration bằng golang-migrate |
| Cache / quota | Redis |
| Object storage | S3-compatible (SeaweedFS ở local; image MinIO không còn phát hành công khai) |
| Console | React + Vite + TypeScript, TanStack Query, shadcn/ui |
| Hạ tầng | Kubernetes, KEDA, Helm; docker-compose cho local |
| Quan sát | OpenTelemetry → Grafana stack: Prometheus, Tempo, Loki |

## 12. Quyết định đã chốt

| # | Chủ đề | Quyết định |
|---|---|---|
| S1 | Hạ tầng tính toán | **không có GPU**; TEI chạy CPU, rerank giới hạn (§6.2) |
| S2 | Gọi LLM | **qua thư viện** trong từng service, quy ước chung (§3.4); không có gateway ở v1 |
| S3 | Hotline (voice) | **ngoài phạm vi v1** |
| S4 | Observability | **OpenTelemetry → Grafana stack** (Prometheus, Tempo, Loki) |
| S5 | Console | **React + Vite + TypeScript**, TanStack Query, shadcn/ui |
| S6 | Chạy bot | **ngoài phạm vi**; Brain xuất bot; Runtime Integration API (§3.6) để giai đoạn sau |
| S7 | Code | ở **git**; Brain chỉ đọc và index theo commit (§3.5) |
