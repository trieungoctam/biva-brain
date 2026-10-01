# BIVA Brain — ghi chú cho Claude Code

Brain tri thức (kiểu Hindsight) để build chatbot cho nhiều nhà xe khách. Builder dùng AI client (ChatGPT
connector, Claude Code / coding agent) qua MCP để nạp tri thức nhà xe và viết bot. Chủ dự án: Triệu Ngọc Tâm.
Trao đổi với chủ dự án bằng **tiếng Việt**; comment code, docs, thông báo lỗi cho AI cũng viết tiếng Việt.
Commit message tiếng Anh.

Tài liệu thiết kế: `docs/architecture.md`, `docs/mcp.md` (danh mục tool, luật trích dẫn), `docs/data-model.md`,
`docs/implementation-plan.md` (milestone M0–M5, story S*.*.* ↔ issue Linear DYN-*).

## Cấu trúc

| Thư mục | Nội dung |
|---|---|
| `brain-api/` | Go: MCP server (`internal/mcpserver`), OAuth 2.1 AS, scheduler leader, CLI quản trị (`cmd/brain-api`) |
| `ai-worker/` | Python (`biva_worker`, uv): runner queue Postgres, ingest (Gemini), index (TEI bge-m3) |
| `contracts/` | dùng chung Go ⇄ Python: migrations, JSON schema, fixtures textnorm/keys/item_search, `llm/llm.yaml` |
| `kb/` | tri thức nền L0/L1 (YAML, review bằng PR): rules, template ngành, `entities.yaml` (alias) |
| `deploy/` | docker-compose (Postgres pgvector, Redis, SeaweedFS S3, TEI CPU) + `smoke.sh` |

Package Go chính: `recall` (semantic + keyword, RRF, `query_data`, `get_source`, overview), `pack` (knowledge pack,
cache theo `knowledge_versions`), `artifact` (version, trích dẫn `[[uuid]]`, `list_stale`), `validate` (kiểm tĩnh),
`export` (snapshot), `coverage` (độ phủ + câu hỏi), `form` (link `/f/<token>` cho nhà xe), `entity` (alias),
`review` (diff/propose/apply), `kb` (load + sync), `queue`, `authz`, `oauth`, `textnorm`.

## Chạy và kiểm

```bash
make up            # compose đầy đủ; make smoke = dựng + kiểm luồng MCP → queue → worker (+ TEI)
make lint          # gofmt, go vet, kb check, ruff — CHẠY TRƯỚC MỖI COMMIT, đừng pipe qua tail (mất exit code)
export BIVA_TEST_DATABASE_URL=postgres://...   # Postgres có pgvector + btree_gist
cd brain-api && go test -p 1 ./...             # -p 1: các package dùng chung một DB test
cd ai-worker && uv run pytest -q
```

- Test Go dùng chung DB test: test tạo/xoá L0/L1 và rule locked → **không dùng DB test để demo**; tạo DB riêng
  (`brain-api migrate up` với `BIVA_MIGRATIONS_DIR=../contracts/migrations`, `kb sync`, `operator add`,
  `user add/grant`, `token issue`).
- Thử end-to-end bằng Claude Code: `claude -p "<prompt>" --mcp-config mcp.json --strict-mcp-config
  --allowedTools "mcp__biva__..."` với `mcp.json` = `{"mcpServers":{"biva":{"type":"http","url":
  "http://127.0.0.1:8080/mcp/operator/<id>/","headers":{"Authorization":"Bearer biva_..."}}}}`. Lấy nội dung
  prompt MCP (`build_bot`, `process_update`, `refresh_bot`, `onboard_operator`) qua `prompts/get`.
- CI (`.github/workflows/ci.yml`): lint, test (Postgres + Redis), compose-smoke (TEI thật).

## Quy ước và quyết định đã chốt

- Migration mới = số kế tiếp trong `contracts/migrations` (+ down); cập nhật số version trong
  `brain-api/internal/migrate/*_test.go`.
- Logic dùng chung Go/Python đặt trong SQL (vd `apply_review`) hoặc fixture `contracts/` mà cả hai bên test.
- Ingest: AI phía builder tự đọc nguồn (Excel, ảnh, Zalo) → `submit_knowledge` (đường chính, Brain không gọi
  LLM); nội dung thô → `ingest` (Gemini qua `ai-worker/biva_worker/llm`). Brain không tự parse file.
- Rủi ro: chỉ NEW ở topic không đụng tiền/giờ và DUPLICATE được tự apply; CHANGE/REMOVE/CONFLICT và topic
  fare/schedule/cancellation/payment luôn chờ builder duyệt (`apply_review` 2 bước, confirm_token).
- Trích dẫn trong artifact là `[[<uuid item>]]`; thông lệ L1 phải gắn nhãn; rule locked phải có trong
  system_prompt; không ghi cứng giá/giờ (bot gọi tool); faq/fallbacks là lời gửi khách nguyên văn.
- Stale: trigger hoãn tới commit (migration 000011) đánh dấu bản mới nhất của artifact; `expire_items` mỗi phút.
- Bí mật (Gemini key, token) chỉ qua biến môi trường — không ghi vào repo/log. Đường dẫn `/f/` không vào trace.
- Không thêm tên/ID model AI vào commit, code, docs.

## Trạng thái (01/10/2026)

- M0, M1 xong phần code (CI xanh). Còn chờ chủ dự án: dữ liệu pilot thật + golden set (DYN-110/112), duyệt
  `kb/` (DYN-111) và `kb/L1/xe-khach/entities.yaml` (DYN-113), URL https để thử ChatGPT connector (DYN-115).
  DYN-65 (prompt build_bot/process_update) In Progress tới khi chạy trên pilot #1 thật.
