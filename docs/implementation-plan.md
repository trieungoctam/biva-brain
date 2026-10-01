# Kế hoạch triển khai

Kế hoạch triển khai BIVA Brain theo **milestone → epic → story**. Bám theo lộ trình M0–M5 trong
[architecture.md §8](architecture.md#8-lộ-trình). Mỗi milestone kết thúc bằng một **demo trên nhà xe thật (pilot)**.

**Theo dõi trên Linear**: project [BIVA Brain](https://linear.app/dpos/project/biva-brain-0c5d0e4d0b0d) (team Build) —
milestone M0–M5, epic là issue cha `[E…]`, story là sub-issue `[S…]` (DYN-5 → DYN-114).

## 0. Giả định

| Mục | Giả định (điều chỉnh khi chốt nhân sự) |
|---|---|
| Team | 2 Go · 2 Python · 1 Frontend (từ M4) · 1 Tech lead/kiến trúc · 1 vận hành tri thức (soạn L0/L1, làm việc với nhà xe) |
| Nhịp | sprint 2 tuần; demo cuối mỗi milestone |
| Pilot | **3 nhà xe** đại diện: 1 nhà xe "cơ bản", 1 nhà xe "có mùa lễ", 1 nhà xe có logic đặc biệt |
| Môi trường | không GPU; LLM qua thư viện; Postgres + Redis + MinIO + TEI (CPU) |
| Ước lượng | tính bằng tuần-người (tw), chỉ để sắp xếp; đo lại sau M1 |

**Ký hiệu**: `Go` / `Py` / `FE` / `Ops` = người phụ trách chính. **AC** = tiêu chí chấp nhận.

## 1. Tổng quan

| Mốc | Mục tiêu | Thời lượng | Demo kết thúc |
|---|---|---|---|
| **M0** Nền móng | repo, contracts, schema, queue, skeleton 2 service, CI, local dev | 2 tuần | AI client kết nối MCP stub; job đi từ Go → Postgres → Python và quay về |
| **M1** Tri thức + build v1 | ingest, review, recall, knowledge pack, artifact có trích dẫn, validate tĩnh | 5 tuần | AI viết **system prompt + FAQ hợp lệ** cho pilot #1 từ tin Zalo + Excel thật |
| **M2** Onboarding, logic, quên | coverage, câu hỏi, entity, tri thức logic v1, stale + refresh, export | 5 tuần | Pilot #2 onboard; đổi giá → đúng đoạn bot bị stale → `/refresh_bot`; tìm nhà xe tương tự theo spec |
| **M3** Trí tuệ | graph/temporal, rerank, consolidate, promote, validate LLM, sandbox, test | 5 tuần | Pilot #3: `run_examples_against` chọn đúng điểm xuất phát; `run_tests` + sandbox UAT |
| **M4** Phát hành & chất lượng | release gate, publish/rollback, lessons, console duyệt, bảo mật | 3 tuần | Bot pilot qua gate, lead duyệt trên console, export bản phát hành |
| **M5** Platform | quản lý L0/L1, promote toàn cục, regression toàn bộ | 3 tuần | Promote một thông lệ + một hook dùng chung từ 3 pilot, regression chạy trên mọi bot |
| Sau | Runtime Integration API, feedback từ hội thoại thật | — | — |

Tổng: **~23 tuần** (≈ 5,5 tháng). Đường găng: M0 → schema/queue → ingest → knowledge pack → artifact/validate → stale → test.

```
M0 ──► M1 ──► M2 ──► M3 ──► M4 ──► M5
        │       │       │
        │       │       └─ cần: logic spec (M2), reference executor
        │       └─ cần: artifact + citations (M1)
        └─ cần: schema, queue, textnorm (M0)
Song song từ M0: E-X1 tri thức L0/L1 + golden set (Ops) · E-X2 quan sát · E-X3 bảo mật
```

## 2. Chi tiết theo milestone

### M0 — Nền móng (2 tuần)

| Epic | Story | Phụ trách | AC |
|---|---|---|---|
| **E0.1 Repo & contracts** | S0.1.1 Monorepo `contracts/ go/ python/ console/ deploy/ docs/` | Lead | cấu trúc như architecture.md §7; README chạy local |
| | S0.1.2 Migration nền (golang-migrate): operators, bots, documents, items, entities, operations, audit_log | Go | `migrate up/down` sạch trên DB trống |
| | S0.1.3 JSON Schema đầu tiên: payload job `ingest`, kết quả operation | Lead | schema có test ở cả Go và Py |
| | S0.1.4 Fixture chung `textnorm` (chuẩn hoá tiếng Việt, không dấu, bigram) | Go + Py | Go và Py cho kết quả **giống hệt** trên ≥ 200 câu fixture |
| **E0.2 Queue Go ⇄ Python** | S0.2.1 `operations`: enqueue idempotent, NOTIFY | Go | enqueue trùng `idempotency_key` → 1 job |
| | S0.2.2 Runner Python: claim SKIP LOCKED, lease, heartbeat, retry backoff | Py | kill worker giữa chừng → job chạy lại sau khi hết lease |
| | S0.2.3 Scheduler (leader qua advisory lock): requeue job hết lease | Go | 2 instance brain-api → chỉ 1 leader |
| **E0.3 brain-api skeleton** | S0.3.1 HTTP server, health/ready, config, pgxpool (primary/replica) | Go | `/health/ready` kiểm DB |
| | S0.3.2 MCP server stub `/mcp/operator/{id}/` (go-sdk), 1 tool `get_operation` | Go | Claude Code kết nối, gọi được tool |
| | S0.3.3 Auth: token cá nhân cho MCP, role builder/lead/ops, scope operator theo URL | Go | token sai scope → bị từ chối; audit ghi actor |
| **E0.4 Dev & CI** | S0.4.1 docker-compose: Postgres(pgvector), Redis, MinIO, TEI CPU, 2 service | Ops | `make up` chạy đủ trên laptop |
| | S0.4.2 CI: lint, unit, contract test (fixture chung), migration check | Lead | PR đỏ khi fixture Go/Py lệch |
| | S0.4.3 OpenTelemetry cơ bản + trace context qua `operations` | Go + Py | 1 trace nối MCP call → job Python |

**Exit M0**: demo "job đi một vòng" + MCP kết nối từ Claude Code; CI xanh.

### M1 — Tri thức + build v1 (5 tuần)

| Epic | Story | Phụ trách | AC |
|---|---|---|---|
| **E1.1 Ingest** | S1.1.1 Thư viện `llm/` (Py): llm.yaml, fallback, quota Redis, đo chi phí | Py | provider chính lỗi → tự chuyển; metrics có `operator_id` |
| | S1.1.2 Parser Zalo text → item (LLM, structured output, giữ tiếng Việt) | Py | ≥ 90% item đúng trên bộ 50 tin Zalo của pilot (golden set) |
| | S1.1.3 Parser Excel giá/lịch → data vận hành + item | Py | file Excel của 3 pilot parse đúng 100% dòng hợp lệ; dòng lỗi được báo |
| | S1.1.4 Index: `search_text` + embedding qua TEI (batch) | Py | item mới tìm được bằng cả có dấu và không dấu |
| **E1.2 Diff & review** | S1.2.1 Diff theo `key`: NEW/CHANGE/REMOVE/DUPLICATE/CONFLICT | Py | đổi giá trong Excel → 1 CHANGE đúng trước/sau |
| | S1.2.2 Review queue + rủi ro (giá/giờ/huỷ = high) | Go | item high không bao giờ tự apply |
| | S1.2.3 Apply trong 1 transaction: supersede, version data vận hành | Go | bản cũ `superseded_by` trỏ đúng; data vận hành có version mới |
| | S1.2.4 Tool MCP: `ingest`, `list_review_queue`, `get_review_item`, `apply_review` (preview + confirm_token), `propose_item` | Go | apply không có token hợp lệ → bị từ chối |
| **E1.3 Recall & data** | S1.3.1 Recall Go: semantic + keyword song song, RRF, pack theo token, lọc `active` + `valid_at`, boost tầng | Go | p95 < 150 ms trên dữ liệu 3 pilot; kết quả có nhãn tầng + nguồn |
| | S1.3.2 Tool `recall_knowledge`, `query_data`, `get_source`, `get_operator_overview` | Go | — |
| **E1.4 Knowledge pack** | S1.4.1 Merge tầng (default/locked/nhãn L1), lọc hiệu lực, cắt theo `budget_tokens` | Go | rule locked luôn có mặt; mục chỉ có L1 luôn mang nhãn "thông lệ chung" |
| | S1.4.2 Cache theo version tri thức của scope; tool `get_knowledge_pack` | Go | p95 < 800 ms; đổi tri thức → cache vô hiệu |
| **E1.5 Artifact & validate tĩnh** | S1.5.1 Bảng `bot_artifacts`, `artifact_citations`; `save_artifact` (version mới, không ghi đè) | Go | — |
| | S1.5.2 Parser trích dẫn `[[item_id]]` + validate tĩnh: UNCITED, STALE_CITATION, MISSING_LOCKED, UNLABELED_DEFAULT, HARDCODED_DATA, COVERAGE | Go | mỗi mã lỗi có test; lỗi có dòng/vị trí |
| | S1.5.3 `get_bot_spec` từ template L1 + L0 | Go | — |
| **E1.6 Workflow AI** | S1.6.1 Server instructions + resources (`pages`, `industry/template`, `guides/citation`) | Lead | — |
| | S1.6.2 Prompts `/process_update`, `/build_bot` | Lead | builder dùng Claude Code làm xong pilot #1 không cần gọi tool thủ công |

**Exit M1**: với pilot #1, từ tin Zalo + Excel thật → review → AI viết `system_prompt` + `faq` → `validate_artifact` 0 lỗi.

### M2 — Onboarding, tri thức logic, quên (5 tuần)

| Epic | Story | Phụ trách | AC |
|---|---|---|---|
| **E2.1 Onboarding** | S2.1.1 Coverage theo template L1 (bắt buộc/khuyến nghị/thiếu/mơ hồ/dùng L1) | Go | trang "Còn thiếu" khớp coverage |
| | S2.1.2 `generate_questions` ưu tiên theo độ khác nhau giữa nhà xe | Py | mục ít nhà xe lệch → câu "xác nhận nhanh" |
| | S2.1.3 Form trả lời cho nhà xe → ingest | Go + FE nhẹ | trả lời form xuất hiện trong review queue |
| | S2.1.4 Prompt `/onboard_operator`; `refresh_pages` | Lead + Py | — |
| **E2.2 Entity** | S2.2.1 Từ điển alias L1 (bến, tỉnh, loại xe) + trigram + co-occurrence | Py | "SG", "Sài Gòn", "TP.HCM" → 1 entity; ≥ 95% đúng trên golden set |
| **E2.3 Quên → stale** | S2.3.1 `mark_stale` theo `artifact_citations` / `logic_param_sources` | Py | đổi giá → đúng đoạn trích dẫn bị stale trong < 1 phút |
| | S2.3.2 Job `expire` theo `valid_to` | Go | lịch Tết tự hết hiệu lực, artifact liên quan stale |
| | S2.3.3 `list_stale` + prompt `/refresh_bot` | Go + Lead | AI chỉ sửa đoạn bị ảnh hưởng, artifact khác giữ nguyên version |
| **E2.4 Export** | S2.4.1 Lắp snapshot từ artifact valid; `export_bot` json · markdown · faq_csv | Go | snapshot không nhận artifact stale/invalid |
| **E2.5 Tri thức logic v1** | S2.5.1 Quy ước repo `biva-integrations` + `module.yaml` / `profile.yaml` / `tests/cases.yaml` (schema) | Lead | 3 module chuẩn đầu tiên: `fare.standard`, `booking.hold`, `schedule.sync_excel` |
| | S2.5.2 `index_code`: đồng bộ manifest, hồ sơ, ví dụ; code chunk theo hàm/lớp gắn commit | Py | merge PR → Brain cập nhật < 5 phút |
| | S2.5.3 Feature catalog L1 (seed) + `extract_logic_spec` từ tri thức | Py + Ops | spec của 3 pilot dựng được trước khi có code |
| | S2.5.4 `find_similar_operators` tầng spec (feature IDF + rule text + tham số), `compare_logic`, `get_logic_spec` | Go | kết quả có giải thích feature trùng/thiếu/khác |
| | S2.5.5 `plan_logic_implementation`, `propose_logic_profile` (tạo PR), `record_decision` (ADR) | Go | custom không có ADR → bị từ chối |
| | S2.5.6 Validate `NO_CAPABILITY`: tool trong `tool_spec` phải có capability active | Go | — |
| | S2.5.7 Prompt `/implement_operator_logic` (bản spec) | Lead | — |

**Exit M2**: pilot #2 onboard từ đầu bằng `/onboard_operator`; đổi giá Tết → stale đúng đoạn → `/refresh_bot`;
`find_similar_operators` cho pilot #2 trả về pilot #1 với giải thích hợp lý.

### M3 — Trí tuệ (5 tuần)

| Epic | Story | Phụ trách | AC |
|---|---|---|---|
| **E3.1 Recall nâng cao** | S3.1.1 Graph arm (entity, link) | Go | câu hỏi đa bước trên golden set cải thiện so với M1 |
| | S3.1.2 Temporal arm theo `valid_at` (ngày đi) + bucket | Go | "giá ngày 28 Tết" trả về giá Tết, không phải giá thường |
| | S3.1.3 Rerank TEI CPU (top 30–50, ngân sách 80 ms, fallback RRF) | Go | TEI chết → recall vẫn trả kết quả |
| **E3.2 Consolidate & promote** | S3.2.1 Consolidate theo scope (quy tắc Hindsight, history, near-dup 0.97) | Py | không có observation trùng lặp trên 3 pilot |
| | S3.2.2 Promote tri thức: ứng viên khi ≥ 3 nhà xe giống nhau, override, đề xuất sửa L1 | Py | đề xuất kèm danh sách nhà xe |
| | S3.2.3 Promote logic: gom custom/hook giống nhau (spec + code) | Py | — |
| **E3.3 Validate LLM** | S3.3.1 Kiểm mâu thuẫn artifact ↔ tri thức (CONTRADICTION), chạy nền | Py | bắt ≥ 90% mâu thuẫn cài sẵn trong bộ test |
| **E3.4 Sandbox & so hành vi** | S3.4.1 Sandbox chạy hàm thuần (không mạng, giới hạn CPU/RAM/thời gian) | Py | code cố gọi mạng → bị chặn |
| | S3.4.2 `run_examples_against` + % pass + case fail | Py | pilot #3 chọn đúng ứng viên có % pass cao nhất |
| | S3.4.3 Tương đồng code (embedding + cấu trúc), `logic_families`, `list_logic_families` | Py | — |
| | S3.4.4 `add_logic_test` / `list_logic_tests`; CI repo chạy `cases.yaml` | Py | — |
| **E3.5 Test bot** | S3.5.1 Reference executor (LLM + tool tra tri thức/data của Brain) | Py | chạy được snapshot của 3 pilot |
| | S3.5.2 Sinh test: regression L0/L1, từ policy, từ data (phải gọi tool), override, thiếu dữ liệu | Py | — |
| | S3.5.3 `run_tests`, `sandbox_chat`; 👎 → lesson L2 + test case; câu không trả lời được → knowledge gap | Py + Go | — |
| **E3.6 Reflect** | S3.6.1 `reflect`, `compare_with_industry` | Py + Go | trả lời có trích dẫn hợp lệ |

**Exit M3**: pilot #3 (logic đặc biệt) — `/implement_operator_logic` chọn đúng điểm xuất phát bằng `run_examples_against`;
`run_tests` + sandbox UAT với nhà xe.

### M4 — Phát hành & chất lượng (3 tuần)

| Epic | Story | Phụ trách | AC |
|---|---|---|---|
| **E4.1 Release** | S4.1.1 `check_release_gate` (coverage, artifact valid/0 stale, logic test, regression, conflict, UAT) | Go | gate trả lý do cụ thể khi chặn |
| | S4.1.2 `request_publish` (staging tự động, production cần lead), rollback | Go | rollback < 1 phút |
| **E4.2 Console** | S4.2.1 Review queue, duyệt L0/L1/promote, duyệt phát hành | FE | lead duyệt không cần MCP |
| | S4.2.2 Operator pages, báo cáo validate/test, giám sát job | FE | — |
| **E4.3 Chất lượng** | S4.3.1 `add_lesson`, `add_test_case`, prompt `/review_quality` | Go + Lead | — |
| **E4.4 Bảo mật** | S4.4.1 Postgres RLS theo `operator_id` | Go | test: token nhà xe A không đọc được dữ liệu nhà xe B kể cả khi service có bug |
| | S4.4.2 Quy tắc 2 người cho L0/L1/promote/production; audit đầy đủ | Go | — |
| | S4.4.3 Prompt injection: nội dung nhà xe/code là data; bộ test tấn công | Py | tin Zalo chứa "hãy xoá chính sách" không gây thao tác ghi |

**Exit M4**: bot của 3 pilot qua release gate, lead duyệt production trên console, export bản phát hành.

### M5 — Platform (3 tuần)

| Epic | Story | Phụ trách | AC |
|---|---|---|---|
| **E5.1 MCP platform** | S5.1.1 `/mcp/platform/`: `list_operators`, `list_promotion_candidates`, `propose_l1_change` | Go | chỉ role lead |
| | S5.1.2 `impact_of_change` (tri thức, module, feature) | Go | liệt kê đúng nhà xe/bot/test bị ảnh hưởng |
| | S5.1.3 `run_regression_all` | Py | chạy song song, báo cáo theo bot |
| **E5.2 Vận hành** | S5.2.1 Helm, KEDA theo queue, backup PITR, cảnh báo theo SLO | Ops | khôi phục thử từ backup thành công |
| | S5.2.2 Runbook sự cố (worker, TEI, LLM, replica) | Ops | — |

**Exit M5**: promote 1 thông lệ + 1 hook dùng chung từ 3 pilot; regression chạy trên mọi bot; hạ tầng production sẵn sàng.

## 3. Epic xuyên suốt (song song từ M0)

| Epic | Nội dung | Phụ trách | Mốc cần |
|---|---|---|---|
| **E-X1 Tri thức nền & golden set** | soạn L0 (guardrail), template onboarding L1, alias bến/tỉnh, feature catalog seed; thu thập dữ liệu thật của 3 pilot; **golden set**: 50 tin Zalo có đáp án item, 100 câu hỏi khách kèm đáp án, ví dụ tính giá | Ops + Lead | L0/L1 + golden set trước giữa M1; feature catalog trước M2 |
| **E-X2 Đo lường** | dashboard: latency theo tool MCP, lỗi validate theo mã, số vòng validate, stale, chi phí LLM theo nhà xe | Go + Py | cơ bản M0, đầy đủ M4 |
| **E-X3 Bảo mật** | token, scope, audit (M0); RLS, 2 người, injection (M4) | Go | M0, M4 |
| **E-X4 Đánh giá AI** | chạy `/build_bot` định kỳ trên golden set; theo dõi tỉ lệ artifact valid ở lần đầu, số vòng sửa, thời gian onboard | Lead | từ M1 |

## 4. Rủi ro

| Rủi ro | Ảnh hưởng | Giảm thiểu |
|---|---|---|
| Parse tin Zalo kém (viết tắt, không dấu, lẫn nhiều ý) | sai tri thức đầu vào | golden set từ sớm; review bắt buộc với mục rủi ro cao; đo độ chính xác mỗi sprint |
| Excel mỗi nhà xe một kiểu | parser vỡ | ánh xạ cột có cấu hình theo nhà xe; báo dòng lỗi thay vì đoán |
| AI không tuân thủ hợp đồng trích dẫn | validate fail lặp | lỗi có vị trí + `next_actions`; `guides/citation`; đo số vòng sửa (E-X4) |
| CPU không đủ cho rerank/embed | recall chậm | giới hạn top 30–50, cache; phương án API của provider |
| Feature catalog lộn xộn, trùng nghĩa | so khớp nhà xe kém | catalog có review; gộp feature trùng định kỳ |
| Sandbox chạy code nhà xe khác không an toàn | rò rỉ / tấn công | không mạng, giới hạn tài nguyên, chỉ hàm thuần; adapter API không chạy |
| Phụ thuộc thời gian của nhà xe pilot | trễ demo | chọn pilot hợp tác tốt; dữ liệu mẫu thay thế khi chờ |
| Chi phí LLM vượt dự tính | ngân sách | đo theo nhà xe/purpose từ M1; model *nhỏ* cho ingest/consolidate |

## 5. Definition of Done (mọi story)

- Code qua review, CI xanh (lint, unit, contract test, migration check).
- Có test cho AC; story liên quan prompt có prompt test.
- Tool MCP mới: có annotation đúng, output có ID + `next_actions`, có trong [mcp.md](mcp.md).
- Thay đổi schema: migration `up/down` + cập nhật [data-model.md](data-model.md).
- Có metric/trace cho đường đi mới.
- Tài liệu liên quan được cập nhật trong cùng PR.

## 6. Chỉ số thành công (sau M4)

| Chỉ số | Mục tiêu |
|---|---|
| Thời gian onboard một nhà xe "cơ bản" | ≤ 3 ngày làm việc |
| Artifact `valid` sau ≤ 3 vòng validate | ≥ 80% |
| Update rủi ro thấp → bot đã cập nhật (stale → refresh → valid) | ≤ 1 giờ |
| Khách mới tìm được nhà xe tương tự với % pass ví dụ ≥ 80% | ≥ 70% trường hợp |
| Câu trả lời sai do tri thức cũ trong test regression | 0 |
