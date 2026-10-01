# BIVA Brain

Bộ tri thức (về nhà xe và về logic) để AI build chatbot cho nhiều nhà xe khách, dựa trên mô hình memory
của Hindsight (retain / recall / reflect + consolidation), mở cho builder dùng AI qua MCP.

Trạng thái: **đang chốt thiết kế**, chưa có code.

## Tài liệu

| Tài liệu | Nội dung |
|---|---|
| [docs/architecture.md](docs/architecture.md) | bài toán, phân tầng L0–L3, luồng Brain, kiến trúc Go + Python, lộ trình, điểm cần chốt |
| [docs/system-architecture.md](docs/system-architecture.md) | kiến trúc hệ thống: containers, components, luồng chạy, triển khai, độ tin cậy, bảo mật, observability, CI/CD |
| [docs/build-flow.md](docs/build-flow.md) | luồng build bot: onboarding → thu thập → duyệt → cấu hình → compile/test → deploy; vòng vận hành |
| [docs/mcp.md](docs/mcp.md) | **giao diện chính**: AI dùng tri thức để build bot — knowledge pack, artifact có trích dẫn, validate, tool, prompts |
| [docs/logic-knowledge.md](docs/logic-knowledge.md) | tri thức logic (code) cho nhà xe: module chung, hồ sơ từng nhà xe, config → hook → custom, ADR |
| [docs/data-model.md](docs/data-model.md) | data model trên PostgreSQL |
