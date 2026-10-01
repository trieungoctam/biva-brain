# Luồng build bot

Mỗi bước build đều đọc/ghi vào Brain; builder làm việc qua AI + MCP ([mcp.md](mcp.md)).
Kiến trúc tổng: [architecture.md](architecture.md).

## 1. Hai vòng lặp

```
   ┌──────────────── VÒNG BUILD (mỗi nhà xe mới) ─────────────────────────────────────┐
   │ ① Khởi tạo → ② Thu thập → ③ Duyệt → ④ Viết bot (AI) → ⑤ Kiểm tra → ⑥ Xuất & phát hành │
   └───────────────────────────────────────────────────────────────────┬─────────────┘
                                                                       ▼
   ┌──────────────── VÒNG CẬP NHẬT (suốt đời bot) ─────────────────────────────────────┐
   │  Update nhà xe ──► ③ ──► stale ──► ④ /refresh_bot (chỉ đoạn bị ảnh hưởng) ──► ⑤ ──► ⑥ │
   │  Hết hạn (valid_to) ──► stale ──► ④ ──► ⑤ ──► ⑥                                     │
   │  Thiếu dữ liệu (phát hiện ở sandbox/UAT/test) ──► câu hỏi gửi nhà xe ──► ②          │
   └───────────────────────────────────────────────────────────────────────────────────┘
                           ▲ tất cả đọc/ghi ▼
                 ════ BIVA BRAIN (L0·L1·L2·L3, tri thức + logic) ════
```

Bên tham gia: **Builder** (team BIVA, làm việc với AI qua MCP), **AI** (viết bot), **Nhà xe**, **Brain**,
**Lead** (duyệt L0/L1 và phát hành production). Chạy bot nằm ngoài phạm vi; Brain xuất bot ra định dạng trung lập.

## 2. Các bước của vòng build

### ① Khởi tạo nhà xe
- Builder: tạo nhà xe (tên, tuyến chính, kênh).
- Brain: tạo scope `operator:<id>`, kế thừa L0 + L1, sinh skeleton từ template onboarding L1
  (tuyến, giá, điểm đón, huỷ vé, hành lý, trẻ em, thú cưng, gửi hàng…) và danh sách **capability logic** bắt buộc.
- Kết quả: trang **Còn thiếu** (coverage 0%).

### ② Thu thập
- Nguồn v1: tin Zalo, file Excel giá/lịch. Form ở M2.
- Brain: `ingest` → tách item → entity resolution → diff; **sinh bộ câu hỏi** cho mục thiếu/mơ hồ.
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
- Builder xử lý **review queue** (qua MCP hoặc console): diff, conflict, mục khác thông lệ, đề xuất promote.
- Brain: apply → data vận hành (versioned) → `mark_stale` → `consolidate` → `promote` → `refresh_pages`.
- Kết quả: Operator Profile pages để nhà xe đọc lại, xác nhận "Brain hiểu đúng về mình".

### ④ Viết bot (AI qua MCP)

Prompt `/build_bot` và `/implement_operator_logic`:

```
get_bot_spec        → bot cần artifact nào, mục bắt buộc, capability/tool bắt buộc
get_knowledge_pack  → tri thức đã merge theo tầng, còn hiệu lực, có nhãn nguồn
get_operator_logic  → capability đã có / còn thiếu
find_similar_operators → nhà xe nào có logic giống
AI viết:
  · artifact: persona · system_prompt · faq · flows · tool_spec · fallbacks — mọi câu có [[item_id]]
  · hồ sơ logic: chọn config → hook → custom (custom kèm ADR); tham số có param_sources
save_artifact / propose_logic_profile
```

Khi viết, AI phát hiện thiếu hoặc mâu thuẫn trong tri thức → `propose_item` (vào review queue) thay vì tự bịa.

### ⑤ Kiểm tra

```
validate_artifact   → lỗi có vị trí: UNCITED, STALE_CITATION, MISSING_LOCKED, UNLABELED_DEFAULT,
                      HARDCODED_DATA, NO_CAPABILITY, CONTRADICTION, COVERAGE → AI sửa đến khi sạch
logic tests         → input → output từ ví dụ thật của nhà xe (vd "28 Tết, giường nằm SG→ĐL = 450k")
run_tests (M3)      → reference executor chạy snapshot với:
                        · regression L0 + L1   ("sai chung", "đúng chung")
                        · test từ L2 policy    mỗi policy → 2–3 câu hỏi + đáp án kỳ vọng
                        · test từ L2 data      phải GỌI tool, không đọc thuộc
                        · override             mục khác thông lệ → theo L2, không theo L1
                        · thiếu dữ liệu        → phải nói "thông lệ chung / xác nhận nhà xe"
sandbox_chat (M3)   → builder + nhà xe chat thử (UAT); 👎 → lesson L2 + test case;
                      câu bot không trả lời được → knowledge gap → câu hỏi cho nhà xe (②)
```

