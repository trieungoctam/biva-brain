# Luồng build bot

Brain không đứng riêng — mỗi bước build đọc/ghi vào Brain. Xem kiến trúc tổng ở [architecture.md](architecture.md).

## 1. Hai vòng lặp

```
   ┌──────────────── VÒNG BUILD (mỗi nhà xe mới) ────────────────────────────┐
   │ ① Khởi tạo → ② Thu thập → ③ Duyệt → ④ Cấu hình → ⑤ Compile+Test → ⑥ Deploy│
   └──────────────────────────────────────────────────────────────┬──────────┘
                                                                  ▼
   ┌──────────────── VÒNG VẬN HÀNH (suốt đời bot) ───────────────────────────┐
   │   Update nhà xe ──► ③ ─► ⑤ ─► ⑥          Hội thoại ──► learn ──► ⑤ ─► ⑥   │
   │   Câu bot không trả lời được ──► câu hỏi gửi nhà xe ──► ②                 │
   └─────────────────────────────────────────────────────────────────────────┘
                           ▲ tất cả đọc/ghi ▼
                        ════ BIVA BRAIN (L0·L1·L2·L3) ════
```

Bên tham gia: **Builder** (team BIVA, làm việc qua AI + MCP), **Nhà xe**, **Brain**, **Bot Runtime**.

## 2. Các bước của vòng build

### ① Khởi tạo nhà xe
- Builder: tạo nhà xe (tên, tuyến chính, kênh).
- Brain: tạo scope `operator:<id>`, kế thừa L0 + L1, sinh skeleton từ template onboarding L1
  (tuyến, giá, điểm đón, huỷ vé, hành lý, trẻ em, thú cưng, gửi hàng…).
- Kết quả: trang **Còn thiếu** (coverage 0%). Bot đã chạy được bằng thông lệ L1 nhưng chưa qua gate.

### ② Thu thập
- Nguồn: Excel giá/lịch, tin Zalo, website/fanpage, ghi âm, form.
- Brain: `ingest` từng nguồn → tách item → entity resolution → diff; **sinh bộ câu hỏi** cho mục thiếu/mơ hồ.
- Câu hỏi ưu tiên theo **độ khác nhau giữa các nhà xe**: mục L1 có `proof_count` cao, ít nhà xe lệch →
  chỉ hỏi xác nhận nhanh; mục hay khác nhau → hỏi chi tiết.

```
Xác nhận nhanh (giống đa số nhà xe):
  ☐ Trẻ em dưới 6 tuổi ngồi chung miễn phí?          [Đúng] [Khác: ___]
Cần trả lời (các nhà xe rất khác nhau):
  ☐ Chính sách huỷ vé: huỷ trước bao lâu, hoàn bao nhiêu %?
Mơ hồ trong dữ liệu đã gửi:
  ☐ "Đón dọc đường QL20" — cụ thể những điểm nào?
```

Lặp đến khi đạt ngưỡng coverage.

### ③ Chuẩn hoá & duyệt
- Builder xử lý **review queue**: diff, conflict, mục khác thông lệ, đề xuất promote.
- Brain: apply → Operational Data (versioned) → `consolidate` → `promote` → `refresh_pages`.
- Kết quả: Operator Profile pages để nhà xe đọc lại, xác nhận "bot hiểu đúng về mình".

### ④ Cấu hình bot
| Thành phần | Nguồn | Brain hỗ trợ |
|---|---|---|
| Persona, xưng hô | L2 persona | gợi ý từ nhà xe tương tự |
| Kênh (L3) | builder chọn | template theo kênh |
| Tool binding | API đặt vé của nhà xe hoặc "chuyển nhân viên" | kiểm tra đủ tool bắt buộc của L0 (vd `check_seats`) |
| Flow nghiệp vụ | template L1: hỏi giá, đặt vé, huỷ, khiếu nại, gửi hàng | bật/tắt theo dữ liệu nhà xe |
| Handoff | SĐT / nhóm Zalo nhà xe | kèm tóm tắt hội thoại đã ẩn danh |

### ⑤ Compile & Test
```
compile(operator, bot) → Bot Definition draft
  → sinh test:
       · regression L0 + L1        ("sai chung", "đúng chung")
       · từ L2 policy              mỗi policy → 2–3 câu hỏi + đáp án kỳ vọng
       · từ L2 data                phải GỌI tool, không đọc thuộc
       · override                  mục khác thông lệ → theo L2, không theo L1
       · thiếu dữ liệu             → phải nói "thông lệ chung / xác nhận nhà xe"
  → chạy test → báo cáo
  → sandbox chat (builder + nhà xe UAT); 👎 trong sandbox → lesson L2
```

