# Pilot kit — nạp dữ liệu nhà xe vào BIVA Brain (DYN-110)

Bộ này biến việc chuẩn bị dữ liệu 3 nhà xe pilot thành cơ học: điền 2 file CSV theo mẫu,
chạy 1 script. Không cần GEMINI key (đường chính `submit_knowledge` không dùng LLM).

## Nhà xe cần chuẩn bị gì

1. **Bảng giá** — điền `bang_gia_template.csv` (mỗi tuyến × loại ghế một dòng; giá mùa cao
   điểm/Tết điền thêm `ap_dung_tu`/`ap_dung_den`). File thật từ Excel cũng được, giữ nguyên tên cột.
2. **Lịch chạy** — điền `lich_chay_template.csv` (cột chuẩn của module `schedule.sync_excel`:
   `tuyen,gio,loai_xe,ngay_chay`; `ngay_chay` nhận "hằng ngày" hoặc "2,4,6" hoặc "7,cn").
3. **Chính sách** (thú nuôi, hành lý, hoàn đổi vé…) — copy nguyên tin nhắn Zalo/thông báo
   vào 1 file text. Phần này nạp bằng `ingest` (trích bằng LLM) — cần GEMINI key; hoặc
   AI builder tự trích thành item rồi `submit_knowledge`.

## Nạp

```bash
python3 nap_du_lieu.py --api http://localhost:8080 --operator <tên_nhà_xe> \
    --token biva_<token_builder> bang_gia.csv lich_chay.csv
```

Giá/lịch là topic rủi ro cao (fare/schedule) → vào **review queue chờ duyệt**, không tự áp
(đúng thiết kế an toàn). Sau khi `get_operation` thấy done: `list_review_queue` →
`get_review_item` → `apply_review` (preview + confirm_token).

## Sau nạp

`get_coverage` xem độ phủ mục bắt buộc → đủ thì chạy prompt `build_bot` → `run_tests` →
`check_release_gate` → `request_publish`.

## Lưu ý vận hành (kiểm chứng trên stack local)

- **Idempotency 2 tầng**: nạp lại file có nội dung y hệt là no-op (documents trùng bị
  dedupe; operations giữ idempotency key). Muốn nạp lại sau khi đã xoá dữ liệu thử:
  đổi nội dung file hoặc xoá cả operations của nhà xe đó.
- **Giá mùa**: dòng có `ap_dung_tu` tự nhận key riêng (`...tu_<ngày>_den_<ngày>`) — cùng
  key với giá thường trong một tin sẽ bị đánh CONFLICT (an toàn, builder phải tự chọn).
- Đã chạy thử end-to-end: nạp 12 item → queue 12 review NEW rủi ro cao → duyệt giá Tết qua
  `apply_review` (preview + confirm_token) → item active đúng khoảng hiệu lực.
