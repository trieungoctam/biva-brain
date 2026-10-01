# Tri thức logic (code) cho nhà xe

Ngoài tri thức *về nhà xe* (giá, lịch, chính sách…), Brain lưu tri thức *về cách xử lý* cho từng nhà xe:
logic tính giá, giữ chỗ/huỷ, đồng bộ lịch, adapter API đặt vé, gán điểm đón, trung chuyển…
Mỗi nhà xe có thể xử lý khác nhau; phần lớn rơi vào vài **họ logic** quen thuộc, một số thật sự đặc biệt.

Mục tiêu:

1. Khi có **khách mới**, tìm ngay **nhà xe tương tự** và tái dùng cách triển khai của họ → triển khai nhanh.
2. Logic giống nhau được **gộp về module chung**; logic đặc biệt có **lý do rõ ràng** (ADR).
3. Logic luôn **khớp tri thức nhà xe**: tham số đổi theo tri thức, tri thức bị "quên" thì logic bị đánh dấu stale.

## 1. Nguyên tắc

1. **Code ở git, tri thức về code ở Brain.** Brain không là nơi sửa code (để giữ PR, review, CI, lịch sử git).
   Brain **đồng bộ** từ repo: manifest module, hồ sơ logic, test, và index code theo commit.
2. **So nhà xe theo hành vi nghiệp vụ, không theo code thô.** Hai nhà xe có thể viết code khác nhau mà cùng
   quy tắc, hoặc code giống nhau mà quy tắc khác. Tìm kiếm bắt đầu từ **yêu cầu** (đã có ngay khi ingest),
   rồi mới tới **code** của nhà xe tương tự.
3. **Bậc thang tuỳ biến — chọn bậc thấp nhất đủ dùng:**

   ```
   ① Config      dùng module chung, chỉ khác tham số            (đa số nhà xe)
   ② Hook        module chung + điểm mở rộng có sẵn             (khác một bước nhỏ)
   ③ Custom      module riêng cho nhà xe — bắt buộc ADR         (nhà xe đặc biệt)
   ```

4. **Không copy code thẳng giữa các nhà xe.** Hai nhà xe cần cùng một logic → đưa lên hook/tham số dùng chung.
   Copy chỉ chấp nhận ở lần đầu (mới 2 nhà xe) và phải ghi `derived_from` để sau này gộp.
5. **Tham số truy về tri thức.** Mỗi tham số (vd phụ thu Tết 20%) có nguồn là item tri thức nhà xe.

## 2. Phân tầng logic

| Tầng | Chứa gì | Ví dụ |
|---|---|---|
| **L0 Platform** | chuẩn code, interface của tool, quy tắc an toàn | interface `FareCalculator`, "adapter phải có timeout + retry", "không log SĐT" |
| **L1 Ngành** | **module chuẩn**, **danh mục feature**, **họ logic**, pattern, bài học chung | `fare.standard`, feature `fare.holiday_surcharge`, họ "Có mùa lễ" |
| **L2 Nhà xe** | **hồ sơ logic** + **logic spec**: module nào, tham số gì, hook nào, custom nào | Phương Nam: `fare.standard@2` + phụ thu Tết 20% + hook `weekend_no_stack` |

## 3. Lưu ở đâu

### 3.1 Trong repo git (nguồn sự thật của code)

```
biva-integrations/
├── modules/                         # L1: module chuẩn
│   ├── fare/standard/
│   │   ├── module.yaml              # manifest
│   │   ├── calculator.py
│   │   └── tests/
│   ├── booking/hold/
│   └── schedule/sync_excel/
├── hooks/shared/                    # hook dùng chung (sau khi được promote từ nhiều nhà xe)
└── operators/                       # L2
    ├── phuongnam/
    │   ├── profile.yaml             # hồ sơ logic
    │   ├── hooks/pickup_by_hour.py  # bậc ②
    │   ├── custom/transfer_q7.py    # bậc ③
    │   └── tests/cases.yaml         # ví dụ thật của nhà xe (input → output)
    └── hoanglong/
        └── profile.yaml             # chỉ config, không có code riêng
```

**`module.yaml`** — manifest nằm cạnh code:

