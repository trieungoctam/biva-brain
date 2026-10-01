# Tri thức logic (code) cho nhà xe

Ngoài tri thức *về nhà xe* (giá, lịch, chính sách…), Brain lưu tri thức *về cách xử lý* cho từng nhà xe:
logic tính giá, giữ chỗ/huỷ, đồng bộ lịch, adapter API đặt vé, gán điểm đón, trung chuyển…
Nhiều nhà xe dùng chung logic; một số nhà xe có logic đặc biệt.

Mục tiêu: khi AI build bot (hoặc viết code tích hợp) cho một nhà xe, nó biết **logic nào đã có, nhà xe nào giống,
chỗ nào phải viết riêng và vì sao** — thay vì viết lại từ đầu hoặc copy nhầm logic của nhà xe khác.

## 1. Nguyên tắc

1. **Code nằm ở git, tri thức về code nằm ở Brain.** Brain không là nơi lưu code; Brain lưu danh mục module,
   hồ sơ logic của từng nhà xe, quyết định, bài học, test — và **index** code (theo commit) để tìm kiếm.
2. **Cùng mô hình tầng** với tri thức nhà xe: chung ở L1, riêng ở L2, ghi đè có lý do, promote khi lặp lại.
3. **Bậc thang tuỳ biến — chọn bậc thấp nhất đủ dùng:**

   ```
   ① Config      dùng module chung, chỉ khác tham số            (đa số nhà xe)
   ② Hook        module chung + điểm mở rộng có sẵn             (khác một bước nhỏ)
   ③ Custom      module riêng cho nhà xe — phải có lý do (ADR)   (nhà xe đặc biệt)
   ```

4. **Tham số truy về tri thức.** Tham số của logic (vd phụ thu Tết 20%) trích dẫn item tri thức nhà xe
   (`param_sources`) — khi item đó bị thay thế/hết hạn, hồ sơ logic bị đánh dấu stale như artifact của bot.

## 2. Phân tầng logic

| Tầng | Chứa gì | Ví dụ |
|---|---|---|
| **L0 Platform** | chuẩn code, hợp đồng interface của tool, quy tắc an toàn | interface `FareCalculator`, "mọi adapter phải có timeout + retry", "không log SĐT" |
| **L1 Ngành** | **module chuẩn** + pattern + bài học chung | `fare.standard` (giá theo tuyến × loại xe × mùa), `booking.hold` (giữ chỗ N phút), `schedule.sync_excel` |
| **L2 Nhà xe** | **hồ sơ logic**: module nào, version nào, tham số gì, hook nào, module riêng nào | Phương Nam: `fare.standard@2` + phụ thu Tết 20%; hook `pickup.assign` theo giờ; custom `transfer.q7_shuttle` |

## 3. Đối tượng tri thức

