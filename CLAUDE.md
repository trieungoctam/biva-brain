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
- Việc tiếp theo theo kế hoạch: **E2.5 tri thức logic v1** (repo `biva-integrations`, module.yaml/profile.yaml,
  index_code, feature catalog, find_similar_operators, plan/propose logic, NO_CAPABILITY,
  /implement_operator_logic) — cần chốt với chủ dự án repo logic nằm ở đâu trước khi làm.
- Linear: workspace dpos, project "BIVA Brain"; mỗi story xong thì comment kết quả + link CI rồi chuyển Done
  (chưa đạt hết AC thì để In Progress và ghi rõ phần thiếu).
