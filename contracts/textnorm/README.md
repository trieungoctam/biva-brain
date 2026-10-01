# textnorm — chuẩn hoá tiếng Việt cho tìm kiếm (spec)

Go (`go/internal/textnorm`) chuẩn hoá **query**, Python (`biva_worker/textnorm.py`) ghi **`search_text`** cho item.
Hai bên phải cho kết quả **giống hệt từng byte**: kiểm bằng `cases.jsonl` (≥ 200 câu) ở cả hai test suite.
Đổi thuật toán = đổi spec này + fixture + cả hai bản cài đặt + re-index `search_text`.

## Thuật toán

Đầu vào: chuỗi UTF-8 bất kỳ.

1. **NFKD** (gộp dạng tương thích: chữ full-width `Ａ`→`A`, `²`→`2`; tách dấu khỏi chữ).
2. **Bỏ mọi ký tự thuộc nhóm Unicode `Mn`** (dấu thanh, dấu mũ, dấu móc của ư/ơ...).
3. **Đổi `đ Đ ð Ð` → `d`** (Ð/ð U+00D0/U+00F0 hay bị gõ nhầm thay Đ).
4. **Chữ hoa ASCII → chữ thường** (`A`–`Z` → `a`–`z`). Không dùng lowercase Unicode (Go và Python khác nhau ở vài ký tự).
5. **Bỏ dấu phân cách hàng nghìn**: ký tự `.` hoặc `,` bị bỏ khi đứng ngay sau một chữ số ASCII và ngay sau nó là
   **đúng 3** chữ số ASCII rồi tới hết chuỗi hoặc một ký tự không phải chữ số.
   Xét trên chuỗi sau bước 4, từng vị trí theo chuỗi gốc (không xét lại chuỗi đã sửa).
   `1.500.000đ` → `1500000d`; `3.14` giữ (thành 2 token `3`, `14`); `1,2345` giữ.
6. **Tách token**: token = chuỗi liên tiếp dài nhất gồm `a`–`z`, `0`–`9`. Mọi ký tự khác là phân cách
   (khoảng trắng, dấu câu, emoji, chữ không phải Latin...).

Đầu ra:

| Hàm | Kết quả |
|---|---|
| `fold(s)` | các token nối bằng một dấu cách — `"Xe giường nằm, 8h30"` → `"xe giuong nam 8h30"` |
| `search_text(s)` | `fold(s)`, rồi (nếu ≥ 2 token) một dấu cách và các bigram `t[i]_t[i+1]` nối bằng dấu cách — `"Xe giường nằm"` → `"xe giuong nam xe_giuong giuong_nam"` |

`search_text` được index bằng `to_tsvector('simple', …)`; query đi qua cùng `search_text` nên khớp được cả gõ có dấu và
không dấu, và bigram giúp ưu tiên cụm âm tiết đúng thứ tự ("giường nằm" khác "nằm giường").

## Fixture

`cases.jsonl`: mỗi dòng `{"input": …, "fold": …, "search_text": …}`. Có cả input dạng NFC và NFD, chữ hoa có dấu,
số tiền, giờ, emoji, full-width, ký tự không phải Latin, chuỗi rỗng.

Thêm câu: sửa `gen_cases.py` → `python/.venv/bin/python contracts/textnorm/gen_cases.py` → đọc kỹ diff của
`cases.jsonl` (kết quả phải đúng theo spec ở trên, không chỉ "đúng theo code") → chạy test cả hai bên.
