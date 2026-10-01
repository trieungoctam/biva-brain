# Data model

Thiết kế dữ liệu của BIVA Brain trên PostgreSQL 16 (pgvector, pg_trgm, unaccent).
Đây là tài liệu thiết kế; migration thật sẽ nằm ở `contracts/migrations/` khi bắt đầu code.

## Nguyên tắc

1. **Một Postgres** cho mọi thứ: vector, full-text, graph (bảng cạnh), JSONB, queue.
2. **Fact và observation chung một bảng** (`items`, phân biệt bằng `kind`) — mọi arm của recall tìm cả hai
   trong một pipeline (như Hindsight).
3. **Không xoá khi "quên"** — đổi `status` + `valid_to`; chỉ xoá vật lý khi purge.
4. **Scope bằng cột, không bằng bảng riêng**: `layer` (0–3) + `operator_id` + `bot_id`,
   có CHECK để tầng và scope luôn khớp nhau.
5. **Data vận hành tách khỏi tri thức mềm**: giá/lịch/điểm đón ở bảng riêng, bot đọc qua tool.
6. **Không lưu thông tin khách hàng**: transcript (sandbox/UAT, sau này từ runtime) ẩn danh và có `expires_at`.
7. **Mọi thứ AI viết đều truy về tri thức**: `artifact_citations` và `logic_param_sources` là cơ sở để đánh dấu stale.

## Nhóm bảng

### Scope
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `operators` | nhà xe | `id`, `name`, `status` (onboarding · live · paused · offboarded) |
| `bots` | bot theo kênh của nhà xe | `id`, `operator_id`, `channel` (zalo · messenger · web), `current_snapshot_id` |

### Nguồn
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `documents` | nội dung gốc nhà xe/team gửi | `layer`, `operator_id`, `source` (zalo · excel · form · console), `content`, `content_hash` (chống trùng), `submitted_by`, `received_at` |

### Tri thức (`items`)
| Nhóm cột | Cột | Ghi chú |
|---|---|---|
| Scope | `layer`, `operator_id`, `bot_id` | L0/L1: không có operator; L2: có operator; L3: có bot |
| Nội dung | `kind` (data · policy · lesson · persona · observation), `topic`, `key`, `text`, `value` (JSONB) | `key` là facet ổn định để diff, vd `fare:sgn-dli:sleeper` |
| Tìm kiếm | `search_text`, `tsv` (generated), `embedding` VECTOR(1024) | `search_text` = bản không dấu + bigram âm tiết, do worker ghi |
| Thời gian | `occurred_start/end`, `mentioned_at`, **`valid_from/valid_to`** | 3 trục: xảy ra · được nói · hiệu lực |
| Vòng đời | `status` (draft · pending · active · superseded · expired · retracted · rejected), `superseded_by` | cơ chế "quên" |
| Kế thừa | `locked` | chỉ L0/L1; tầng dưới không ghi đè được |
| Observation | `proof_count`, `history` (JSONB) | lịch sử các lần diễn đạt trước |
| Consolidation | `consolidated_at` | NULL = chưa consolidate → observation liên quan stale |
| Nguồn gốc | `document_id` | truy về document gốc |

Index chính: item active theo scope; `key` cho diff; item chưa consolidate; `valid_to` cho job expire;
GIN trên `tsv`; HNSW trên `embedding`.

| Bảng phụ | Vai trò |
|---|---|
| `observation_sources` | observation ↔ item bằng chứng, kèm `quote` |

### Entity graph
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `entities` | bến, tỉnh, tuyến, loại xe, chủ đề chính sách… | `entity_type`, `name`, `name_norm` (trigram index), `aliases[]`, `layer`, `operator_id` |
| `item_entities` | item nhắc tới entity nào | |
| `entity_cooccurrences` | đếm cặp entity xuất hiện cùng nhau | dùng cho entity resolution |
| `item_links` | cạnh giữa item | `link_type`: entity · temporal · semantic · caused_by · **overrides** · **supersedes**; `weight` 0–1 |

