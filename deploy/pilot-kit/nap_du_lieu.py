# -*- coding: utf-8 -*-
"""Nạp dữ liệu pilot vào BIVA Brain qua MCP submit_knowledge (đường chính, không LLM).

Cách dùng (đã có token builder của nhà xe + stack chạy):

    python3 nap_du_lieu.py --api http://localhost:8080 --operator pilot1 \\
        --token biva_... bang_gia.csv lich_chay.csv

- bang_gia.csv: cột tuyen,loai_ghe,gia_ve,ap_dung_tu,ap_dung_den (xem bang_gia_template.csv)
  → item kind=data topic=fare, key fare.<tuyến chuẩn hoá>.<loại ghế>, facts gia_ve/tuyen.
  Dòng có ap_dung_tu coi là giá mùa cao điểm (vd Tết) — kèm valid_from/valid_to.
- lich_chay.csv: cột tuyen,gio,loai_xe,ngay_chay (chuẩn module schedule.sync_excel)
  → item kind=data topic=schedule, key schedule.<tuyến>.<gio>, facts gio_xuat_ben/ngay_chay.

Topic fare/schedule là rủi ro cao → vào review queue chờ builder duyệt (đúng thiết kế).
Script in danh sách review_id để duyệt qua MCP (apply_review) hoặc console.
"""

from __future__ import annotations

import argparse
import csv
import json
import re
import sys
import unicodedata
import urllib.request
from datetime import date
from pathlib import Path


def _fold(s: str) -> str:
    """Chuẩn hoá tiếng Việt cho key: bỏ dấu, thường hoá, giữ [a-z0-9_]."""
    s = unicodedata.normalize("NFD", s)
    s = "".join(c for c in s if unicodedata.category(c) != "Mn")
    s = s.lower().replace("đ", "d")
    return re.sub(r"[^a-z0-9]+", "_", s).strip("_")


# Self-check: _fold phải khớp chuẩn textnorm của Brain (contracts/textnorm/keys.jsonl).
# Chạy mỗi lần khởi động — bắt sớm Python/Unicode lạ trên máy nạp (vd Windows + Excel CSV).
_FOLD_CHECK = [
    ("Sài Gòn - Đà Lạt", "sai_gon_da_lat"),
    ("Giường nằm", "giuong_nam"),
    ("Điều kiện", "dieu_kien"),
    ("Quận 1 → Đà Lạt (VIP)", "quan_1_da_lat_vip"),
    ("HOTLINE 1900 6067", "hotline_1900_6067"),
]


def _self_check() -> None:
    bad = [(raw, _fold(raw), want) for raw, want in _FOLD_CHECK if _fold(raw) != want]
    if bad:
        sys.exit("lỗi chuẩn hoá key (Python/Unicode của máy này?): " + "; ".join(
            f"{r!r}→{g!r} (muốn {w!r})" for r, g, w in bad))


def policy_items(rows: list[dict]) -> list[dict]:
    items = []
    for r in rows:
        topic = (r["topic"] or "").strip().lower()
        text = (r["text"] or "").strip()
        if not (topic and text):
            continue
        raw_key = (r.get("key") or "").strip() or text
        parts = [_fold(p) for p in raw_key.split(".") if _fold(p)]
        if parts and parts[0] == topic:  # key đã có topic ở đầu — không lặp lại
            parts = parts[1:]
        key = ".".join([topic] + parts)
        items.append({
            "kind": "policy", "topic": topic, "key": key, "text": text,
        })
    return items


