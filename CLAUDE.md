# BIVA Brain — ghi chú cho Claude Code

Brain tri thức (kiểu Hindsight) để build chatbot cho nhiều nhà xe khách. Builder dùng AI client (ChatGPT
connector, Claude Code / coding agent) qua MCP để nạp tri thức nhà xe và viết bot. Chủ dự án: Triệu Ngọc Tâm.
Trao đổi với chủ dự án bằng **tiếng Việt**; comment code, docs, thông báo lỗi cho AI cũng viết tiếng Việt.
Commit message tiếng Anh.

Tài liệu thiết kế: `docs/architecture.md`, `docs/mcp.md` (danh mục tool, luật trích dẫn), `docs/data-model.md`,
`docs/implementation-plan.md` (milestone M0–M5, story S*.*.* ↔ issue Linear DYN-*).

## Security review trước pilot (02/10)

Agent security-review rà 7 mặt; phân quyền đa tenant, tham số hoá SQL, confirm_token, lõi OAuth
(PKCE, xoay refresh) **sạch**. Đã sửa 4/4 phát hiện nghiêm trọng:
- HIGH sandbox: builtin `io`/`_io` lọt purge/deny → code không tin cậy đọc secret/ghi đè
  sandbox_worker.py — đã purge+deny (io, marshal, faulthandler, zipimport, runpy…), stub `io.open`,
  thêm RLIMIT_FSIZE 1MB + CORE 0; verify `io.open` đọc/ghi đều SANDBOX_BLOCKED.
- MED git: token nhúng URL push lộ qua stderr khi lỗi — chuyển sang `http.extraheader`, lọc token
  khỏi thông báo lỗi; workdir trả về nhánh gốc sau push (chế độ path không đọc nhánh chưa duyệt).
- MED approve_publish: token gắn subject "*" — nay bắt buộc release_id ở preview, token hash đúng
  release đó (test: token của r1 duyệt r2 bị chặn).
- MED examples: entrypoint/hooks chứa ".." đọc file ngoài repo — resolve + is_relative_to(workdir).
Vòng 3 (review chất lượng, agent reviewer 22 finding): đã sửa 4 high + 3 leak/medium —
(a) schema operation_result từng additionalProperties:false khiến job bot.tests/bot.chat/index.code
v.v. bị đánh failed dù handler chạy xong (mở rộng schema theo kết quả thật + test hồi quy);
(b) Publish/Approve/Rollback từng 3 lệnh autocommit riêng (A/B đua nhau → cả hai rolled_back) —
giờ 1 transaction + pg_advisory_xact_lock theo (operator, kênh);
(c) gate phát hành từng chấp nhận test run của snapshot khác bản phát hành — bot.tests giờ ghi
snapshot_id, gate chỉ nhận test của đúng snapshot mới nhất (snapshot mới phải test lại);
(d) lỗi DB khi ghi kết quả job từng giết slot worker vĩnh viễn — giờ bắt ở ranh giới slot;
(e) run_code/git-clone đồng bộ từng chặn event loop → hết lease 60s bị claim lại — sang to_thread;
(f) clone lỗi để lộ tmpdir; (g) goroutine ticker scheduler tích luỹ qua từng phiên leader.
Vòng 11 (review lại chính các bản fix — security-reviewer trên diff cbe11c9..HEAD): SỬA 1 high
thật — vòng fix sandbox vòng 1 KHÔNG đậu: purge list không chứa io/_io/_socket (replace không
khớp chuỗi sau format), và UNDER_DENY chỉ áp nhánh non-builtin nên `import _io; _io.open()`
vẫn đọc file thật, `_socket.socket()` vẫn mở mạng. Hướng chặn đúng: builtin DENY-BY-DEFAULT
(BUILTIN_OK chỉ builtin tính toán) + purge io/_io/marshal/_socket/pickle/ctypes… khỏi
sys.modules + NẠP SẴN whitelist trước khi đăng ký finder (machinery đọc .py qua _io — không
nạp trước là vỡ import hợp lệ; whitlist đóng băng: module ngoài không nạp thêm được).
Corpus tấn công cố hóa 6 đường + test whitelist-full. Kèm: Publish chốt snapshot dưới advisory
lock (chống TOCTOU export_bot chen snapshot chưa test), Approve theo thứ tự advisory→row,
list_stale JOIN ràng operator (chặn leak chéo tenant qua logic_param_sources + logic_sync kiểm
ownership khi ghi — bộ test cũ tự dùng nguồn chéo, đã sửa fixture), OAuth resource bắt buộc
(RFC 8707). SLO đo lại sau 36 fix: recall p95 8,8ms — không suy giảm.