### Data vận hành (bot đọc qua tool)
| Bảng | Cột chính |
|---|---|
| `routes` | `operator_id`, `origin_entity_id`, `destination_entity_id`, `name`, `status` |
| `trips` | `route_id`, `depart_time`, `days_of_week[]`, `vehicle_type`, `valid_from/to`, `status`, `source_item_id` |
| `fares` | `route_id`, `vehicle_type`, `amount_vnd`, `valid_from/to`, `status`, `superseded_by`, `source_item_id` |
| `pickup_points` | `operator_id`, `route_id`, `entity_id`, `name`, `address`, `time_offset`, `valid_from/to`, `status` |

`source_item_id` nối dữ liệu vận hành về item đã duyệt sinh ra nó (truy vết + retract dây chuyền).

### Duyệt & build
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `review_items` | hàng đợi duyệt | `change_kind` (NEW · CHANGE · REMOVE · DUPLICATE · CONFLICT · OVERRIDE · PROMOTE), `before`, `after`, `risk`, `status`, `decided_by` |
| `snapshots` | Bot Definition lắp từ artifact `valid` + hồ sơ logic `active` | `bot_id`, `version`, `built_from` (version từng tầng), `definition`, `artifact_versions`, `status` (assembled · testing · passed · failed · published · retired), `test_report` |
| `test_cases` | regression theo tầng | `layer`, `operator_id`, `bot_id`, `input`, `expected` (must_call_tool, must_mention, must_not_say…), `source_item_id` |
| `releases` | yêu cầu và lịch sử **phát hành snapshot** (đánh dấu bản chính thức) | `stage` (staging · production), `status` (requested · approved · published · rolled_back · rejected), `requested_by`, `approved_by` |

### Artifact của bot (AI viết, Brain kiểm)
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `bot_artifacts` | các phần của bot: persona · system_prompt · faq · flows · tool_spec · fallbacks | `bot_id`, `kind`, `version`, `content`, `status` (draft · valid · invalid · stale · published), `author` (ai:<session> / user), `validation` (JSONB lỗi có vị trí) |
| `artifact_citations` | câu/đoạn trong artifact ↔ item được trích dẫn | `artifact_id`, `item_id`, `location` (dòng/đoạn) — dùng để đánh dấu **stale** khi item rời `active` |

Snapshot là đầu vào cho `export_bot` và `releases`. Artifact chỉ được lắp vào snapshot khi `valid` và không `stale`.

### Tri thức logic (xem [logic-knowledge.md](logic-knowledge.md))

Code nằm ở git; các bảng dưới **đồng bộ từ repo** (`module.yaml`, `profile.yaml`, `tests/cases.yaml`) hoặc do Brain dựng.

| Bảng | Vai trò | Cột chính |
|---|---|---|
| `logic_modules` | module L1 (chuẩn) hoặc L2 (custom), từ `module.yaml` | `id`, `layer`, `operator_id`, `capability`, `version`, `summary`, `entrypoint`, `features[]`, `params_schema`, `hooks`, `repo`, `path`, `commit`, `status` (active · deprecated · retired), `proof_count` |
| `logic_profiles` | hồ sơ logic của nhà xe theo capability, từ `profile.yaml` | `operator_id`, `capability`, `mode` (config · hook · custom), `module_id`, `params`, `hooks`, `decision_id`, `derived_from` (operator), `commit`, `status` (pending · active · stale · retired) |
| `logic_param_sources` | tham số ↔ item tri thức là nguồn | `profile_id`, `param_path`, `item_id` |
| `logic_features` | **danh mục feature** L1 | `id` (vd `fare.holiday_surcharge`), `capability`, `description`, `params_schema`, `operator_count`, `status` (proposed · active · deprecated) |
| `logic_specs` | **logic spec** của nhà xe theo capability (có cả khi chưa có code) | `operator_id`, `capability`, `features` (JSONB: feature + tham số), `rules_text[]`, `embedding`, `implementation` (nullable), `family_id`, `status` |
| `logic_families` | **họ logic** (cụm spec) | `id`, `capability`, `name`, `centroid_features`, `members[]`, `recommended_implementation`, `promote_candidate` |
| `logic_similarity` | cache điểm tương đồng giữa hai nhà xe theo capability | `capability`, `operator_a`, `operator_b`, `spec_score`, `code_score`, `behavior_pass_rate`, `computed_at` |
| `logic_tests` | ví dụ input → output (từ `tests/cases.yaml` và ví dụ nhà xe gửi) | `operator_id`, `capability`, `input`, `expected`, `note`, `source_item_id` |
| `logic_decisions` | ADR: vì sao hook/custom | `operator_id`, `capability`, `context`, `options`, `decision`, `author`, `approved_by` |
| `code_chunks` | index code theo hàm/lớp, gắn commit | `repo`, `path`, `commit`, `symbol`, `text`, `summary`, `search_text`, `embedding`, `module_id`, `operator_id` |

