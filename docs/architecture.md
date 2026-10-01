# BIVA Brain — Kiến trúc

> Trạng thái: **đã chốt** (cập nhật 01/10/2026). Tài liệu này là nguồn tham chiếu cho mọi thay đổi về thiết kế.
> Nguồn cảm hứng: kiến trúc memory của [Hindsight](https://github.com/vectorize-io/hindsight)
> (retain / recall / reflect + worker consolidation), điều chỉnh cho **bot builder cho nhà xe khách**.

## 1. Bài toán và phạm vi

BIVA build chatbot cho nhiều **nhà xe**. Mỗi nhà xe liên tục gửi cập nhật (giá, lịch, điểm đón, chính sách…),
và mỗi nhà xe cần logic xử lý (tính giá, giữ chỗ, đồng bộ lịch…) — phần lớn giống nhau, một số đặc biệt.
Trong cả hai loại có **điểm chung**, **điểm riêng**, **điểm đúng chung** và **điểm sai chung**.

BIVA Brain giúp:

- thu thập, chuẩn hoá, duyệt thông tin của từng nhà xe;
- lưu **tri thức logic** (cách xử lý/code): logic chung và logic đặc biệt;
- tự phát hiện điểm chung giữa các nhà xe (và điểm lệch thông lệ);
- **quên** đúng cách (thông tin cũ, hết hạn, nhập sai) — và báo phần bot bị ảnh hưởng;
- cho builder dùng AI qua **MCP**: AI đọc tri thức, **viết bot**; Brain **kiểm tra** và **xuất** bot.

**Trọng tâm: Brain + MCP.** AI là người viết bot, Brain là nguồn sự thật và người kiểm tra.

**Ngoài phạm vi**: chạy bot. Các runtime hiện có của BIVA chưa thống nhất; Brain xuất bot ra định dạng trung lập.
Runtime Integration API được thiết kế sẵn cho giai đoạn sau ([system-architecture.md §3.6](system-architecture.md#36-runtime-integration-api-giai-đoạn-sau)).

**Không lưu thông tin khách hàng.**

## 2. Phân tầng tri thức

```
L0  PLATFORM   luật chung cho mọi bot (không phụ thuộc ngành)
 └─ L1  NGÀNH       thông lệ, template onboarding, module logic chuẩn, thuật ngữ, bài học của ngành xe khách
     └─ L2  NHÀ XE      điểm riêng của từng nhà xe (ghi đè L1), hồ sơ logic
         └─ L3  BOT        (tuỳ chọn) biến thể theo kênh: Zalo, Messenger, web
```

Mỗi tầng chứa 5 loại tri thức:

| Loại | Lưu ở | Bot dùng qua |
|---|---|---|
| **data** — giá, chuyến, tuyến, điểm đón | bảng có cấu trúc, có version (`routes`, `trips`, `fares`, `pickup_points`) | tool |
| **policy** — huỷ vé, hành lý, trẻ em, thú cưng… | `items` (facts → observations) | câu trong artifact (có trích dẫn) |
| **lesson** — đúng/sai (do/don't), cả về hội thoại lẫn về code | `items` kind=lesson + `test_cases` | artifact + regression test |
| **persona** — giọng, xưng hô | `items` kind=persona | artifact `persona` |
| **logic** — tính giá, giữ chỗ, đồng bộ lịch, adapter API… | danh mục module + hồ sơ logic từng nhà xe; code ở git, Brain index theo commit | tool của bot trỏ tới capability ([logic-knowledge.md](logic-knowledge.md)) |

**Luật kế thừa**

1. *Default*: tầng cụ thể hơn thắng (L3 > L2 > L1 > L0).
2. *Locked*: item `locked=true` ở L0/L1 không tầng dưới nào ghi đè được (guardrail).
3. Khi L2 không có thông tin, dùng L1 **và phải gắn nhãn** "thông lệ chung, vui lòng xác nhận với nhà xe".

**Dịch chuyển giữa tầng**: *promote* (L2 → L1 khi ≥ 3 nhà xe giống nhau, có người duyệt);
*override* (nhà xe khác thông lệ → bản riêng ở L2, không sửa L1). Áp dụng cho cả tri thức lẫn logic.

## 3. Luồng Brain (theo mô hình Hindsight)

| Hindsight | BIVA Brain |
|---|---|
| retain() | **ingest()** — parse → entity resolve → diff → classify tầng → review → apply |
| world/experience fact | **item** (`data` / `policy` / `lesson` / `persona`) |
| observation | **observation theo scope** (chính sách hợp nhất của một nhà xe / thông lệ L1) |
| mental model / knowledge page | **Operator Profile pages** + **knowledge pack** (gói tri thức cho AI build bot) |
| recall() | **recall_knowledge** — 4 arm, merge tầng, lọc hiệu lực |
| reflect() | **reflect** — agent phân tích cho builder |
| consolidation worker | `consolidate` · `promote` · `expire` · `mark_stale` · `refresh_pages` |

### 3.1 ingest()

Hai đầu vào, chung một pipeline: **AI phía builder** (ChatGPT, coding agent) tự đọc nguồn — tin Zalo, file Excel,
ảnh bảng giá, ghi chú cuộc gọi — rồi gửi item có cấu trúc (`submit_knowledge`, không gọi LLM ở Brain); nội dung
**thô** chưa trích thì gửi `ingest` để ai-worker trích bằng LLM. Brain không tự parse file Excel/ảnh.

1. Lưu document gốc (ai gửi, kênh, thời điểm), `content_hash` để chống trùng.
2. Item có cấu trúc `{kind, topic, key, value, text, valid_from, valid_to}`: từ AI client, hoặc LLM tách từ nội dung thô.
3. Entity resolution: alias L1 → trigram trên `name_norm` → co-occurrence.
4. Diff với trạng thái hiện tại của scope: `NEW | CHANGE | REMOVE | DUPLICATE | CONFLICT`.
5. Classify tầng so với L1: giống thông lệ / override / ứng viên chung.
6. Review: **tự apply** chỉ khi vô hại — nhắc lại điều đã đúng (DUPLICATE) hoặc thêm mới (NEW) ở topic không
   đụng tiền/giờ. **Bắt buộc người duyệt**: mọi CHANGE/REMOVE (sửa, bỏ điều đang đúng), CONFLICT, và mọi đề xuất về
   giá, lịch chạy, huỷ vé, thanh toán.
7. Apply trong 1 transaction (SQL `apply_review`, dùng chung Go/Python): bản cũ → `superseded` (`superseded_by`),
   bản mới → `active`; **bản mới có hiệu lực từ ngày tương lai** thì bản cũ vẫn `active` với `valid_to` = ngày đó
   (job `expire` chuyển sang `expired` khi tới hạn) — DB chặn hai bản active cùng key chồng khoảng hiệu lực.
   Đề xuất khác đang mở cho cùng key → `stale`. Data vận hành lên version mới.
8. Enqueue `mark_stale → consolidate → promote → refresh_pages`.

### 3.2 Job nền

| Job | Làm gì |
|---|---|
| **consolidate** | item active chưa consolidate → observation *trong cùng scope*. Quy tắc (từ Hindsight): ưu tiên update, 1 facet / observation, khớp theo entity, state change giữ lịch sử, không tự tính toán, xoá hạn chế. Near-duplicate cosine ≥ 0.97 → LLM quyết định merge/keep |
| **promote** | so observation cùng facet giữa các nhà xe (và hồ sơ logic cùng capability) → đề xuất promote / tăng `proof_count` L1 / ghi nhận override; đa số đã lệch → đề xuất sửa L1. Mọi thay đổi L1 qua review |
| **mark_stale** | item rời `active` → mọi artifact trích dẫn nó và hồ sơ logic lấy tham số từ nó → `stale` (kèm vị trí) |
| **expire** | `valid_to < now` → `expired` → `mark_stale` + re-consolidate đúng scope |
| **refresh_pages** | trang Tổng quan / Chính sách / Khác thông lệ / Còn thiếu / Logic cho mỗi nhà xe |
| **validate** (phần LLM) | kiểm tra mâu thuẫn giữa artifact và tri thức (phần tĩnh chạy đồng bộ ở brain-api) |
| **run_tests** | chạy test bằng reference executor (M3) |
| **index_code** | đồng bộ từ git: manifest, hồ sơ, ví dụ; index code chunk theo commit |
| **extract_logic_spec** | tri thức nhà xe → logic spec (ánh xạ vào feature catalog L1); gom họ logic |
| **run_examples_against** | chạy ví dụ của nhà xe trên code nhà xe khác trong sandbox (M3) |

### 3.3 Vòng đời trạng thái ("quên")

```
          ingest            review OK
 draft ───────────► pending ─────────► active ──┬── superseded  (có bản mới thay)
                        │                        ├── expired     (quá valid_to)
                        └── rejected             ├── retracted   (nhập sai → cascade)
                                                 └── (purge)     (nhà xe nghỉ: xoá vật lý)
```

Recall, knowledge pack, consolidate và validate **chỉ dùng `active` và còn hiệu lực tại thời điểm hỏi**.
Item rời `active` → observation liên quan được tính lại, artifact/hồ sơ logic liên quan → `stale`,
chỉ trong scope bị ảnh hưởng.

### 3.4 recall_knowledge

4 arm song song → RRF (k=60) → rerank → boost nhân → pack theo `max_tokens`.
Không có GPU: mặc định rerank top 30–50 (CPU, ngân sách 80 ms, quá hạn thì dùng điểm RRF);
rerank sâu (top 300) cho `reflect` và khi dựng knowledge pack.

| Arm | Cách làm |
|---|---|
| semantic | pgvector HNSW cosine |
| keyword | full-text trên `search_text` (bản không dấu + bigram âm tiết) |
| graph | entity trong query → `item_entities` / `item_links` (M3) |
| temporal | lọc theo hiệu lực tại `valid_at` (vd **ngày đi**), không phải hôm nay (M3) |

Boost: `recency ±10%`, `temporal ±10%`, `proof_count ±5%`, **`layer`** (L3 > L2 > L1 > L0) `±10%`.
Kết quả luôn mang nhãn tầng + nguồn.

**Bản M1** (`brain-api/internal/recall`): semantic + keyword song song → RRF → boost `layer ±10%`, `proof_count +5%`,
độ mới `±5%` → cắt theo `max_tokens` (luôn ≥ 1 kết quả). Lọc `active` + còn hiệu lực trong **cả ngày** `valid_at`
(giá mới từ 01/11 khớp ngày 01/11, bản cũ hết hạn 31/10 thì không). Nhánh semantic: query nhúng bằng TEI (cùng
bge-m3 với job `index.items`), ngưỡng cosine 0.45, `hnsw.ef_search` 200 + iterative scan (pgvector ≥ 0.8) để lọc theo
nhà xe không bị thiếu ứng viên; TEI lỗi/chậm (> 800 ms) → chỉ keyword, kết quả báo `degraded`. Đo trên 3 × 300 item:
p95 ≈ 12 ms (chưa tính TEI). Graph/temporal arm, rerank: M3.

## 4. Luồng build bot

```
VÒNG BUILD:    ① Khởi tạo → ② Thu thập → ③ Duyệt → ④ Viết bot (AI) → ⑤ Kiểm tra → ⑥ Xuất & phát hành
VÒNG CẬP NHẬT: update nhà xe → ③ → stale → ④ (/refresh_bot, chỉ đoạn bị ảnh hưởng) → ⑤ → ⑥
               hết hạn (valid_to) → stale → ④ → ⑤ → ⑥ ;  thiếu dữ liệu (từ sandbox/UAT) → câu hỏi → ②
```

| Bước | Ai làm | Brain làm |
|---|---|---|
| ① Khởi tạo | builder | scope `operator:<id>`, kế thừa L0+L1, skeleton từ template L1, coverage 0% |
| ② Thu thập | builder + nhà xe | ingest Zalo/Excel; **sinh câu hỏi** ưu tiên mục hay khác nhau giữa các nhà xe |
| ③ Duyệt | builder | review queue: diff, conflict, override, đề xuất promote |
| ④ Viết bot | **AI** qua MCP | cấp **knowledge pack** + bot spec; AI viết artifact (có trích dẫn) và hồ sơ logic (config → hook → custom) |
| ⑤ Kiểm tra | Brain + builder + nhà xe | `validate_artifact`, logic test, `run_tests`, sandbox/UAT bằng reference executor |
| ⑥ Xuất & phát hành | builder (+ lead) | lắp **snapshot** từ artifact valid → `export_bot` (json · markdown · faq_csv); `request_publish` đánh dấu bản phát hành |

**Release gate**: coverage mục bắt buộc 100%, khuyến nghị ≥ 80%; mọi artifact `valid`, 0 `stale`;
logic test của các capability đang dùng pass; regression L0/L1 100%; test L2 ≥ 95%; 0 conflict mở; nhà xe đã UAT.

Chi tiết: [build-flow.md](build-flow.md).

## 5. Giao diện: MCP

MCP là **giao diện chính**. Builder làm việc với AI (Claude Code / Desktop / agent nội bộ). **AI đề xuất, người duyệt.**

| Endpoint | Dùng cho |
|---|---|
| `/mcp/operator/{operator_id}/` | làm việc với 1 nhà xe; tool không có tham số `operator_id` |
| `/mcp/platform/` | L0/L1, promote, tác động toàn cục (role lead) |

Điểm cốt lõi:

- **Knowledge pack**: tri thức đã merge theo tầng, còn hiệu lực, có nhãn nguồn, vừa ngân sách token.
- **Hợp đồng trích dẫn**: mọi câu mang thông tin trong artifact gắn `[[item_id]]`; mọi tham số logic có `param_sources`.
- **Validate** trả lỗi có vị trí (`UNCITED`, `STALE_CITATION`, `MISSING_LOCKED`, `HARDCODED_DATA`…) để AI tự sửa.
- **Stale**: tri thức bị quên → đúng đoạn/tham số bị ảnh hưởng được đánh dấu → `/refresh_bot`.
- Tool ghi quan trọng dùng **preview + confirm_token**; L0/L1 và phát hành production chỉ tạo *request* cho lead.
- Prompts: `/onboard_operator`, `/process_update`, `/implement_operator_logic`, `/build_bot`, `/refresh_bot`, `/review_quality`.

Chi tiết: [mcp.md](mcp.md).

## 6. Kiến trúc runtime: Go + Python

Nguyên tắc: **đường đọc (tool MCP đọc tri thức, knowledge pack, validate tĩnh) không đi qua Python.**
Go giữ đường đọc và mọi thứ nhiều kết nối; Python giữ việc cần LLM/NLP chạy nền; embedding/rerank tách ra TEI (CPU).
LLM được gọi **qua thư viện** trong từng service (không có LLM gateway riêng).

```
 Builder ── AI client ──MCP──┐        Console (lead/ops) ──REST──┐
                             ▼                                    ▼
                      brain-api (Go): MCP · REST · recall · knowledge pack · validate tĩnh ·
                                      artifacts · logic · scheduler
                             │
       PostgreSQL 16: pgvector · pg_trgm · unaccent · operations(queue) · NOTIFY
                             ▲
       ai-worker (Python): ingest · consolidate · promote · validate (LLM) · run_tests ·
                           index_code · refresh_pages + reference executor (test/sandbox)
                             │
                             └──► TEI (Rust, CPU): /embed bge-m3 · /rerank bge-reranker-v2-m3
```

| Deployable | Ngôn ngữ | Trách nhiệm |
|---|---|---|
| `brain-api` | Go | MCP server, REST (console), recall, knowledge pack, artifact + validate tĩnh, tri thức logic, enqueue job, scheduler |
| `ai-worker` | Python | mọi job dùng LLM/NLP; **mọi prompt nằm ở đây**; reference executor cho test/sandbox |
| `console` | TypeScript | duyệt review queue, L0/L1, phát hành; xem pages, báo cáo test |
| `tei` | Rust (HF TEI), CPU | embedding, rerank |

**Reference executor**: bộ chạy bot tối giản trong ai-worker (LLM + tool tra tri thức/data của Brain) để chạy test
và sandbox/UAT mà không phụ thuộc runtime nào. Không phục vụ khách thật.

**Giao tiếp giữa Go và Python**

| Kênh | Dùng cho |
|---|---|
| Postgres queue (`operations`, `FOR UPDATE SKIP LOCKED`, lease) | mọi job nền |
| `LISTEN/NOTIFY` | đánh thức worker; báo job xong |
| HTTP → TEI | embed / rerank |

Go **không gọi đồng bộ** sang Python. Python chết → MCP vẫn đọc tri thức, lưu và validate tĩnh artifact được;
chỉ ingest/consolidate/test chậm lại.

**Hợp đồng dữ liệu** (`contracts/`) — một nguồn duy nhất cho cả hai ngôn ngữ:

- `contracts/migrations/` — SQL migrations (golang-migrate; không để ORM Python sửa schema).
- `contracts/schemas/` — JSON Schema cho payload job, knowledge pack, artifact, Bot Definition.
- `contracts/fixtures/` — fixture dùng chung; test của **cả Go và Python** chạy trên cùng fixture
  (ví dụ chuẩn hoá tiếng Việt phải cho kết quả giống hệt nhau ở hai bên).

**Tiếng Việt trên đường đọc không cần Python**: worker index sẵn `search_text` (bản không dấu + bigram âm tiết);
Go chỉ chuẩn hoá query theo cùng thuật toán (`textnorm`, kiểm bằng fixture chung).

**Ngân sách hiệu năng (tool MCP)**

| Chỉ số | Mục tiêu |
|---|---|
| `recall_knowledge` p95 | < 150 ms; trúng cache < 5 ms |
| `query_data` p95 | < 15 ms |
| `get_knowledge_pack` p95 | < 800 ms (có cache theo phiên bản tri thức của scope) |
| `validate_artifact` (phần tĩnh) p95 | < 300 ms; phần LLM chạy nền, trả qua `operation_id` |
| Update rủi ro thấp → artifact liên quan được đánh dấu stale | < 1 phút |

Data model: [data-model.md](data-model.md). Hệ thống, triển khai, bảo mật: [system-architecture.md](system-architecture.md).

## 7. Cấu trúc repo

Thư mục đặt theo **thành phần** (service), không theo ngôn ngữ.

```
biva-brain/
├── contracts/            migrations · schemas · fixtures · llm.yaml · textnorm (dùng chung Go ⇄ Python)
├── brain-api/            service Go
│   ├── cmd/brain-api/    MCP + REST + scheduler + CLI quản trị
│   └── internal/         mcpserver · authz · queue · scheduler · kb · textnorm · store · recall · pack…
├── ai-worker/            service Python
│   └── biva_worker/      runner · llm/ · index · textnorm · (ingest, consolidate, executor…)
├── kb/                   tri thức nền L0/L1 (YAML, review bằng PR)
├── console/              React + Vite
├── deploy/               docker-compose, helm
└── docs/
```

## 8. Lộ trình

Chưa bắt đầu code. Thứ tự dự kiến (chi tiết epic/story: [implementation-plan.md](implementation-plan.md)):

| Mốc | Nội dung |
|---|---|
| M0 | skeleton: contracts, schema, operations queue, brain-api (health + enqueue + MCP stub), ai-worker (claim job), docker-compose |
| M1 | ingest + diff + review queue; recall semantic + keyword; **knowledge pack**; artifact (`save` / `validate` tĩnh với hợp đồng trích dẫn); MCP đọc + ingest + review + build; prompt `/process_update`, `/build_bot` |
| M2 | coverage + sinh câu hỏi + `/onboard_operator`; entity resolution; **tri thức logic** (module, hồ sơ, ADR, `find_similar_operators`); `mark_stale` + `/refresh_bot`; `export_bot` |
| M3 | graph + temporal arm, rerank; consolidate + promote (tri thức + logic); validate phần LLM; logic test; reference executor + `run_tests` + `sandbox_chat` |
| M4 | release gate + `request_publish`; lessons + `/review_quality`; console duyệt |
| M5 | endpoint platform (L0/L1, promote, regression toàn bộ) |
| Sau | Runtime Integration API để nối các runtime chạy bot; feedback/learn từ hội thoại thật |

## 9. Quyết định đã chốt

| # | Chủ đề | Quyết định | Lý do |
|---|---|---|---|
| 1 | Hợp đồng Go ⇄ Python | **JSON Schema + fixture chung**; chuyển protobuf khi có RPC trực tiếp | hai bên chỉ trao đổi qua payload JSONB trong Postgres |
| 2 | Công cụ migration | **golang-migrate**, file SQL thuần (`up`/`down`) | đơn giản, không khoá vào ORM |
| 3 | Định dạng update của nhà xe (v1) | **tin nhắn Zalo (text) + file Excel**; form ở M2; ảnh/ghi âm sau | hai nguồn phổ biến nhất |
| 4 | API đặt vé của nhà xe | **giả định chưa có**: bot tư vấn + handoff; capability `check_seats` sẵn sàng cho nhà xe có API | không chặn tiến độ |
| 5 | Quy mô thiết kế | **≤ 50 nhà xe, ≤ 30 builder** năm đầu; ngưỡng promote **N = 3** nhà xe | hạ tầng gọn, đo rồi tăng |
| 6 | LLM | **Gemini** (chốt 10/2026), gọi qua thư viện `llm/` (adapter theo provider); hạng *nhỏ* (ingest, consolidate, eval) và *mạnh* (validate LLM, reflect, reference executor); fallback giữa các model trong tier; thêm provider dự phòng = thêm adapter | thư viện không khoá nhà cung cấp; chi phí nền thấp |
| 7 | Release gate | như mục 4 | điều chỉnh sau khi có dữ liệu thật |
| 8 | Hạ tầng tính toán | **không có GPU** — TEI chạy CPU; rerank giới hạn | |
| 9 | Kênh v1 | **Zalo, Messenger, web**; **không có hotline (voice)** | giảm phạm vi |
| 10 | Chạy bot | **ngoài phạm vi**; Brain xuất bot (json · markdown · faq_csv); Runtime Integration API để giai đoạn sau | runtime hiện có chưa thống nhất |
| 11 | Trọng tâm | **Brain + MCP**: AI viết bot từ knowledge pack, mọi câu có trích dẫn; Brain validate, đánh dấu stale, xuất bot | bot luôn khớp tri thức, kể cả khi "quên" |
| 12 | Tri thức logic | code ở git kèm manifest (`module.yaml`, `profile.yaml`, `tests/cases.yaml`); Brain đồng bộ và lưu **tri thức về code**; so nhà xe theo **logic spec** (feature catalog L1) → code → **hành vi** (chạy ví dụ trong sandbox); gom **họ logic**; bậc thang config → hook → custom; promote khi ≥ 3 nhà xe có custom giống nhau | khách mới triển khai nhanh từ nhà xe tương tự; logic chung được dùng lại |
| 13 | Ai viết bot | **AI của builder** (qua MCP) viết artifact; Brain **không tự sinh** bot, chỉ lắp snapshot từ artifact đã valid | giữ người + AI trong vòng quyết định; Brain giữ vai trò kiểm tra |

## Tài liệu liên quan

- [mcp.md](mcp.md) — giao diện MCP: AI dùng tri thức để build bot
- [logic-knowledge.md](logic-knowledge.md) — tri thức logic (code) cho nhà xe
- [build-flow.md](build-flow.md) — luồng build bot chi tiết
- [data-model.md](data-model.md) — data model
- [implementation-plan.md](implementation-plan.md) — kế hoạch triển khai theo milestone, epic, story
- [system-architecture.md](system-architecture.md) — kiến trúc hệ thống (triển khai, độ tin cậy, bảo mật, observability)
