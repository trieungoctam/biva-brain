# -*- coding: utf-8 -*-
"""Đo recall trên golden set (DYN-112): mỗi câu hỏi phải tìm thấy item mong đợi trong top-k.

Golden set = câu hỏi khách thật + item tri thức đúng để trả lời. Đây là dụng cụ đo AC
"recall ≥ 90%": chạy từng câu qua MCP recall_knowledge, điểm pass nếu item mong đợi
(text khớp theo fold của key hoặc id) nằm trong k kết quả đầu.

    python3 golden.py --api http://localhost:8080 --operator pilot1 --token biva_... \
        --top 5 golden.csv

CSV: cột `cau_hoi`, `key_du_kien` (key item đúng — rút từ list_knowledge/get_coverage).
In tỷ lệ pass từng câu + tổng; exit code 1 nếu dưới ngưỡng (mặc định 90%).
"""

from __future__ import annotations

import argparse
import csv
import json
import os
import sys
import urllib.request
from pathlib import Path

from nap_du_lieu import _fold  # cùng chuẩn hoá key với loader


def fold_key(k: str) -> str:
    """Fold TỪNG SEGMENT (giữ dấu chấm) — _fold cả chuỗi làm route.diem_dung
    == route.diem.dung, gây pass giả."""
    return ".".join(_fold(seg) for seg in k.split("."))


def recall(api: str, token: str, operator: str, question: str, top: int) -> list[dict]:
    req = urllib.request.Request(
        f"{api.rstrip('/')}/mcp/operator/{operator}/",
        data=json.dumps({
            "jsonrpc": "2.0", "id": 1, "method": "tools/call",
            "params": {"name": "recall_knowledge",
                       "arguments": {"query": question, "max_tokens": 4000}},
        }).encode(),
        headers={"Authorization": f"Bearer {token}",
                 "Content-Type": "application/json",
                 "Accept": "application/json, text/event-stream"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        body = resp.read().decode()
    for line in body.splitlines():
        if line.startswith("data: "):
            body = line[6:]
    d = json.loads(body)
    if "error" in d:
        sys.exit(f"MCP lỗi: {d['error']}")
    r = d["result"]
    if r.get("isError"):
        sys.exit(f"recall lỗi: {(r.get('content') or [{}])[0].get('text', '')[:300]}")
    sc = r.get("structuredContent") or {}
    return sc.get("items", [])[:top]


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default="http://localhost:8080")
    ap.add_argument("--operator", required=True)
    ap.add_argument("--token", default=os.environ.get("BIVA_TOKEN", ""),
                    help="mặc định lấy từ biến môi trường BIVA_TOKEN (không lộ qua ps/history)")
    ap.add_argument("--top", type=int, default=5, help="điểm pass nếu item đúng trong top-k")
    ap.add_argument("--pass-rate", type=float, default=0.9, help="ngưỡng AC (mặc định 0.90)")
    ap.add_argument("golden", type=Path)
    a = ap.parse_args()
    if not a.token:
        ap.error("cần token: đặt biến môi trường BIVA_TOKEN=biva_... hoặc dùng --token")
    if a.api.startswith("http://") and "localhost" not in a.api and "127.0.0.1" not in a.api:
        print("CẢNH BÁO: --api là http:// ngoài localhost — token đi cleartext!", file=sys.stderr)

    rows = [r for r in csv.DictReader(a.golden.open(encoding="utf-8-sig"))
            if (r.get("cau_hoi") or "").strip()]
    if not rows:
        sys.exit(f"{a.golden}: không có câu hỏi")

    passed = 0
    for i, r in enumerate(rows, 1):
        q = r["cau_hoi"].strip()
        raw_want = (r.get("key_du_kien") or "").strip()
        if not raw_want:
            print(f"? [{i}] thiếu key_du_kien — CSV cần quote câu chứa dấu phẩy")
        want = fold_key(raw_want)
        hits = recall(a.api, a.token, a.operator, q, a.top)
        got_keys = [fold_key(h.get("key") or "") for h in hits]
        ok = want in got_keys
        passed += ok
        mark = "✓" if ok else "✗"
        best = hits[0].get("key", "-") if hits else "(không có kết quả)"
        print(f"{mark} [{i}/{len(rows)}] {q[:60]}  muốn={r.get('key_du_kien')}  top1={best}")

    rate = passed / len(rows)
    verdict = "ĐẠT" if rate >= a.pass_rate else "KHÔNG ĐẠT"
    print(f"\nrecall golden set: {passed}/{len(rows)} = {rate:.0%} (top-{a.top}) — {verdict} "
          f"(ngưỡng {a.pass_rate:.0%})")
    if rate < a.pass_rate:
        sys.exit(1)


if __name__ == "__main__":
    main()