Bài học về code dùng chung bảng `items` (`kind=lesson`, `topic=code:<capability>`).

### Runtime (giai đoạn sau)
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `runtime_clients` | runtime được phép gọi Runtime API | `id`, `name`, `api_key_hash`, `webhook_url`, `webhook_secret_ref`, `bot_ids[]`, `status` |

### Tín hiệu vận hành (có TTL)
| Bảng | Vai trò |
|---|---|
| `chat_logs` | transcript ẩn danh: sandbox/UAT (hiện tại), runtime qua Runtime API (giai đoạn sau); `expires_at` mặc định 30 ngày |
| `feedback` | 👎 / sửa trong sandbox/UAT (hiện tại), từ runtime (giai đoạn sau): thumbs_down · staff_correction · handoff · repeat_question → lesson + test case |
| `knowledge_gaps` | câu bot chưa trả lời được (test, sandbox/UAT; sau này từ runtime), gộp theo `question_norm`, đếm `hits` → câu hỏi cho nhà xe |

### Queue & audit
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `operations` | queue job Go ⇄ Python | `kind`, `payload`, `status` (queued · running · done · failed · cancelled), `priority`, `attempts/max_attempts`, `idempotency_key`, `locked_by`, `lease_until`, `run_after`, `parent_id`, `trace_context` |
| `audit_log` | mọi thao tác ghi, mọi lần bị từ chối vì vượt quyền | `actor` (user:… · ai:<session> · system:<job> · cli:<os user>), `approved_by`, `action`, `target`, `payload` |

Queue: worker claim bằng `SELECT … FOR UPDATE SKIP LOCKED`, giữ lease; trigger `pg_notify('biva_operations')`
khi insert để đánh thức worker ngay; scheduler (Go) requeue job hết lease.

### Người dùng & quyền (người của BIVA, không phải khách hàng)

| Bảng | Vai trò | Trường chính |
|---|---|---|
| `users` | builder · lead · ops | `id`, `email`, `name`, `role`, `status` (active · disabled) |
| `user_operators` | builder được gán nhà xe nào | `user_id`, `operator_id`, `granted_by` |
| `api_tokens` | token cá nhân cho MCP | `token_hash` (SHA-256, không lưu token gốc), `prefix`, `expires_at` (bắt buộc), `revoked_at`, `last_used_at` |

## Luồng dữ liệu của một update

```
documents ──parse──► items(pending) ──diff──► review_items(open)
                                                   │ approve
                                                   ▼
        items(active) + bản cũ → superseded ──► routes/trips/fares/pickup_points (version mới)
                │
                ├─► mark_stale: artifact_citations / logic_param_sources trỏ tới bản cũ
                │               → bot_artifacts(stale), logic_profiles(stale)
                │                     │ AI /refresh_bot (MCP)
                │                     ▼
                │               bot_artifacts(draft → valid) ──► snapshots(assembled → passed) ──► releases / export
                │
                ▼ consolidate
        items(kind=observation) + observation_sources
                │ promote
                ▼
        review_items(PROMOTE)
```