**Release gate**

| Chỉ số | Ngưỡng |
|---|---|
| Coverage mục bắt buộc (tuyến, giá, giờ, điểm đón, huỷ vé) | 100% |
| Coverage mục khuyến nghị | ≥ 80% |
| Regression L0 + L1 | 100% pass |
| Test L2 | ≥ 95% pass |
| Conflict chưa xử lý | 0 |
| Nhà xe xác nhận UAT | ✅ |

### ⑥ Deploy
```
draft ──► staging (sandbox) ──► canary (10% hội thoại) ──► production
                                     │ 👎 / handoff tăng bất thường
                                     └──► auto rollback về snapshot trước
```
Bot Runtime pin theo `snapshot_id`; mỗi câu trả lời lưu `snapshot_id` + rule/observation đã dùng để truy vết.

## 3. Bot Definition (sản phẩm của compile)

```yaml
bot_id: phuongnam-zalo
snapshot: v43
built_from: {L0: v12, L1: v31, L2(phuongnam): v57, L3(zalo): v4}
persona:
  xung_ho: {bot: "nhà xe", khach: "anh/chị"}
  style: "ngắn gọn, thân thiện, có nút lựa chọn"
directives:
  - {rule: "Không xác nhận còn ghế khi chưa gọi check_seats", from: L0, locked: true}
  - {rule: "Luôn hỏi ngày đi + điểm đón trước khi báo giá", from: L1}
lessons:
  - {type: dont, from: L2, rule: "Không nhầm BX Miền Đông mới/cũ", example: "..."}
tools:
  check_seats: {provider: phuongnam_api}
  get_fare:    {source: operational_db, operator: phuongnam}
  handoff:     {zalo_group: "..."}
flows: [hoi_gia, dat_ve, huy_ve, khieu_nai]      # gui_hang: disabled
policy_index: policies/phuongnam/v43             # chỉ item active, đã merge theo tầng
fallback: "Thông lệ chung là…, anh/chị vui lòng xác nhận với nhà xe"
tests: {suite: phuongnam-v43, pass: 187/190}
```

## 4. Vòng vận hành

| Tín hiệu | Brain xử lý | Quay về |
|---|---|---|
| Nhà xe gửi update | ingest → diff → review | ③ → ⑤ → ⑥ (tự động nếu rủi ro thấp + test pass) |
| Tới `valid_to` (vd hết lịch Tết) | expire → consolidate | ⑤ → ⑥ tự động |
| Khách hỏi mà bot **không có dữ liệu** | ghi *knowledge gap*, gộp; đủ N lần → câu hỏi gửi nhà xe | ② |
| 👎 / nhân viên sửa / handoff | `learn` → lesson → consolidate/promote | ⑤ (thêm test) → ⑥ |
| Thay đổi L1/L0 | liệt kê bot kế thừa bị ảnh hưởng | ⑤ cho mọi bot đó, deploy lần lượt |

## 5. Sơ đồ onboarding một nhà xe

```
Builder        Nhà xe          Brain                          Bot Runtime
  │ tạo NX ───────────────────► create_scope, skeleton
  │◄──────────────────────────── "Còn thiếu" 0%
  │ upload Excel/Zalo ────────► ingest ×N → diff
  │◄──────────────────────────── bộ câu hỏi
  │ gửi form ──► │ trả lời ───► ingest → diff
  │ duyệt queue ──────────────► apply → consolidate → promote → pages
  │ cấu hình bot ─────────────► validate tools/flows theo L0
  │ build ────────────────────► compile → tests → report
  │ sandbox ──► │ UAT, 👎 ────► lesson L2 → compile lại
  │ deploy ───────────────────► snapshot v1 ────────────────► pin v1, canary → prod
  │                             ◄── gaps, 👎, handoff ─────── (log TTL, ẩn danh)
```

Mọi thao tác của builder ở trên đi qua MCP ([mcp.md](mcp.md)); duyệt quan trọng làm trên console.

## 6. Hiệu ứng tích luỹ

| | Nhà xe thứ 1 | Thứ 10 | Thứ 50 |
|---|---|---|---|
| L1 | gần như trống | có thông lệ, `proof_count` thấp | thông lệ vững, biết mục nào hay khác |
| Câu hỏi cho nhà xe | hỏi mọi thứ | phần lớn xác nhận nhanh | chỉ hỏi phần thực sự riêng |
| Bộ test | ít | có "sai chung" | regression dày |
| Thời gian onboard (ước tính) | 1–2 tuần | 3–5 ngày | 1–2 ngày |