Tập dượt /refresh_bot (03/10): ĐẠT — stale reason JSON đủ để builder sửa không đoán
(line, line_text kèm trích dẫn cũ, new_text, superseded_by); làm theo prompt (get_artifact →
sửa đúng dòng → save base_version + note → validate) cho valid ngay và list_stale về 0.
Tình trạng tập dượt prompt: build_bot ✓ · refresh_bot ✓ · process_update ✓ (flow kit r16) ·
onboard_operator ✓ (chuỗi r22) · còn lại review_quality (RO) và implement_operator_logic
(các bước riêng đã test r12).

Tập dượt /build_bot (DYN-65, 03/10): đóng vai AI builder làm theo prompt từng bước trên stack
local — KẾT QUẢ ĐẠT (5 artifact valid + snapshot v1). Học được (đã sửa/nắm pattern): mọi câu
thông tin phải có [[id]] kể cả câu vai trò (tạo item kind=persona rồi trích); fallbacks giữ
chung chung "Chưa rõ: chuyển nhân viên" (nhắc chủ đề cụ thể = bị coi là mang thông tin);
hướng dẫn tool nằm ở tool_spec. Fix kèm: save_artifact với base_version trên kind CHƯA TỪNG
tồn tại từng báo "đã có version mới hơn (v0…)" lạc hướng — giờ báo "chưa có artifact … bỏ
base_version" (test hồi quy).

Vòng 20–22 (audit/visibility + form không-LLM): release.publish/approve/rollback và oauth.
register/token/refresh/family_revoke giờ ghi audit_log (test assert đủ); token SAI (401) ghi
WARN log kèm IP/path (không đụng DB — chống khuếch đại khi bị quét). Form submit trích
DETERMINISTIC: câu trả lời → item policy theo topic của câu hỏi (key topic.q_<token câu hỏi>)
— vòng thu thập DYN-110 (coverage → questions → form → items) chạy trọn KHÔNG cần GEMINI key;
nội dung thô vẫn giữ trong payload.content để duyệt/ingest LLM sau này.

Vòng 8–9 (verify + drift): demo E2E pass trên stack code hiện tại (migrate 24/25 trên DB sống);
test concurrency thật (8 publish song song → đúng 1 published; 2 consolidate song song → 0 câu mất);
review repo biva-integrations (sửa alias columns không strip, lỗi child_policy rõ ràng). Vòng 10 (drift
docs/tool-desc): approve_publish + check_release_gate mô tả lại đúng flow hiện tại (token gắn release,
test phải của đúng snapshot); list_stale BỔ SUNG logic_profiles — docs hứa từ E2.5 mà code chưa trả:
giờ mỗi tham số stale hiện capability/param/item/replaced_by/new_item_text kèm next_actions.

Vòng 7 (2 mục cuối của 22 finding review chất lượng — chủ dự án "tiếp tục" = chốt hướng bảo thủ):
migration 000025 — propose_l1_change giờ GÁN target_item_id khi key L1 đang có bản active,
apply_review supersede bản cũ như nhánh CHANGE của L2 (trước đây apply đụng unique
items_platform_key_active → không sửa được thông lệ đang có); guard no-target cũng nhìn key
L1 active. Ingest: document + review + auto-apply + enqueue index.items nằm trong MỘT
transaction (trước đây _write commit riêng rồi mới auto-apply/enqueue — chết giữa hai khối để
lại document dở mà retry trả "tin đã ingest"); test rollback bằng proxy raise ở enqueue.

Vòng 6 (2 medium nữa — thực ra deterministic, không cần golden set): target review/ingest giờ
chọn bản active có khoảng hiệu lực CHỨA thời điểm đề xuất (mặc định hôm nay) thay vì bản
valid_from lớn nhất — sửa giá đang áp dụng khi đã có bản Tết lên lịch không còn nhắm nhầm/
vi phạm items_scope_key_validity (review.go + ingest load_existing trả mọi bản active theo key,
diff.pick theo khoảng; test Go review_test + Python test_ingest). Promote L1 giờ chỉ đề xuất
các CÂU được ≥3 nhà xe cùng nói (giao theo textnorm.fold) — câu riêng nhà xe đại diện không
còn lên thông lệ ngành (_common_sentences + test).

