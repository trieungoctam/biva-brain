# BIVA Brain

Bộ tri thức và pipeline để build, vận hành chatbot cho nhiều nhà xe khách, dựa trên mô hình memory
của Hindsight (retain / recall / reflect + consolidation), mở cho builder dùng AI qua MCP.

Trạng thái: **đang chốt thiết kế**, chưa có code.

## Tài liệu

| Tài liệu | Nội dung |
|---|---|
| [docs/architecture.md](docs/architecture.md) | bài toán, phân tầng L0–L3, luồng Brain, kiến trúc Go + Python, lộ trình, điểm cần chốt |
| [docs/build-flow.md](docs/build-flow.md) | luồng build bot: onboarding → thu thập → duyệt → cấu hình → compile/test → deploy; vòng vận hành |
| [docs/mcp.md](docs/mcp.md) | giao diện MCP cho builder: endpoint, quy tắc an toàn, danh mục tool, prompts |
| [docs/data-model.md](docs/data-model.md) | data model trên PostgreSQL |