- M2 đã làm: stale/expire/list_stale/refresh_bot (E2.3), export_bot + lưu object storage `download_url`
  (E2.4), coverage/generate_questions/form/onboard_operator + `refresh_pages` (E2.1 — 5 trang Operator Profile
  `biva://operator/{id}/pages/<slug>.md`, job scheduler mỗi phút theo version tri thức),
  entity alias (E2.2 — thiếu trigram/co-occurrence + đo ≥95%, DYN-70).
- E2.5 tri thức logic v1 **đã chốt: repo riêng `github.com/trieungoctam/biva-integrations`** (clone ở
  `../biva-integrations`). S2.5.1 xong: schema hợp đồng `contracts/schemas/logic/{module,profile,cases}.schema.json`
  (fixture test cả Go ⇄ Python) + 3 module chuẩn đầu tiên (`fare.standard`, `booking.hold`,
  `schedule.sync_excel`, CI riêng chạy cases). S2.5.2 xong: job `index.code` (ai-worker `logic_sync.py`)
  sync repo → `logic_modules/logic_profiles/logic_param_sources/logic_tests/code_chunks/logic_syncs`
  (migration 000015); chunk theo hàm/lớp gắn commit (commit mới xoá chunk cũ); no-op theo HEAD;
  scheduler tick mỗi phút khi đặt `BIVA_INTEGRATIONS_REPO`; profile/case của nhà xe chưa onboard bị
  bỏ qua và retry ở tick sau. S2.5.3 xong: seed 16 feature `kb/L1/xe-khach/features.yaml`
  (schema kb/features + fixture 2 phía; `kb sync` upsert `logic_features`, rời bundle → deprecated) +
  job `logic.spec` (ai-worker `logic_spec.py`, LLM purpose `logic`): ánh xạ tri thức đã duyệt vào feature
  catalog → `logic_specs` (migration 000016), feature lạ → `proposed_features` chờ review; embedding
  rules_text qua TEI (optional). S2.5.4 xong: `internal/logic` + tools MCP `get_operator_logic`,
  `get_logic_spec`, `find_similar_operators` (0.5 feature IDF + 0.3 rules + 0.2 params, giải thích
  trùng/thiếu/khác + param lệch), `compare_logic`. S2.5.5–5.7 xong: `plan_logic_implementation` (bậc thấp nhất đủ
  dùng, params_draft), `record_decision` (ADR, bảng logic_decisions — migration 000017),
  `propose_logic_profile` (job `logic.propose`: validate + custom chặn khi thiếu ADR; có
  BIVA_INTEGRATIONS_TOKEN thì tạo PR, không thì trả nội dung tạo PR tay), `NO_CAPABILITY` trong
  validate_artifact (tool khai báo `capability:` trong tool_spec phải có profile active),
  prompt `/implement_operator_logic`. E2.5 đủ nội dung M2 — đo trên pilot thật chờ DYN-110.
- M3 đã làm (S3.1.1 graph + S3.1.2 temporal): migration 000018 (`entities.ext_id`, `item_entities`);
  `kb sync` điền 39 entity L1; job `index.items` nhận diện entity trong item text (ranh giới token);
  recall thêm 2 nhánh vào RRF — `graph` (query nhắc thực thể) và `temporal` (có valid_at rõ ràng →
  item mùa hẹp chứa ngày đi thắng item quanh năm). AC đo trên golden set chờ DYN-112. S3.1.3 rerank xong: `recall.TEIRerank`
  (/rerank bge-reranker-v2-m3), top ≤50 sau RRF, ngân sách 80ms, quá hạn/lỗi → giữ RRF + `degraded`;
  bật bằng `BIVA_RERANK_URL` (compose: cùng profile rerank).
- M3 E3.2.1+E3.2.2 xong: migration 000019 (`observation_sources`, review nhận `PROMOTE`); job
  `consolidate` (gom item active theo scope+topic thành `kind=observation`, 1 facet/observation,
  near-dup bỏ trùng, nguồn ghi observation_sources; observation đã lọc khỏi pack/recall) — enqueue
  tự động sau mỗi apply_review; job `promote` (scheduler 5 phút/lần): cụm observation giống nhau
  (cosine ≥0.7 / jaccard câu ≥0.45×0.9) ở ≥3 nhà xe → review PROMOTE kèm danh sách nhà xe, duyệt
  qua apply_review → item L1 active. S3.2.3 + S3.4.3 xong: migration 000020
  (`logic_families`, `logic_similarity`); job promote gom họ logic (union-find theo trùng feature
  ≥ 0.6, centroid = feature chung, recommended = mode phổ biến nhất; `promote_candidate` khi ≥ 3 nhà xe
  hook/custom) + cache điểm tương đồng mọi cặp; tool `list_logic_families`. Phần tương đồng code
  (embedding chunks) và behavior bổ sung khi có dữ liệu nhiều nhà xe.
- M3 E3.3 xong (CONTRADICTION): job `validate` (ai-worker `validate_llm.py`, LLM purpose
  `validate`) chạy nền sau mỗi validate_artifact PASS — Go enqueue tự động; detect mâu thuẫn
  fact (chỉ nhận confident), ghi lỗi CONTRADICTION + dòng + item, hạ valid→invalid, có audit;
  không mâu thuẫn → đánh dấu đã kiểm. Live test Gemini thật (skip CI) đo recall ≥90% trên 10
  case cài sẵn (5 mâu thuẫn / 5 hợp lệ); AC đo đầy đủ cần GEMINI key thật.
- Linear: workspace dpos, project "BIVA Brain"; mỗi story xong thì comment kết quả + link CI rồi chuyển Done
  (chưa đạt hết AC thì để In Progress và ghi rõ phần thiếu).
