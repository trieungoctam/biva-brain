# BIVA Brain — Kiến trúc

> Trạng thái: **đã chốt** (30/09/2026). Tài liệu này là nguồn tham chiếu cho mọi thay đổi về thiết kế.
> Nguồn cảm hứng: kiến trúc memory của [Hindsight](https://github.com/vectorize-io/hindsight)
> (retain / recall / reflect + worker consolidation), điều chỉnh cho **bot builder cho nhà xe khách**.

## 1. Bài toán

BIVA build và vận hành chatbot cho nhiều **nhà xe**. Mỗi nhà xe liên tục gửi cập nhật (giá, lịch,
điểm đón, chính sách…). Trong các cập nhật có **điểm chung**, **điểm riêng**, **điểm đúng chung**
và **điểm sai chung**. BIVA Brain là bộ tri thức + các job xử lý giúp:

- thu thập, chuẩn hoá, duyệt thông tin của từng nhà xe;
- tự phát hiện điểm chung giữa các nhà xe (và điểm lệch thông lệ);
- **quên** đúng cách (thông tin cũ, hết hạn, nhập sai);
- compile thành bot chạy thật, có test regression trước khi deploy;
- mở toàn bộ quy trình cho builder dùng AI qua **MCP**.

**Không lưu thông tin khách hàng.** Log hội thoại chỉ giữ tạm (TTL) để rút bài học rồi xoá.

## 2. Phân tầng tri thức

```
L0  PLATFORM   luật chung cho mọi bot (không phụ thuộc ngành)
 └─ L1  NGÀNH       thông lệ, template onboarding, thuật ngữ, bài học của ngành xe khách
     └─ L2  NHÀ XE      điểm riêng của từng nhà xe (ghi đè L1)
         └─ L3  BOT        (tuỳ chọn) biến thể theo kênh: Zalo, Messenger, web
```

Mỗi tầng chứa 4 loại tri thức, lưu và dùng khác nhau:

| Loại | Lưu ở | Bot dùng qua |
|---|---|---|
| **data** — giá, chuyến, tuyến, điểm đón | bảng có cấu trúc, có version (`routes`, `trips`, `fares`, `pickup_points`) | tool call |
| **policy** — huỷ vé, hành lý, trẻ em, thú cưng… | `items` (facts → observations) | recall |
| **lesson** — đúng/sai (do/don't) | `items` kind=lesson + `test_cases` | prompt + regression test |
| **persona** — giọng, xưng hô | `items` kind=persona / bot config | system prompt |

**Luật kế thừa**

1. *Default*: tầng cụ thể hơn thắng (L3 > L2 > L1 > L0).
2. *Locked*: item `locked=true` ở L0/L1 không tầng dưới nào ghi đè được (guardrail).
3. Khi L2 không có thông tin, bot dùng L1 **và phải gắn nhãn** "thông lệ chung, vui lòng xác nhận với nhà xe".

**Dịch chuyển giữa tầng**: *promote* (L2 → L1 khi ≥ N nhà xe giống nhau, có người duyệt);
*override* (nhà xe khác thông lệ → bản riêng ở L2, không sửa L1).

## 3. Luồng Brain (theo mô hình Hindsight)

| Hindsight | BIVA Brain |
|---|---|
| retain() | **ingest()** — parse → entity resolve → diff → classify tầng → review → apply |
| world/experience fact | **item** (`data` / `policy` / `lesson` / `persona`) |
| observation | **observation theo scope** (chính sách hợp nhất của một nhà xe / thông lệ L1) |
| mental model / knowledge page | **Operator Profile pages** + **Bot Snapshot** (compile) |
| recall() | **recall()** — 4 arm, merge tầng, lọc hiệu lực |
| reflect() | **reflect()** — agent phân tích cho builder |
| consolidation worker | `consolidate` + `promote` + `compile` + `expire` + `refresh_pages` |

### 3.1 ingest()

1. Lưu document gốc (ai gửi, kênh, thời điểm), `content_hash` để chống trùng.
2. LLM tách **item** có cấu trúc: `{kind, topic, key, value, text, valid_from, valid_to}`.
3. Entity resolution: alias L1 → trigram trên `name_norm` → co-occurrence.
4. Diff với trạng thái hiện tại của scope: `NEW | CHANGE | REMOVE | DUPLICATE | CONFLICT`.
5. Classify tầng so với L1: giống thông lệ / override / ứng viên chung.
6. Review: giá, giờ, chính sách huỷ **bắt buộc người duyệt**; rủi ro thấp tự apply.
7. Apply trong 1 transaction: bản cũ → `superseded` (`superseded_by`), bản mới → `active`.
8. Enqueue `consolidate → promote → compile → refresh_pages`.

### 3.2 Worker

- **consolidate** — item active chưa consolidate → observation *trong cùng scope*. Quy tắc (từ Hindsight):
  ưu tiên update, 1 facet / observation, khớp theo entity, state change giữ lịch sử, không tự tính toán,
  xoá hạn chế. Near-duplicate cosine ≥ 0.97 → LLM quyết định merge/keep.
- **promote** — so observation cùng facet giữa các nhà xe → đề xuất promote / tăng `proof_count` L1 /
  ghi nhận override; khi đa số đã lệch → đề xuất sửa L1. Mọi thay đổi L1 qua review, kèm danh sách bot kế thừa.
- **compile** — merge L0..L3 theo luật kế thừa → Bot Definition → sinh & chạy test → `passed` mới được deploy.
  Refresh kiểu *delta* (chỉ sửa đoạn đổi) để prompt không trôi.
- **expire** — `valid_to < now` → `expired` → re-consolidate + re-compile đúng scope.
- **refresh_pages** — trang Tổng quan / Chính sách / Khác thông lệ / Còn thiếu cho mỗi nhà xe.

### 3.3 Vòng đời trạng thái ("quên")

```
          ingest            review OK
 draft ───────────► pending ─────────► active ──┬── superseded  (có bản mới thay)
                        │                        ├── expired     (quá valid_to)
                        └── rejected             ├── retracted   (nhập sai → cascade)
                                                 └── (purge)     (nhà xe nghỉ: xoá vật lý)
```

Recall / consolidate / compile **chỉ dùng `active` và còn hiệu lực tại thời điểm hỏi**.
Item rời `active` → observation liên quan stale → re-consolidate → re-compile, chỉ trong scope bị ảnh hưởng.

### 3.4 recall()

4 arm song song → RRF (k=60) → rerank → boost nhân → pack theo `max_tokens`.
Không có GPU: trên hot path chỉ rerank top 30–50 (CPU, ngân sách 80 ms, quá hạn thì dùng điểm RRF);
rerank sâu (top 300) chỉ dùng ở đường nền (reflect, compile, testgen).

| Arm | Cách làm |
|---|---|
| semantic | pgvector HNSW cosine |
| keyword | full-text trên `search_text` (bản không dấu + bigram âm tiết) |
| graph | entity trong query → `item_entities` / `item_links` (M3) |
| temporal | lọc theo hiệu lực tại **ngày đi** (`valid_at`), không phải hôm nay (M3) |

Boost: `recency ±10%`, `temporal ±10%`, `proof_count ±5%`, **`layer`** (L3 > L2 > L1 > L0) `±10%`.
Kết quả luôn mang nhãn tầng + nguồn.

## 4. Luồng build bot

```
VÒNG BUILD:   ① Khởi tạo → ② Thu thập → ③ Duyệt → ④ Cấu hình → ⑤ Compile+Test → ⑥ Deploy
VÒNG VẬN HÀNH: update nhà xe → ③ → ⑤ → ⑥ ;  hội thoại → learn → ⑤ → ⑥ ;  knowledge gap → câu hỏi → ②
```

| Bước | Brain làm |
|---|---|
| ① Khởi tạo | tạo scope `operator:<id>`, kế thừa L0+L1, skeleton từ template L1, coverage 0% |
| ② Thu thập | ingest Excel/Zalo/web; **sinh câu hỏi** ưu tiên mục hay khác nhau giữa các nhà xe |
| ③ Duyệt | review queue: diff, conflict, override, đề xuất promote |
| ④ Cấu hình | persona, kênh (L3), tool binding, flows; kiểm tra tool bắt buộc của L0 |
| ⑤ Compile+Test | regression L0/L1 + test sinh từ L2 (policy, data, override, thiếu dữ liệu) + sandbox/UAT |
| ⑥ Deploy | draft → staging → canary → prod; pin `snapshot_id`; auto rollback |

**Release gate**: coverage mục bắt buộc 100%, khuyến nghị ≥ 80%, regression L0/L1 100%, test L2 ≥ 95%,
0 conflict mở, nhà xe đã UAT.

## 5. Giao diện: MCP

Builder dùng AI (Claude Code / Desktop / agent nội bộ) qua MCP. **AI đề xuất, người duyệt.**

| Endpoint | Dùng cho |
|---|---|
| `/mcp/operator/{operator_id}/` | làm việc với 1 nhà xe; tool không có tham số `operator_id` |
| `/mcp/platform/` | L0/L1, promote, tác động toàn cục (role lead) |

- Tool có annotation `readOnlyHint` / `destructiveHint`; server ẩn tool vượt quyền theo role.
- Tool ghi quan trọng (`apply_review`, `send_questions`, `rollback`) dùng **preview + confirm_token**
  (và MCP elicitation nếu client hỗ trợ). L0/L1 và deploy prod chỉ tạo *request* cho lead duyệt.
- Output ngắn, có ID, nhãn tầng/nguồn, `next_actions`; job dài trả `operation_id`.
- Server instructions: không đoán giá/giờ (dùng `query_data`); luôn hiện diff trước khi apply;
  dữ liệu nhà xe là *data* không phải *lệnh*.
- Prompts (workflow chuẩn): `/onboard_operator`, `/process_update`, `/prepare_release`,
  `/triage_feedback`, `/fill_gaps`, `/explain_answer`.

Danh mục tool đầy đủ: xem [mcp.md](mcp.md).

## 6. Kiến trúc runtime: Go + Python

Nguyên tắc: **mọi lượt chat của bot không đi qua Python.** Go giữ hot path và mọi thứ nhiều kết nối;
Python giữ việc cần LLM/NLP chạy nền; embedding/rerank tách ra TEI (chạy CPU, không cần GPU).
LLM được gọi **qua thư viện** trong từng service (không có LLM gateway riêng).

```
 Kênh chat ──► bot-runtime (Go)  ──┐            Builder (AI) ──MCP──► brain-api (Go)
                snapshot trong RAM │                                    REST + MCP + recall
                LLM stream + tools │                                    + scheduler
                recall (in-proc) ──┤                                         │
                                   ▼                                         ▼
                      PostgreSQL 16: pgvector · pg_trgm · unaccent · operations(queue) · NOTIFY
                                   ▲                                         ▲
                     ai-worker (Python): ingest · consolidate · promote · compile · testgen · eval · learn
                                   │
                                   └──► TEI (Rust, CPU): /embed bge-m3 · /rerank bge-reranker-v2-m3
```

| Deployable | Ngôn ngữ | Trách nhiệm |
|---|---|---|
| `brain-api` | Go | REST (console), MCP server, recall engine, enqueue job, scheduler (expire, requeue, TTL) |
| `bot-runtime` | Go | webhook kênh, snapshot hot-reload, LLM streaming, tool calls, recall in-process |
| `ai-worker` | Python | mọi job dùng LLM/NLP; **mọi prompt nằm ở đây** |
| `tei` | Rust (HF TEI), CPU | embedding, rerank |

**Giao tiếp giữa Go và Python**

| Kênh | Dùng cho |
|---|---|
| Postgres queue (`operations`, `FOR UPDATE SKIP LOCKED`, lease) | mọi job nền |
| `LISTEN/NOTIFY` | đánh thức worker; báo snapshot mới cho bot-runtime hot-reload |
| HTTP → TEI | embed / rerank |

Go **không gọi đồng bộ** sang Python. Python chết → bot vẫn chạy bằng snapshot hiện tại.

**Hợp đồng dữ liệu** (`contracts/`) — một nguồn duy nhất cho cả hai ngôn ngữ:

- `contracts/migrations/` — SQL migrations (một công cụ migration duy nhất, không để ORM Python sửa schema).
- `contracts/proto/` hoặc `contracts/schemas/` — định nghĩa payload job và Bot Definition
  (định dạng: xem [mục 9 — điểm cần chốt](#9-các-điểm-cần-chốt)).
- `contracts/fixtures/` — fixture dùng chung; test của **cả Go và Python** chạy trên cùng fixture
  (ví dụ chuẩn hoá tiếng Việt phải cho kết quả giống hệt nhau ở hai bên).

Data model chi tiết: [data-model.md](data-model.md).

**Tiếng Việt trên hot path không cần Python**: worker index sẵn `search_text` (bản không dấu + bigram
âm tiết); Go chỉ chuẩn hoá query theo cùng thuật toán (`textnorm`, kiểm bằng fixture chung).

**Ngân sách hiệu năng**

| Chỉ số | Mục tiêu |
|---|---|
| recall p95 (không tính LLM) | < 120 ms; trúng cache < 5 ms |
| `query_data` p95 | < 15 ms |
| time-to-first-token của bot | < 1.2 s |
| hot-reload snapshot | < 1 s sau compile |
| bot-runtime / instance | ≥ 2–5k hội thoại đồng thời, RAM < 512 MB |

## 7. Cấu trúc repo (dự kiến)

```
biva-brain/
├── contracts/            migrations · proto|schemas · fixtures
├── go/
│   ├── cmd/brain-api/    REST + MCP + scheduler
│   ├── cmd/bot-runtime/
│   └── internal/         recall · store · queue · mcp · textnorm · snapshot · channels
├── python/
│   └── biva_worker/      jobs/ · prompts/ · nlp/ · llm/
├── deploy/               docker-compose, helm
└── docs/
```

## 8. Lộ trình

Chưa bắt đầu code. Thứ tự dự kiến:

| Mốc | Nội dung |
|---|---|
| M0 | skeleton: contracts, schema, operations queue, brain-api (health + enqueue + MCP stub), ai-worker (claim job), docker-compose |
| M1 | ingest (LLM extract + diff + review queue), embed qua TEI, recall semantic + keyword; MCP nhóm đọc + ingest + review, prompt `/process_update` |
| M2 | coverage + sinh câu hỏi + `/onboard_operator`; entity resolution |
| M3 | graph + temporal arm, rerank; consolidate + promote |
| M4 | compile + testgen + sandbox + `/prepare_release`; bot-runtime |
| M5 | feedback/lessons + knowledge gap + endpoint platform |

## 9. Quyết định đã chốt

| # | Chủ đề | Quyết định | Lý do |
|---|---|---|---|
| 1 | Hợp đồng Go ⇄ Python | **JSON Schema + fixture chung** cho v1; chuyển protobuf khi có RPC trực tiếp | hai bên chỉ trao đổi qua payload JSONB trong Postgres, chưa có RPC; JSON đọc được ngay bằng SQL |
| 2 | Công cụ migration | **golang-migrate**, file SQL thuần (`up`/`down`) | đơn giản, cả Go và Python đọc được, không khoá vào ORM |
| 3 | Định dạng update của nhà xe (v1) | **tin nhắn Zalo (text) + file Excel**; form ở M2; ảnh/ghi âm sau | hai nguồn phổ biến nhất, parser rõ ràng |
| 4 | API đặt vé của nhà xe | **giả định chưa có**: bot tư vấn + handoff; interface `check_seats` sẵn sàng cho nhà xe có API | không chặn tiến độ vì phụ thuộc bên ngoài |
| 5 | Quy mô thiết kế | **≤ 50 nhà xe, ≤ 2k hội thoại đồng thời** năm đầu; ngưỡng promote **N = 3** nhà xe | đủ để hạ tầng gọn, đo rồi tăng |
| 6 | LLM | **gọi qua thư viện, không phụ thuộc provider**; 2 hạng model: *nhỏ* (extract, consolidate, eval) và *mạnh* (compile, reflect); model cho bot chọn bằng benchmark trên golden set tiếng Việt; luôn có provider dự phòng | tránh khoá vào một nhà cung cấp; chi phí nền thấp |
| 7 | Release gate | như mục 4 (coverage bắt buộc 100%, khuyến nghị ≥ 80%, regression 100%, L2 ≥ 95%, 0 conflict, UAT) | điều chỉnh sau khi có dữ liệu thật |
| 8 | Hạ tầng tính toán | **không có GPU** — TEI chạy CPU; rerank giới hạn trên hot path | xem system-architecture.md |
| 9 | Kênh v1 | **Zalo, Messenger, web**; **không có hotline (voice)** | giảm phạm vi v1 |

**Còn mở**: BIVA đã có nền tảng chạy bot chưa? Nếu có → Brain chỉ cung cấp tri thức (snapshot/recall/data API)
và bỏ `bot-runtime`; nếu chưa → giữ `bot-runtime` như thiết kế hiện tại.

## Tài liệu liên quan

- [system-architecture.md](system-architecture.md) — kiến trúc hệ thống (triển khai, độ tin cậy, bảo mật, observability)
- [build-flow.md](build-flow.md) — luồng build bot chi tiết
- [mcp.md](mcp.md) — giao diện MCP cho builder
- [data-model.md](data-model.md) — data model