def _read_csv(path: Path) -> list[dict]:
    """Đọc CSV từ Excel Việt Nam: BOM, CRLF, delimiter ',' hoặc ';' (locale), cột thừa."""
    raw = path.read_bytes()
    text = raw.decode("utf-8-sig", errors="replace").lstrip("\ufeff")
    lines = [ln for ln in text.splitlines() if ln.strip()]
    if not lines:
        sys.exit(f"{path}: file rỗng")
    # Locale Excel VN hay xuất ';' — đánh giá bằng dòng đầu, mặc định ','
    delim = ";" if lines[0].count(";") > lines[0].count(",") else ","
    header = [h.strip().lower() for h in lines[0].split(delim)]
    rows = []
    for ln in lines[1:]:
        vals = [v.strip() for v in ln.split(delim)]
        row = dict(zip(header, vals))  # cột thừa bị bỏ, cột thiếu rỗng
        if any(row.values()):
            rows.append(row)
    if not rows:
        sys.exit(f"{path}: không có dòng dữ liệu")
    return rows


def _money(v: str) -> int:
    """'320.000đ' / '350,000 VNĐ' / '320000' → int VND; sai định dạng → sys.exit thân thiện."""
    cleaned = (str(v or "").strip()
               .replace("\u00a0", " ")
               .lower().replace("vnđ", "").replace("vnd", "").replace("đ", "")
               .replace(" ", "").replace(".", "").replace(",", ""))
    if not cleaned.isdigit():
        sys.exit(f"giá vé {v!r} không đọc được — ghi số thuần, vd 320000 (hoặc 320.000đ)")
    return int(cleaned)


def fare_items(rows: list[dict]) -> list[dict]:
    items = []
    for r in rows:
        tuyen = (r["tuyen"] or "").strip()
        loai = (r["loai_ghe"] or "").strip().lower()
        try:
            gia = _money(r["gia_ve"])
        except SystemExit:
            raise
        if not tuyen or gia <= 0:
            continue
        key = f"fare.{_fold(tuyen)}.{_fold(loai)}"
        item = {
            "kind": "data", "topic": "fare", "key": key,
            "text": f"{tuyen} {loai} {gia:,}đ".replace(",", "."),
            "facts": {"tuyen": tuyen, "loai_ghe": loai, "gia_ve": str(gia)},
        }
        # Giá mùa (Tết/hè...): KEY RIÊNG theo quy ước tri thức (vd fare.sg_dl.giuong_nam.tet) —
        # cùng key với giá thường trong một tin sẽ bị diff đánh CONFLICT (đúng an toàn,
        # builder phải tự chọn; key riêng thì hai bản song song theo khoảng hiệu lực).
        if (r.get("ap_dung_tu") or "").strip():
            item["valid_from"] = r["ap_dung_tu"].strip()
            key = f"{key}.tu_{item['valid_from']}"
            if (r.get("ap_dung_den") or "").strip():
                item["valid_to"] = r["ap_dung_den"].strip()
                key = f"{key}_den_{item['valid_to']}"
            item["key"] = key
            item["text"] += f" (áp dụng từ {item['valid_from']})"
        items.append(item)
    return items


def schedule_items(rows: list[dict]) -> list[dict]:
    items = []
    for r in rows:
        tuyen = (r["tuyen"] or "").strip()
        gio = (r["gio"] or "").strip()
        loai = (r["loai_xe"] or "").strip().lower()
        ngay = (r["ngay_chay"] or "").strip().lower()
        if not (tuyen and gio and loai and ngay):
            continue
        key = f"schedule.{_fold(tuyen)}.{gio.replace(':', '')}"
        items.append({
            "kind": "data", "topic": "schedule", "key": key,
            "text": f"{tuyen} khởi hành {gio} ({loai}), chạy: {ngay}",
            "facts": {"tuyen": tuyen, "gio_xuat_ben": gio, "loai_xe": loai,
                      "ngay_chay": ngay},
        })
    return items


def _call(api: str, token: str, operator: str, tool: str, args: dict):
    """Gọi tool MCP của nhà xe; trả (structuredContent, text)."""
    req = urllib.request.Request(
        f"{api.rstrip('/')}/mcp/operator/{operator}/",
        data=json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": tool, "arguments": args},
        }).encode(),
        headers={"Authorization": f"Bearer {token}",
                 "Content-Type": "application/json",
                 "Accept": "application/json, text/event-stream"})
    with urllib.request.urlopen(req, timeout=120) as resp:
        body = resp.read().decode()
    for line in body.splitlines():
        if line.startswith("data: "):
            body = line[6:]
    d = json.loads(body)
    if "error" in d:
        sys.exit(f"MCP lỗi: {d['error']}")
    result = d["result"]
    if result.get("isError"):
        text = result["content"][0]["text"] if result.get("content") else ""
        sys.exit(f"{tool} lỗi: {text[:500]}")
    return result.get("structuredContent") or {}, ""


