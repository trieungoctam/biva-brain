# Pilot kit — nạp dữ liệu nhà xe vào BIVA Brain (DYN-110)

Bộ này biến việc chuẩn bị dữ liệu 3 nhà xe pilot thành cơ học: điền 2 file CSV theo mẫu,
chạy 1 script. Không cần GEMINI key (đường chính `submit_knowledge` không dùng LLM).

## Nhà xe cần chuẩn bị gì

1. **Bảng giá** — điền `bang_gia_template.csv` (mỗi tuyến × loại ghế một dòng; giá mùa cao
   điểm/Tết điền thêm `ap_dung_tu`/`ap_dung_den`). File thật từ Excel cũng được, giữ nguyên tên cột.
2. **Lịch chạy** — điền `lich_chay_template.csv` (cột chuẩn của module `schedule.sync_excel`:
   `tuyen,gio,loai_xe,ngay_chay`; `ngay_chay` nhận "hằng ngày" hoặc "2,4,6" hoặc "7,cn").
3. **Chính sách** — điền `chinh_sach_template.csv` (topic,key,text). Topic rủi ro thấp
   (pets/luggage/contact…) **tự áp dụng** ngay; topic chạm tiền (cancellation…) chờ duyệt.
   Nguồn太难 trích (ảnh, ghi âm) mới cần `ingest` + GEMINI key.

## Nạp

```bash
python3 nap_du_lieu.py --api http://localhost:8080 --operator <tên_nhà_xe> \
    --token biva_<token_builder> bang_gia.csv lich_chay.csv chinh_sach.csv
```

Xem độ phủ mục bắt buộc bất cứ lúc nào (theo template ngành, kèm mục còn thiếu và cần gì):

```bash
python3 nap_du_lieu.py --api ... --operator ... --token ... --coverage
```
`✓` đã có tri thức riêng · `≈` đang dùng thông lệ chung (L1) · `?` mâu thuẫn mở · `✗` còn thiếu.

Giá/lịch là topic rủi ro cao (fare/schedule) → vào **review queue chờ duyệt**, không tự áp
(đúng thiết kế an toàn). Sau khi `get_operation` thấy done: `list_review_queue` →
`get_review_item` → `apply_review` (preview + confirm_token).

## Sau nạp

`get_coverage` xem độ phủ mục bắt buộc → đủ thì chạy prompt `build_bot` → `run_tests` →
`check_release_gate` → `request_publish`.

## Đo recall trên golden set (DYN-112)

`golden_template.csv` (cau_hoi, key_du_kien — quote câu chứa dấu phẩy) + `golden.py`:
chạy từng câu qua `recall_knowledge`, pass nếu item đúng trong top-k (mặc định 5), exit 1
khi dưới ngưỡng (mặc định 90%). Thay câu hỏi bằng câu KHÁCH THẬT của nhà xe khi có pilot.

Finding đầu tiên từ chính bộ mẫu (13 câu, 92% top-5, keyword-only local): query chứa token
hiếm ("Hotline") vẫn bị L0/L1 boilerplate (chứa "nhà xe") tràn xếp hạng — chế độ keyword-only
báo `degraded` đúng thiết kế; đo AC chính thức cần TEI bật (production). Tune xếp hạng phải
đợi corpus golden thật, không tune trên N=1.

## File Excel/CSV "bẩn" cũng ăn

Loader chịu được: BOM, CRLF, delimiter `;` (Excel locale Việt Nam), khoảng trắng thừa,
cột thừa bị bỏ, giá dạng `320000` · `320.000đ` · `350,000 VNĐ` (tự bỏ chữ/sep).
Giá không đọc được sẽ báo dòng thân thiện thay vì traceback. Đã test bằng file có đủ
thứ bẩn trên đó nạp thật vào stack.

## Lưu ý vận hành (kiểm chứng trên stack local)

- **Idempotency 2 tầng**: nạp lại file có nội dung y hệt là no-op (documents trùng bị
  dedupe; operations giữ idempotency key). Muốn nạp lại sau khi đã xoá dữ liệu thử:
  đổi nội dung file hoặc xoá cả operations của nhà xe đó.
- **Giá mùa**: dòng có `ap_dung_tu` tự nhận key riêng (`...tu_<ngày>_den_<ngày>`) — cùng
  key với giá thường trong một tin sẽ bị đánh CONFLICT (an toàn, builder phải tự chọn).
- Đã chạy thử end-to-end: nạp 12 item giá/lịch → queue 12 review NEW rủi ro cao → duyệt giá
  Tết qua `apply_review` (preview + confirm_token) → item active đúng khoảng hiệu lực; nạp 5
  chính sách → pets/luggage/contact **tự áp dụng** + consolidate tự gom observation,
  cancellation **chờ duyệt** (đúng phân loại rủi ro theo topic).
- Loader có self-check chuẩn hoá key lúc khởi động (khớp chuẩn `textnorm` của Brain — đã đối
  chiếu 8/8 fixture `contracts/textnorm/keys.jsonl`); Python/Unicode lạ trên máy nạp sẽ báo
  ngay thay vì tạo key lệch.
