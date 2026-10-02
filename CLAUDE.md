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

## Trạng thái (02/10/2026) — M0–M5 HẾT STORY CODE

Toàn bộ story code của lộ trình M0–M5 đã xong, CI xanh (Go 21 pkg + Python 351 test + compose-smoke).
Chi tiết từng story nằm trong comment các issue Linear (kèm link CI). Tóm tắt theo epic:

- **M0–M1** nền móng + tri thức + build v1 (queue, MCP + auth, ingest/review, recall, knowledge pack,
  artifact + validate tĩnh, prompts).
- **M2** onboarding (coverage/questions/form/`refresh_pages`), stale → refresh, export lên object storage,
  **tri thức logic trọn E2.5**: repo riêng `github.com/trieungoctam/biva-integrations` (clone `../biva-integrations`)
  + schema hợp đồng `contracts/schemas/logic/*`, job `index.code` sync repo, feature catalog L1
  (`kb/L1/xe-khach/features.yaml`), job `logic.spec`, tools `get_operator_logic/get_logic_spec/
  find_similar_operators/compare_logic/plan_logic_implementation/record_decision/propose_logic_profile`,
  NO_CAPABILITY, prompt `/implement_operator_logic`.
- **M3** recall graph/temporal/rerank; consolidate + promote tri thức & logic (PROMOTE qua review,
  logic_families); CONTRADICTION (LLM nền, live test chờ key); sandbox chống mạng; `run_examples_against`;
  add/list_logic_tests; reference executor + `run_tests`/`sandbox_chat` + sinh test; `reflect` +
  `compare_with_industry`.
- **M4** `check_release_gate`; `request_publish` (staging tự động, production chờ lead) +
  `rollback_release` < 1 phút; `add_lesson` + prompt `/review_quality`; bộ test chống prompt injection.
- **M5** platform tools: `list_operators`, `list_promotion_candidates`, `propose_l1_change`,
  `impact_of_change`, `run_regression_all`. **Danh mục tool MCP đầy đủ 45/45** —包括 2 tool sót
  `search_logic` (module/feature/code chunk/lesson, có dấu-không dấu) và `get_logic_module`
  (manifest đầy đủ) vừa bổ sung.

Việc còn treo (đều ngoài code — cần chủ dự án):
1. **Dữ liệu pilot thật + golden set** (DYN-110/112) — đo mọi AC số liệu (recall, ≥90% parse, ≥95% alias,
   spec 3 pilot, demo M2/M3).
2. **GEMINI key** (`BIVA_TEST_GEMINI_API_KEY`) — chạy live test CONTRADICTION (recall ≥90%) và đo
   executor/reflect thật.
3. **Máy amd64 hoặc RAM Docker >10GB** — TEI (semantic + rerank) không chạy được trên laptop này (OOM).
4. **URL https công khai** (DYN-115) — thử ChatGPT connector (OAuth đã có).
5. **S5.2 vận hành**: runbook đầy đủ `docs/runbook.md` (8 sự cố theo system-architecture §7.1 +
  backup); `deploy/backup.sh` backup Postgres + `BIVA_RESTORE_VERIFY=1` khôi phục thử (đã chạy OK:
  39 bảng / 26 items / 3 operations khớp) — tự dùng client trong container khi host thiếu pg tools;
  PITR + Helm/KEDA theo cắt giảm (compose 1 VM đến khi cần scale).
6. **RLS (S4.4.1)** — hoãn có chủ đích: app đang chạy role owner nên RLS sẽ bị bypass (giả an toàn);
   làm đúng cần role riêng + SET LOCAL mỗi request (thay đổi kiến trúc connection).
6. Khi có pilot: chạy `/onboard_operator` → `/build_bot` → `run_tests` → `check_release_gate` →
   `request_publish`; DYN-65/70/77… chuyển Done khi AC đo được.

- Linear: workspace dpos, project "BIVA Brain"; mỗi story xong thì comment kết quả + link CI rồi chuyển Done
  (chưa đạt hết AC thì để In Progress và ghi rõ phần thiếu).
