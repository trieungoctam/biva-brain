"""Sinh contracts/textnorm/cases.jsonl (chạy từ gốc repo: ai-worker/.venv/bin/python contracts/textnorm/gen_cases.py).

Kết quả kỳ vọng tính bằng bản Python; bản Go kiểm độc lập. Thêm câu → chạy lại → đọc kỹ diff trước khi commit.
"""
import json, sys, unicodedata
sys.path.insert(0, "ai-worker")
from biva_worker.textnorm import fold, search_text

base = """Xe giường nằm 40 chỗ chạy tuyến Sài Gòn – Đà Lạt mỗi ngày.
Giá vé từ 250.000đ đến 350.000đ tuỳ giờ.
Nhà xe Phương Trang có bao nhiêu chuyến đi Cần Thơ?
Chuyến 22h30 xuất phát từ bến xe Miền Đông mới.
Khách được mang tối đa 20kg hành lý miễn phí.
Thú cưng phải để trong lồng, phụ thu 50.000 đồng.
Trẻ em dưới 5 tuổi được miễn vé nếu ngồi chung ghế với bố mẹ.
Huỷ vé trước 24 giờ được hoàn 90% tiền vé.
Đổi vé miễn phí 1 lần, đổi lần 2 mất phí 20.000đ.
Xe limousine 9 chỗ đón trả tận nơi trong nội thành Hà Nội.
Điểm đón: 272 Đề Thám, phường Phạm Ngũ Lão, Quận 1.
Tổng đài đặt vé 1900 6067 hoạt động 24/7.
Xe có wifi, nước uống, khăn lạnh và cổng sạc USB.
Tuyến Hà Nội – Sa Pa chạy cao tốc Nội Bài – Lào Cai.
Giờ khởi hành: 06:00, 08:30, 13:45 và 21:15.
Khách lên xe phải xuất trình CCCD hoặc mã vé điện tử.
Vé Tết 2027 mở bán từ ngày 15/12/2026.
Phụ thu ngày lễ 30% so với giá vé thường.
Ghế VIP tầng dưới giá 450.000 VNĐ.
Có trung chuyển miễn phí từ quận 7, Nhà Bè và Bình Chánh.
Xe dừng nghỉ 20 phút tại trạm Dầu Giây.
Hành lý cồng kềnh như xe máy phải gửi hàng riêng.
Không chở hàng cấm, hàng dễ cháy nổ.
Thanh toán qua MoMo, ZaloPay, VNPay hoặc chuyển khoản.
Khách đặt online được giảm 5%, tối đa 30.000đ.
Chuyến cuối ngày rời bến lúc 23g45.
Đường Hồ Chí Minh đoạn qua Đắk Nông đang sửa, xe đi chậm 1 tiếng.
Bến xe Mỹ Đình có lối vào riêng cho xe giường nằm.
Hoàn tiền trong vòng 3–5 ngày làm việc.
Liên hệ hotline 0909.123.456 khi cần hỗ trợ gấp.
Quy định: không hút thuốc, không uống rượu bia trên xe.
Người khuyết tật được hỗ trợ lên xuống xe.
Đặt vé khứ hồi được giảm thêm 10%.
Xe Thành Bưởi tuyến Sài Gòn – Đà Lạt có 3 loại xe.
Ghế ngồi 29 chỗ, giường nằm 34 phòng, limousine 22 phòng.
Ưu đãi sinh viên giảm 15% khi xuất trình thẻ.
Cước gửi hàng tính theo kg: 10.000đ/kg, tối thiểu 30.000đ.
Hàng gửi được nhận tại văn phòng trong 7 ngày.
Văn phòng Đà Nẵng: 123 Điện Biên Phủ, Thanh Khê.
Tuyến Quy Nhơn – Pleiku chạy qua đèo An Khê.
Xe đón khách dọc Quốc lộ 1A nếu báo trước.
Không nhận đặt chỗ qua tin nhắn Facebook sau 21h.
Chính sách mới áp dụng từ 01/11/2026.
Giá vé cũ hết hiệu lực ngày 31/10/2026.
Nhân viên sẽ gọi xác nhận trước giờ đi 2 tiếng.
Xe khởi hành đúng giờ, khách đến muộn quá 15 phút coi như huỷ vé.
Ghế số 1 và 2 dành cho người cao tuổi.
Mỗi khách được mang một vali và một túi xách.
Trên xe có nhà vệ sinh.
Giường đôi dành cho 2 người, giá 700.000đ.
Ở Huế, điểm đón là bến xe phía Nam.
Tuyến Cà Mau – Sài Gòn chạy đêm, đến nơi lúc 5h sáng.
Khách đi Phú Quốc mua combo vé xe + tàu cao tốc.
Hãng không chịu trách nhiệm tài sản để quên trên xe.
Gặp sự cố, xe sẽ được điều xe khác thay thế trong 2 giờ.
Bảo hiểm hành khách tối đa 100.000.000đ mỗi vụ.
Phương thức nhận vé: SMS, email hoặc Zalo OA.
Trẻ từ 6 đến 10 tuổi mua vé 50%.
Xe trung chuyển chạy từ 5h00 đến 22h00.
Khách hỏi: "xe có ghé Bảo Lộc không?"
Trả lời: Có, xe ghé trạm dừng chân Bảo Lộc 15'.
Tuyến Hải Phòng ↔ Quảng Ninh tăng chuyến dịp hè.
Đặt trước 3 ngày giữ chỗ không cần cọc.
Đặt nhóm trên 10 người liên hệ phòng kinh doanh.
Giá trọn gói thuê xe 16 chỗ: 2,5 triệu/ngày.
Phí cầu đường đã bao gồm trong giá vé.
Khách nước ngoài cần hộ chiếu (passport) khi lên xe.
Không áp dụng đồng thời nhiều mã giảm giá.
Mã KM: TET2027 giảm 20k cho vé đầu tiên.
Thời gian chạy dự kiến 6–7 tiếng tuỳ tình hình giao thông.
Lịch chạy có thể thay đổi khi thời tiết xấu.
Khách say xe có thể xin túi nôn và dầu gió.
Tài xế không nghe điện thoại khi đang lái.
Xe được giám sát hành trình GPS liên tục.
Ưu tiên phụ nữ mang thai chọn ghế tầng dưới.
Giá có thể chênh lệch giữa ngày thường và cuối tuần.
Thứ 6, thứ 7, chủ nhật giá tăng 10%.
Đổi tên người đi miễn phí trước giờ chạy 6 tiếng.
Không hoàn tiền với vé khuyến mãi.
Đại lý vé tại ngã tư Bảy Hiền.
Tuyến Vũng Tàu chạy mỗi 30 phút một chuyến.
Khách cần xuất hoá đơn VAT báo trước khi thanh toán.
Ở Đồng Tháp, xe đón ở cầu Cao Lãnh.
Xe về tới Buôn Ma Thuột khoảng 5g30.
Phòng đôi có rèm che, đèn đọc sách và TV.
Ngày 30/4 – 1/5 tăng 15 chuyến.
Đường dây nóng phản ánh dịch vụ: 1800 1234.
Hệ thống đặt vé bảo trì từ 0h đến 2h.
Ghế được chọn ngẫu nhiên nếu khách không chọn.
Khách VIP được phục vụ bữa nhẹ.""".splitlines()

