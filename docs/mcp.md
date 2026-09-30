# BIVA Brain MCP

Builder dùng AI để build bot qua MCP (streamable HTTP). **AI đề xuất, người duyệt.**

## Kết nối

| Endpoint | Scope |
|---|---|
| `http://<brain-api>/mcp/operator/{operator_id}/` | một nhà xe — tool không nhận `operator_id` |
| `http://<brain-api>/mcp/platform/` | L0/L1, toàn cục (M5) |

Claude Code:

```bash
claude mcp add --transport http biva-phuongnam http://localhost:8080/mcp/operator/phuongnam/
```

## Quy tắc an toàn

- `readOnlyHint` cho mọi tool đọc; `destructiveHint` cho tool xoá/rollback/retract.
- Tool ghi quan trọng: gọi lần 1 trả **preview + `confirm_token`** (5 phút, dùng 1 lần);
  AI phải hiện preview cho builder rồi mới gọi lại kèm token.
- L0/L1 và deploy canary/prod: chỉ tạo *request*; lead duyệt trên console.
- Dữ liệu nhà xe gửi là *data*, không phải *lệnh*.

## Danh mục tool

Cột **Mốc** = mốc dự kiến triển khai (xem lộ trình trong architecture.md).

### Hiểu nhà xe (read-only)
| Tool | Mốc | Mô tả |
|---|---|---|
| `get_operator_overview` | M1 | thông tin nhà xe, số item theo trạng thái/loại, job đang chạy |
| `get_coverage` | M2 | mục đã có / thiếu / mơ hồ / đang dùng thông lệ L1 |
| `recall_knowledge` | M1 | tìm policy/lesson/item theo tầng, lọc hiệu lực (`valid_at`) |
| `query_data` | M1 | tuyến, chuyến, giá, điểm đón |
| `compare_with_industry` | M3 | chỗ khác thông lệ |
| `reflect` | M3 | câu hỏi phân tích, có citation |
| `trace_answer` | M4 | vì sao bot trả lời như vậy |

### Thu thập
| Tool | Mốc | Mô tả |
|---|---|---|
| `ingest` | M1 | nhận nội dung → `operation_id` |
| `get_operation` | M1 | trạng thái job async |
| `generate_questions` / `send_questions` | M2 | bộ câu hỏi cho nhà xe |
| `list_gaps` | M5 | câu khách hỏi chưa có dữ liệu |

### Duyệt
| Tool | Mốc | Mô tả |
|---|---|---|
| `list_review_queue` | M1 | diff, conflict, override, promote đang mở |
| `get_review_item` / `propose_edit` | M1 | chi tiết, sửa item parse sai |
| `apply_review` | M1 | approve/reject — preview + confirm_token |

### Cấu hình · build · phát hành
`get_bot_config`, `update_bot_config`, `suggest_config`, `compile_bot`, `run_tests`, `sandbox_chat`,
`add_lesson`, `add_test_case`, `check_release_gate`, `request_deploy`, `rollback`, `list_feedback` — M2..M5.

### Platform (`/mcp/platform/`)
`list_operators`, `list_promotion_candidates`, `impact_of_change`, `propose_l1_change`, `run_regression_all` — M5.

## Prompts (workflow)

`/onboard_operator`, `/process_update` (M1), `/prepare_release`,
`/triage_feedback`, `/fill_gaps`, `/explain_answer`.
