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
6. **Không lưu thông tin khách hàng**: log hội thoại có `expires_at`.

## Nhóm bảng

### Scope
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `operators` | nhà xe | `id`, `name`, `status` (onboarding · live · paused · offboarded) |
| `bots` | bot theo kênh của nhà xe | `id`, `operator_id`, `channel`, `config` (draft), `active_snapshot_id` |

### Nguồn
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `documents` | nội dung gốc nhà xe/team gửi | `layer`, `operator_id`, `source` (zalo · excel · web · form · call · console), `content`, `content_hash` (chống trùng), `submitted_by`, `received_at` |

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
| `snapshots` | Bot Definition đã compile | `bot_id`, `version`, `built_from` (version từng tầng), `definition`, `status` (draft · testing · passed · failed · active · retired), `test_report` |
| `test_cases` | regression theo tầng | `layer`, `operator_id`, `bot_id`, `input`, `expected` (must_call_tool, must_mention, must_not_say…), `source_item_id` |
| `releases` | yêu cầu và lịch sử **phát hành snapshot** cho runtime | `stage` (staging · production), `status` (requested · approved · published · rolled_back · rejected), `requested_by`, `approved_by` |

### Runtime (ngoài phạm vi chạy bot, chỉ là điểm nối)
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `runtime_clients` | runtime được phép gọi Runtime API | `id`, `name`, `api_key_hash`, `webhook_url`, `webhook_secret_ref`, `bot_ids[]`, `status` |

### Tín hiệu vận hành (có TTL)
| Bảng | Vai trò |
|---|---|
| `chat_logs` | transcript ẩn danh do runtime gửi qua Runtime API (tuỳ chọn), `expires_at` mặc định 30 ngày |
| `feedback` | runtime gửi qua Runtime API: thumbs_down · staff_correction · handoff · repeat_question → nguồn cho `learn` |
| `knowledge_gaps` | câu khách hỏi chưa có dữ liệu, gộp theo `question_norm`, đếm `hits` |

### Queue & audit
| Bảng | Vai trò | Cột chính |
|---|---|---|
| `operations` | queue job Go ⇄ Python | `kind`, `payload`, `status` (queued · running · done · failed · cancelled), `priority`, `attempts/max_attempts`, `idempotency_key`, `locked_by`, `lease_until`, `run_after`, `parent_id`, `trace_context` |
| `audit_log` | mọi thao tác ghi | `actor` (user:… · ai:<session> · system:<job>), `approved_by`, `action`, `target`, `payload` |

Queue: worker claim bằng `SELECT … FOR UPDATE SKIP LOCKED`, giữ lease; trigger `pg_notify('biva_operations')`
khi insert để đánh thức worker ngay; scheduler (Go) requeue job hết lease.

## Luồng dữ liệu của một update

```
documents ──parse──► items(pending) ──diff──► review_items(open)
                                                   │ approve
                                                   ▼
        items(active) + bản cũ → superseded ──► routes/trips/fares/pickup_points (version mới)
                │
                ▼ consolidate
        items(kind=observation) + observation_sources
                │ promote                 │ compile
                ▼                         ▼
        review_items(PROMOTE)       snapshots(draft → testing → passed) ──► releases ──webhook──► runtime
```