def submit(api: str, token: str, operator: str, items: list[dict], source: str) -> dict:
    sc, _ = _call(api, token, operator, "submit_knowledge", {
        "source": source,
        "source_excerpt": f"nạp pilot kit {date.today().isoformat()}",
        "items": items,
    })
    return {"operation_id": sc.get("operation_id"),
            "next": sc.get("next_actions", [])}


def coverage(api: str, token: str, operator: str) -> None:
    """In độ phủ mục bắt buộc theo template ngành + gợi ý bước kế tiếp (get_coverage)."""
    sc, text = _call(api, token, operator, "get_coverage", {})
    pct, total = sc.get("required_percent", 0), sc.get("required_total", 0)
    print(f"Độ phủ mục bắt buộc: {sc.get('required_covered', 0)}/{total} ({pct:.0%})")
    for sec in sc.get("sections", []):
        mark = {"covered": "✓", "industry_default": "≈", "ambiguous": "?", "missing": "✗"}.get(
            sec.get("status"), " ")
        extra = ""
        if sec.get("status") == "industry_default":
            extra = " (đang dùng thông lệ chung — có tri thức riêng sẽ tự thay)"
        if sec.get("status") == "ambiguous":
            extra = " (mâu thuẫn đang mở — list_review_queue để chọn)"
        print(f"  {mark} {sec.get('title')} [{sec.get('topic')}] — {sec.get('items', 0)} item{extra}")
        if sec.get("status") == "missing":
            print(f"      cần: {'; '.join(sec.get('facts', [])[:4])}")
    for n in sc.get("next_actions", []):
        print(f"→ {n}")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://localhost:8080")
    ap.add_argument("--operator", required=True)
    ap.add_argument("--token", default=os.environ.get("BIVA_TOKEN", ""),
                    help="mặc định lấy từ biến môi trường BIVA_TOKEN (không lộ qua ps/history)")
    ap.add_argument("--coverage", action="store_true",
                    help="chỉ xem độ phủ mục bắt buộc (get_coverage), không nạp")
    ap.add_argument("files", nargs="*", type=Path,
                    help="bang_gia.csv / lich_chay.csv / chinh_sach.csv")
    a = ap.parse_args()
    if not a.token:
        ap.error("cần token: đặt biến môi trường BIVA_TOKEN=biva_... hoặc dùng --token")
    if a.api.startswith("http://") and "localhost" not in a.api and "127.0.0.1" not in a.api:
        print("CẢNH BÁO: --api là http:// ngoài localhost — token đi cleartext!", file=sys.stderr)
    _self_check()
    if a.coverage:
        coverage(a.api, a.token, a.operator)
        return
    if not a.files:
        ap.error("cần file để nạp, hoặc --coverage để xem độ phủ")

    for f in a.files:
        rows = _read_csv(f)
        name = f.name.lower()
        if "chinh_sach" in name or "policy" in name:
            items, source = policy_items(rows), "other"
        elif "gia" in name:
            items, source = fare_items(rows), "excel"
        elif "lich" in name or "schedule" in name:
            items, source = schedule_items(rows), "excel"
        else:
            sys.exit(f"{f}: không nhận ra loại (tên file cần chứa 'gia' hoặc 'lich')")
        got = submit(a.api, a.token, a.operator, items, source)
        print(f"{f.name}: {len(items)} item → operation {got['operation_id']}")
        for n in got["next"]:
            print(f"  → {n}")
        print("  → theo dõi: get_operation tới khi done, rồi list_review_queue để duyệt")


if __name__ == "__main__":
    main()