edge = [
    "", "   ", "!!!", "🚌🚌🚌", "Ａ Ｂ Ｃ １２３", "x²+y²", "ÐỒNG NAI", "ðà nẵng", "İzmir xe", "東京 Tokyo",
    "Xe\tgiường\nnằm", "a.b.c", "1.000", "1.0000", "1,000,000", "10.5km", "0.500", ".500", "1.500.", "12.345.678,90",
    "3,14", "v1.2.3", "8h30-9h45", "100%", "#1 nhà xe", "email: datve@nhaxe.vn", "https://nhaxe.vn/dat-ve?id=12",
    "ƯƠ ươ Ư Ơ", "Ỹ ỹ Ỷ Ỵ", "Ạ Ặ Ậ Ẹ Ệ", "ﬁle", "№5", "½ giá", "Ⅻ", "café", "naïve façade", "ŒUVRE",
    "Mr. Nguyễn Văn A", "xe-khach_ha-noi", "A1B2C3", "giờ: 07:05", "1.5", "01.234", "9.999.999",
]

cases = []
for s in base:
    cases.append(s)
for s in base[:40]:
    cases.append(s.upper())
for s in base[40:80]:
    cases.append(unicodedata.normalize("NFD", s))
for s in base[80:]:
    cases.append("👉 " + s + " ❤️")
cases += edge
assert len(cases) >= 200, len(cases)

with open("contracts/textnorm/cases.jsonl", "w", encoding="utf-8") as f:
    for s in cases:
        f.write(json.dumps({"input": s, "fold": fold(s), "search_text": search_text(s)}, ensure_ascii=False) + "\n")
print(len(cases), "cases")
