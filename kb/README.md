# kb/ — tri thức nền L0 / L1

Nguồn sự thật cho tri thức **dùng chung mọi nhà xe**, review bằng PR (lead duyệt PR = duyệt L0/L1).
Tri thức riêng của từng nhà xe (L2) không nằm ở đây: nó vào Brain qua ingest + review.

```
kb/
├── L0/rules.yaml                  luật nền tảng cho mọi bot (guardrail)
└── L1/<ngành>/
    ├── rules.yaml                 thông lệ ngành
    └── template.yaml              template onboarding: mục bắt buộc/khuyến nghị, capability, artifact
```

Schema: `contracts/schemas/kb/rules.schema.json`, `contracts/schemas/kb/template.schema.json`.

## Rule

```yaml
- key: l1.default.luggage     # l0.* / l1.* — ổn định, không đổi khi sửa nội dung
  kind: policy                # policy | persona | lesson
  topic: luggage              # L1: phải có trong template.yaml của ngành
  locked: false
  text: Mỗi khách thường được mang ... (≥ 10 ký tự)
  value: {free_kg: 20}        # tuỳ chọn, dữ liệu có cấu trúc
```

| `locked` | Nghĩa |
|---|---|
| `true` | guardrail: tầng dưới (L2/L3) không ghi đè được; thiếu trong artifact → `MISSING_LOCKED` |
| `false` | mặc định / thông lệ: chỉ dùng khi nhà xe chưa có thông tin riêng. L1 không locked được gắn nhãn `thông lệ chung` — bot phải nói rõ và mời khách xác nhận (L0 `l0.label_industry_default`) |

## Template onboarding

`sections[].topic` là **bộ từ vựng chuẩn** cho `items.topic`: parser, coverage, `get_bot_spec`, release gate đều dùng.
Mục `required` phải phủ 100% trước phát hành, `recommended` ≥ 80% (docs/build-flow.md).
`capabilities` là logic bot cần (`booking` cho phép `handoff` khi nhà xe chưa có API).

## Quy trình

```bash
cd go
go run ./cmd/brain-api kb check            # kiểm schema, key trùng, prefix tầng, topic ∈ template (CI chạy trong make lint)
brain-api kb sync --dry-run                # xem sẽ thêm / sửa / bỏ gì
brain-api kb sync                          # ghi: idempotent theo key
```

Sync: key mới → thêm; nội dung đổi → bản cũ `superseded` (trỏ `superseded_by` sang bản mới, artifact trích dẫn bản cũ
thành stale); key bị xoá khỏi file → `retracted`. Mỗi thay đổi ghi `audit_log` (`kb.add|update|retire`), có thay đổi thì
tạo job `index.items`. Không sửa trực tiếp item L0/L1 trong DB — sửa file rồi sync.

## Trạng thái

Bản nháp đầu (DYN-111): 12 rule L0, 12 rule L1 xe khách (4 locked + 8 thông lệ), template 16 mục (5 bắt buộc).
**Nội dung nghiệp vụ L1 cần vận hành/lead xác nhận** trước khi sync lên môi trường có nhà xe thật.