```yaml
id: fare.standard
version: 2
capability: fare
layer: L1
summary: Giá theo tuyến × loại xe × mùa, có phụ thu ngày lễ và chính sách trẻ em
entrypoint: calculator.py:calculate
features: [fare.by_route_vehicle, fare.holiday_surcharge, fare.child_policy]   # từ danh mục L1
params_schema:
  holiday_surcharge: {type: object, description: "hệ số theo dịp: tet, le_30_4…"}
  child_policy: {enum: [free_under_6, half_over_4, full]}
hooks:
  - name: adjust_price
    when: "sau khi tính giá gốc, trước khi áp phụ thu"
required_tests: [tests/test_holiday.py]
deprecated_by: null
```

**`profile.yaml`** — hồ sơ logic của nhà xe; tham số ghi kèm item nguồn:

```yaml
operator: phuongnam
capabilities:
  fare:
    mode: hook
    module: fare.standard@2
    params:
      holiday_surcharge: {value: {tet: 0.20}, source: "1c725bb6-cf77-4e81-bc66-dd29f32536d9"}
      child_policy: {value: half_over_4, source: "0f9d2b31-8a44-4d2e-9c1f-5b6e7d80a112"}
    hooks: {adjust_price: hooks/weekend_no_stack.py:apply}
    derived_from: null
  pickup:
    mode: hook
    module: pickup.assign@1
    hooks: {resolve_point: hooks/pickup_by_hour.py:resolve}
    decision: adr_017
  transfer:
    mode: custom
    entrypoint: custom/transfer_q7.py:handle
    decision: adr_021
decisions:
  adr_021:
    title: Trung chuyển Q7 bằng shuttle riêng
    date: 2026-09-12
    context: module transfer chuẩn không cover cụm sau 20h ở Q7
    decision: custom riêng, theo dõi để promote khi đủ 3 nhà xe
```

**`tests/cases.yaml`** — ví dụ thật của nhà xe, vừa là logic test vừa là dữ liệu so khớp hành vi (§5.3):

```yaml
capability: fare
cases:
  - {in: {route: SGN-DLI, seat: sleeper, date: "2027-02-04"}, out: 450000, note: "28 Tết", source: it_118}
  - {in: {route: SGN-DLI, seat: sleeper, date: "2026-10-03"}, out: 330000, note: "thứ 7"}
```

### 3.2 Trong Brain (tri thức về code)

| Đối tượng | Nguồn | Bảng |
|---|---|---|
| Module (L1 chuẩn / L2 custom) | đồng bộ từ `module.yaml` | `logic_modules` |
| Hồ sơ logic của nhà xe | đồng bộ từ `profile.yaml` (hoặc đề xuất qua MCP → PR) | `logic_profiles`, `logic_param_sources` |
| **Danh mục feature** | L1, lớn dần qua review | `logic_features` |
| **Logic spec** của nhà xe | trích từ tri thức nhà xe + hồ sơ (§4) | `logic_specs` |
| **Họ logic** | gom cụm spec (§6) | `logic_families` |
| Ví dụ / logic test | đồng bộ từ `tests/cases.yaml` + ví dụ nhà xe gửi | `logic_tests` |
| ADR | MCP `record_decision` | `logic_decisions` |
| Bài học về code | MCP `add_lesson` | `items` (kind=lesson, topic=`code:<capability>`) |
| Index code | job `index_code` | `code_chunks` |

### 3.3 Đồng bộ từ git

```
push nhánh chính ──tick mỗi phút của scheduler──► Brain: enqueue job index_code
  1. module.yaml       → logic_modules
  2. profile.yaml      → logic_profiles + logic_param_sources (đối chiếu item id)
  3. tests/cases.yaml  → logic_tests
  4. code              → code_chunks: cắt theo hàm/lớp (không theo số dòng), kèm tóm tắt ngắn, gắn commit
  5. so lần trước      → module đổi version? tham số lệch tri thức? → stale / impact_of_change
  6. cập nhật          → logic_specs, logic_families (§4, §6)
```

Chunk gắn commit: commit mới → chunk cũ không còn được trả về (cơ chế "quên" cho code).

Job tự no-op khi HEAD đã đồng bộ (bảng `logic_syncs`) → merge PR được Brain cập nhật trong ≤ 1 phút.
Nhà xe chưa onboard trong Brain thì profile và case của nhà xe đó bị bỏ qua và mốc HEAD **không** được
ghi — tick kế thử lại tới khi onboard. Embedding qua TEI; TEI lỗi → chunk vẫn vào (search keyword dùng
được), embedding để NULL và lần sync sau bù.