**Release gate**

| Chỉ số | Ngưỡng |
|---|---|
| Coverage mục bắt buộc (tuyến, giá, giờ, điểm đón, huỷ vé) | 100% |
| Coverage mục khuyến nghị | ≥ 80% |
| Artifact | tất cả `valid`, 0 `stale` |
| Logic test của capability đang dùng | 100% pass |
| Regression L0 + L1 | 100% pass |
| Test L2 | ≥ 95% pass |
| Conflict chưa xử lý | 0 |
| Nhà xe xác nhận UAT | ✅ |

### ⑥ Xuất & phát hành

```
artifact valid ──► snapshot (lắp ráp, có version) ──► export_bot: json · markdown · faq_csv
                                     │
                                     └──► request_publish: staging (tự động khi gate pass)
                                                          production (lead duyệt)
                                          rollback = phát hành lại snapshot trước
```

Brain không chạy bot: "phát hành" là **đánh dấu snapshot nào là bản chính thức** để đem nạp vào runtime.
Việc nạp vào runtime cụ thể nằm ngoài Brain (giai đoạn sau: Runtime Integration API).

## 3. Bot Definition (snapshot)

Snapshot lắp từ các artifact đã `valid` và hồ sơ logic `active`; mỗi phần giữ nguồn trích dẫn.

```yaml
bot_id: phuongnam-zalo
snapshot: v43
built_from: {L0: v12, L1: v31, L2(phuongnam): v57, L3(zalo): v4}
artifacts:
  persona:       {version: 3, citations: [it_201]}
  system_prompt: {version: 7, citations: [it_007, it_045, it_311, it_090]}
  faq:           {version: 5, entries: 42}
  flows:         [hoi_gia, dat_ve, huy_ve, khieu_nai]      # gui_hang: tắt vì nhà xe không gửi hàng [[it_160]]
  fallbacks:     {version: 2}
tools:                                                      # mỗi tool trỏ tới capability trong hồ sơ logic
  get_fare:    {capability: fare,    module: fare.standard@2}
  check_seats: {capability: booking, mode: handoff}          # nhà xe chưa có API
  handoff:     {target: "nhóm Zalo nhà xe"}
validation: {errors: 0, stale: 0}
tests: {suite: phuongnam-v43, pass: 187/190}
```

## 4. Vòng cập nhật

| Tín hiệu | Brain xử lý | Quay về |
|---|---|---|
| Nhà xe gửi update | ingest → diff → review → apply → `mark_stale` | ④ `/refresh_bot` → ⑤ → ⑥ |
| Tới `valid_to` (vd hết lịch Tết) | `expire` → `mark_stale` | ④ → ⑤ → ⑥ |
| Thiếu dữ liệu (test, sandbox, UAT) | knowledge gap → câu hỏi | ② |
| 👎 trong sandbox/UAT | lesson L2 + test case → consolidate/promote | ⑤ |
| Thay đổi L1/L0 hoặc module logic L1 | `impact_of_change`: liệt kê nhà xe, bot, test bị ảnh hưởng | ④ → ⑤ → ⑥ cho từng bot |
| Feedback từ hội thoại thật | *giai đoạn sau*, khi có Runtime Integration API | — |

## 5. Sơ đồ onboarding một nhà xe

```
Builder + AI (MCP)        Nhà xe          Brain
  │ tạo nhà xe ─────────────────────────► scope, skeleton, capability bắt buộc
  │◄───────────────────────────────────── "Còn thiếu" 0%
  │ ingest Excel/Zalo ──────────────────► parse → diff
  │◄───────────────────────────────────── bộ câu hỏi
  │ gửi câu hỏi ──► │ trả lời ──────────► ingest → diff
  │ duyệt review queue ─────────────────► apply → consolidate → promote → pages
  │ /implement_operator_logic ──────────► hồ sơ logic (config/hook/custom + ADR), logic test
  │ /build_bot ─────────────────────────► knowledge pack → artifact → validate (lặp đến sạch)
  │ sandbox ──► │ UAT, 👎 ──────────────► lesson L2, test case, knowledge gap
  │ export / request_publish ───────────► snapshot v1 (json · markdown · faq_csv)
```

## 6. Hiệu ứng tích luỹ

| | Nhà xe thứ 1 | Thứ 10 | Thứ 50 |
|---|---|---|---|
| L1 (tri thức) | gần như trống | có thông lệ, `proof_count` thấp | thông lệ vững, biết mục nào hay khác |
| L1 (logic) | vài module chuẩn | module chuẩn + tham số phổ biến | ít custom; custom lặp lại đã được promote |
| Câu hỏi cho nhà xe | hỏi mọi thứ | phần lớn xác nhận nhanh | chỉ hỏi phần thực sự riêng |
| Bộ test | ít | có "sai chung" | regression dày |
| Thời gian onboard (ước tính) | 1–2 tuần | 3–5 ngày | 1–2 ngày |