| Đối tượng | Nội dung | Ghi chú |
|---|---|---|
| **Logic module** | tên, mục đích, interface, `params_schema`, các hook, repo/path, version, trạng thái, test bắt buộc | L1 (chuẩn) hoặc L2 (custom) |
| **Operator logic profile** | với mỗi capability (tính giá, giữ chỗ…): `mode` (config · hook · custom), module + version, `params`, `param_sources` (item id), hook đã cài | 1 profile / nhà xe |
| **Pattern / recipe** | cách làm một việc, có ví dụ code ngắn | "Cách viết adapter cho API đặt vé dạng REST" |
| **Decision (ADR)** | vì sao nhà xe này cần hook/custom, phương án đã cân nhắc | bắt buộc với mode ③ |
| **Lesson (code)** | đúng/sai chung về code | "Giá Tết tính theo **ngày đi**, không theo ngày đặt" |
| **Logic test** | input → output kỳ vọng, lấy từ ví dụ thật của nhà xe | "28 Tết, giường nằm SG→ĐL = 450k" |
| **Code chunk (index)** | đoạn code theo symbol, gắn repo/path/**commit** | để `search_logic` tìm được code thật |

## 4. Luồng

### 4.1 Onboard logic cho nhà xe mới

```
AI (MCP) ── get_operator_logic ──► profile trống, liệt kê capability bắt buộc (từ L1)
        ── find_similar_operators(capability) ──► "7 nhà xe dùng fare.standard@2; 2 nhà xe có phụ thu Tết"
        ── với mỗi capability, chọn bậc thấp nhất:
             ① đủ tham số từ tri thức nhà xe?  → propose_logic_profile(mode=config, params, param_sources)
             ② cần khác 1 bước?               → mode=hook, chỉ ra hook nào
             ③ không vừa module nào?          → mode=custom + ADR (bắt buộc)
        ── add_logic_test từ ví dụ nhà xe cung cấp
        ── review (builder/lead duyệt) → profile active
Code: AI viết code (mode ②③) trong repo như bình thường; CI chạy logic test; merge → Brain re-index theo commit.
```

### 4.2 Giống nhau và đặc biệt

| Tình huống | Brain làm |
|---|---|
| Nhiều nhà xe dùng cùng module với tham số tương tự | tăng `proof_count` của module; gợi ý giá trị tham số mặc định cho L1 |
| **≥ 3 nhà xe** có custom/hook làm cùng một việc | **đề xuất promote**: đưa thành tham số hoặc hook chuẩn của module L1 (kèm danh sách nhà xe được lợi) |
| Một nhà xe thật sự đặc biệt | giữ custom ở L2, ADR ghi lý do; không kéo L1 theo |
| Module L1 đổi version | `impact_of_change`: liệt kê nhà xe đang dùng version cũ, test nào phải chạy lại |

Phát hiện "làm cùng một việc": so sánh capability + interface + độ tương đồng code (embedding trên code chunk)
+ tham số; LLM xác nhận trước khi tạo đề xuất.

### 4.3 Quên

| Sự kiện | Hệ quả |
|---|---|
| Item tri thức là nguồn của tham số bị superseded/expired | profile → `stale`; AI được báo cập nhật tham số |
| Module version bị deprecated | profile dùng version đó → cảnh báo nâng cấp |
| Code ở commit mới | re-index code chunk; chunk của commit cũ không còn được trả về |
| Custom module bị thay bằng module L1 sau promote | custom → `retired`, giữ lịch sử + ADR |

### 4.4 Nối với bot

`tool_spec` của bot (xem [mcp.md](mcp.md)) trích dẫn **capability** trong profile (vd `get_fare` → `fare.standard@2`
với params của Phương Nam). `validate_artifact` kiểm tra: tool bot gọi phải có capability tương ứng trong profile active.

## 5. Ví dụ hồ sơ logic

```yaml
operator: phuongnam
capabilities:
  fare:
    mode: config
    module: fare.standard@2
    params:
      holiday_surcharge: {tet: 0.20, le_30_4: 0.10}
      child_policy: half_over_4
    param_sources: [it_118, it_311]
  booking_hold:
    mode: config
    module: booking.hold@1
    params: {hold_minutes: 30}
    param_sources: [it_140]
  pickup:
    mode: hook
    module: pickup.assign@1
    hooks: {resolve_point: "phuongnam/pickup_by_hour.py:resolve"}
    decision: adr_017
  transfer:
    mode: custom
    module: phuongnam.transfer_q7_shuttle@1
    repo: biva-integrations, path: operators/phuongnam/transfer.py
    decision: adr_021        # "trung chuyển Q7 chỉ chạy chuyến sau 20h, đón theo cụm"
tests: [lt_301, lt_302, lt_303]
status: active
```

## 6. MCP tools (logic)

| Tool | Cờ | Mô tả |
|---|---|---|
| `search_logic` | RO | tìm module, pattern, lesson, code chunk (semantic + keyword trên code) |
| `get_logic_module` | RO | chi tiết module: interface, params_schema, hooks, version, test bắt buộc |
| `get_operator_logic` | RO | hồ sơ logic của nhà xe, capability còn thiếu, chỗ stale |
| `find_similar_operators` | RO | nhà xe nào có logic giống cho một capability (module, tham số, custom tương tự) |
| `propose_logic_profile` | | đề xuất config/hook/custom cho một capability → review (custom bắt buộc kèm ADR) |
| `record_decision` | | ghi ADR |
| `add_logic_test` / `list_logic_tests` | | test input → output từ ví dụ thật |
| `impact_of_change` | RO | module/version đổi → nhà xe, bot, test bị ảnh hưởng |

Prompt: `/implement_operator_logic` — đi theo §4.1.