## 4. Logic spec — "dấu vân tay" logic của nhà xe

Mỗi nhà xe, theo mỗi capability, có một spec chuẩn hoá theo **danh mục feature L1**:

```yaml
operator: phuongnam
capability: fare
features:
  - fare.by_route_vehicle
  - fare.holiday_surcharge        {tet: 0.20, le_30_4: 0.10}
  - fare.child_policy             {value: half_over_4}
  - fare.weekend_surcharge        {sat_sun: 30000, stack_with_holiday: false}
rules_text:                       # quy tắc bằng lời đã chuẩn hoá (để so ngữ nghĩa)
  - "Giá Tết tính theo ngày đi, áp từ 25 tháng Chạp đến mùng 6"
  - "Cuối tuần cộng 30k/vé, không cộng dồn với phụ thu Tết"
examples: [lt_301, lt_302, lt_303]
implementation: {mode: hook, module: fare.standard@2, hook: weekend_no_stack}
```

- **Spec có trước code**: với khách mới, job `extract_logic_spec` dựng spec từ tri thức đã duyệt (giá, chính sách,
  ví dụ nhà xe gửi) — `implementation` để trống.
- **Danh mục feature** (`logic_features`) lớn dần: LLM ánh xạ quy tắc vào feature có sẵn; quy tắc chưa khớp
  feature nào → **đề xuất feature mới** (review). Mỗi feature đếm số nhà xe có nó.

## 5. Tìm nhà xe tương tự — 3 tầng

```
Nhà xe mới (chưa có code)
   │ ingest tri thức + ví dụ → extract_logic_spec
   ▼
① SPEC ─── so feature + quy tắc + tham số ──────────────► top 10 ứng viên (có giải thích)
   ▼
② CODE ─── với ứng viên đã có code: embedding + cấu trúc ─► gom "họ logic", phát hiện custom trùng nhau
   ▼
③ HÀNH VI ─ chạy ví dụ của nhà xe mới trên code ứng viên ─► xếp hạng theo % pass; case fail = phần cần viết thêm
```

### 5.1 Tương đồng theo spec

```
sim(A, B) = 0.5 × feature_overlap(A, B)       # trùng feature, có trọng số theo độ hiếm
          + 0.3 × rule_text_similarity(A, B)  # embedding các quy tắc bằng lời
          + 0.2 × param_closeness(A, B)       # tham số gần nhau (20% vs 25%)
```

**Trọng số theo độ hiếm** (giống IDF): feature mà gần như nhà xe nào cũng có (vd `fare.by_route_vehicle`)
gần như không đóng góp; feature hiếm cùng có (vd "trung chuyển theo cụm sau 20h") là tín hiệu mạnh.
Trọng số và ngưỡng được tinh chỉnh khi có dữ liệu thật.

### 5.2 Tương đồng theo code

- Embedding của code chunk + so cấu trúc sau khi chuẩn hoá tên biến.
- Dùng chủ yếu để **gom các custom/hook giống nhau** thành họ → đề xuất promote khi ≥ 3 nhà xe;
  và phát hiện hai nhà xe "tưởng khác mà code giống".

### 5.3 Tương đồng theo hành vi (đáng tin nhất)

- Chạy **ví dụ thật** của nhà xe mới (input → output) trên **cách triển khai của từng ứng viên**.
- Ứng viên pass nhiều nhất = điểm xuất phát tốt nhất; case fail chỉ ra **đúng phần phải viết thêm**.
- **Sandbox bắt buộc**: không mạng, giới hạn CPU/RAM/thời gian, chỉ chạy hàm thuần với input giả lập.
  Adapter gọi API thật của nhà xe **không** chạy kiểu này — chỉ so ở tầng spec và code.

## 6. Họ logic

Brain gom spec theo capability thành **họ logic** (cụm). Ví dụ với tính giá:

| Họ | Đặc điểm | Số nhà xe | Triển khai đề xuất |
|---|---|---|---|
| Cơ bản | giá theo tuyến × loại xe | 18 | config `fare.standard` |
| Có mùa lễ | + phụ thu Tết/lễ | 21 | config `fare.standard` + `holiday_surcharge` |
| Cuối tuần không cộng dồn | + phụ thu cuối tuần, không cộng với Tết | 6 | hook `weekend_no_stack` (ứng viên promote) |
| Giá động | giá theo số ghế còn trống | 2 | custom + ADR |

