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
    with path.open(encoding="utf-8-sig", newline="") as f:  # BOM từ Excel
        rows = [r for r in csv.DictReader(f) if any((v or "").strip() for v in r.values())]
    if not rows:
        sys.exit(f"{path}: không có dòng dữ liệu")
    return rows


def fare_items(rows: list[dict]) -> list[dict]:
    items = []
    for r in rows:
        tuyen = (r["tuyen"] or "").strip()
        loai = (r["loai_ghe"] or "").strip().lower()
        gia = int(str(r["gia_ve"] or "0").replace(".", "").replace(",", ""))
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


def submit(api: str, token: str, operator: str, items: list[dict], source: str) -> dict:
    req = urllib.request.Request(
        f"{api.rstrip('/')}/mcp/operator/{operator}/",
        data=json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": "submit_knowledge", "arguments": {
                "source": source,
                "source_excerpt": f"nạp pilot kit {date.today().isoformat()}",
                "items": items,
            }},
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
        sys.exit(f"submit_knowledge lỗi: {text[:500]}")
    sc = result.get("structuredContent") or {}
    return {"operation_id": sc.get("operation_id"),
            "next": sc.get("next_actions", [])}


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://localhost:8080")
    ap.add_argument("--operator", required=True)
    ap.add_argument("--token", required=True)
    ap.add_argument("files", nargs="+", type=Path,
                    help="bang_gia.csv và/hoặc lich_chay.csv")
    a = ap.parse_args()
    _self_check()

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