Vòng 5 (3 medium cuối cùng sửa được không cần golden set): migration 000024 — item nguồn của
tham số profile (logic_param_sources) bị supersede/expire/rút giờ đánh profile `stale` ngay
trong transaction (mở rộng items_mark_stale; logic_sync upsert hồi phục 'active' khi merge PR —
test hồi quy test_logic_sync); promote chỉ gộp observation CÙNG topic + dedupe operators
(test: 2 nhà xe topic này + 1 nhà xe topic kia không thành đề xuất); operator_pages outdated
thêm điều kiện ngày VN (item hiệu lực theo ngày không bump version — Refresh chọn lại nhà xe
có trang dựng trước 00:00 VN, Read không dùng cache cũ, sameVNDay).

Vòng 4 (sửa nốt medium máy móc được): khoảng hiệu lực đổi sang NỬA MỞ [valid_from, valid_to)
trừ 4 chỗ Go (`valid_to > T`) + executor Python (cast ngày theo Asia/Ho_Chi_Minh, ngày mặc định
giờ VN thay vì UTC container — test hồi quy cả hai phía); consolidate lấy advisory lock theo
scope (chặn lost-update mất câu đã đánh dấu consolidated); recall load lọc lại status='active'
(item superseded giữa chừng không trả về); logic Overview chỉ nuốt đúng ErrNoRows/ErrNoSpec,
lỗi DB khác báo lên (builder không bị báo "chưa có" khi DB lỗi); index.code HEAD không đổi vẫn
hoàn thiện embedding chunk thiếu (TEI hồi phục là tự chữa); Similar/Compare nạp embedding phía
spec mình — cosine chạy thật thay vì luôn fallback Jaccard.
Review chất lượng 22/22 finding đã xử lý hết (vòng 3–7).

Vòng 2 (sau review) sửa nốt 3 low/medium còn lại: export key thêm nonce (URL không đoán được),
resource OAuth whitelist chặt (chặn `/mcp/operator/` rỗng + `../`), register hỗ trợ
`BIVA_OAUTH_REGISTRATION_SECRET`, consent hiển thị client_id. Ghi hướng dẫn trong runbook.

## Đo SLO (E-X2 cơ bản)

`deploy/slo.sh` đo p50/p95 các tool chính so mục tiêu §7.3 runbook. Kết quả 02/10/2026 trên stack
local (TEI off — chỉ keyword+graph+temporal): recall p95 10ms (<150), pack p95 4ms (<800),
validate tĩnh p95 3ms (<300); update rủi ro thấp → stale 1.9ms (trigger, trong transaction — AC <1 phút).

## Demo nhanh (stack local)

```bash
# token demo (ops = mọi nhà xe, lead = platform) — tạo 1 lần:
C='docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.local.yml exec -T brain-api brain-api'
$C operator add demoa "Demo A" && $C user add demoops --email demo@biva.vn --name "Demo Ops" --role ops \
  && $C user grant demoops demoa
$C token issue demoops --name demo

# chạy chuỗi promote toàn cục (không cần GEMINI key):
BIVA_API=http://localhost:7788 BIVA_DEMO_OPS_TOKEN=… BIVA_DEMO_LEAD_TOKEN=… deploy/demo.sh
```

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
- **Sửa hổng kiến trúc (migration 000023)**: enqueue consolidate sau apply nằm TRONG hàm SQL
  `apply_review` — mọi đường apply (MCP lẫn auto-apply rủi ro thấp của worker) đều chạy consolidate;
  bỏ enqueue cũ phía Go. Đã kiểm trên stack: submit_knowledge → worker tự apply → consolidate tự chạy.
- **M5** platform tools: `list_operators`, `list_promotion_candidates`, `propose_l1_change`,
  `impact_of_change`, `run_regression_all`. **Danh mục tool MCP đầy đủ 46/46, resource 9/9** —包括 2 tool sót
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