Khách mới thường rơi vào một họ có sẵn → câu hỏi đầu tiên là "thuộc họ nào", thường mất vài phút.

## 7. Luồng triển khai cho khách mới

```
/implement_operator_logic (MCP)
  1. get_logic_spec(capability)          → spec trích từ tri thức + ví dụ
  2. find_similar_operators(capability)  → ứng viên kèm giải thích:
        Phương Nam  92%  trùng: holiday_surcharge, child_half_over_4 · thiếu: vip_seat_surcharge
        Hoàng Long  81%  trùng: holiday_surcharge · khác: child_free_under_6
  3. run_examples_against(candidates)    → Phương Nam pass 9/10, Hoàng Long 6/10
  4. plan_logic_implementation           → kế hoạch:
        · fare.standard@2 (config), tham số lấy từ tri thức
        · tái dùng hook weekend_no_stack (đề xuất đưa lên hooks/shared nếu ≥ 3 nhà xe)
        · viết mới: phụ thu ghế VIP (case fail) → tham số mới hoặc hook mới
  5. AI viết profile.yaml + hook trong repo → PR → CI chạy toàn bộ ví dụ → merge
  6. Brain đồng bộ: cập nhật spec, họ logic; feature mới lặp ở ≥ 3 nhà xe → đề xuất promote
```

## 8. Giống nhau, đặc biệt, và "quên"

| Tình huống | Brain làm |
|---|---|
| Nhiều nhà xe dùng cùng module, tham số tương tự | tăng `proof_count`; gợi ý giá trị mặc định cho L1 |
| **≥ 3 nhà xe** có custom/hook làm cùng một việc (cùng họ) | **đề xuất promote** thành tham số / hook chuẩn, kèm danh sách nhà xe được lợi |
| Nhà xe thật sự đặc biệt | giữ custom ở L2 + ADR; không kéo L1 theo |
| Code copy từ nhà xe khác (`derived_from`) | theo dõi; đủ 3 bản → ưu tiên gộp |
| Module L1 đổi version | `impact_of_change`: nhà xe đang dùng bản cũ, test phải chạy lại |
| Item tri thức là nguồn tham số bị thay/hết hạn | hồ sơ + spec → `stale`; AI được báo sửa tham số |
| Commit mới | re-index chunk; chunk commit cũ không còn trả về |
| Custom được thay bằng module L1 sau promote | custom → `retired`, giữ lịch sử + ADR |

## 9. MCP tools (logic)

| Tool | Cờ | Mô tả |
|---|---|---|
| `get_operator_logic` | RO | hồ sơ logic, capability còn thiếu, chỗ stale |
| `get_logic_spec` | RO | logic spec theo capability (có cả với khách chưa có code) |
| `find_similar_operators` | RO | ứng viên tương tự kèm điểm, feature trùng/thiếu/khác, họ logic |
| `run_examples_against` | | chạy ví dụ của nhà xe này trên code ứng viên (sandbox) → % pass, case fail |
| `compare_logic` | RO | so hai nhà xe theo spec, tham số, code |
| `plan_logic_implementation` | RO | kế hoạch: tái dùng gì, config/hook/custom, phần viết mới |
| `list_logic_families` | RO | họ logic theo capability |
| `search_logic` | RO | tìm module, feature, pattern, lesson, code chunk |
| `get_logic_module` | RO | manifest: interface, params_schema, hooks, version, test bắt buộc |
| `propose_logic_profile` | | đề xuất hồ sơ (config/hook/custom) → tạo PR vào repo hoặc review (custom kèm ADR) |
| `record_decision` | | ghi ADR |
| `add_logic_test` / `list_logic_tests` | | ví dụ input → output |
| `impact_of_change` | RO | module/version/feature hoặc item đổi → nhà xe, bot, test bị ảnh hưởng |

Prompt: `/implement_operator_logic` — đi theo §7.

## 10. Nối với bot

`tool_spec` của bot ([mcp.md](mcp.md)) trỏ tới **capability** trong hồ sơ logic (vd `get_fare` → `fare.standard@2`
với tham số của Phương Nam). `validate_artifact` kiểm tra: tool bot gọi phải có capability tương ứng ở trạng thái `active`.
