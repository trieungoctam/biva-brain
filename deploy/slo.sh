#!/usr/bin/env bash
# Đo nhanh SLO (docs/runbook.md §7.3) trên stack đang chạy — E-X2 "đo lường cơ bản".
#   BIVA_API=... BIVA_TOKEN=... deploy/slo.sh [số lượt mỗi tool]
set -euo pipefail
API="${BIVA_API:-http://localhost:8080}"
TOK="${BIVA_TOKEN:?đặt BIVA_TOKEN}"
OP="${BIVA_OPERATOR:-pilot1}"
N="${1:-30}"
python3 - "$API" "$TOK" "$OP" "$N" <<'PY'
import json, sys, time, urllib.request
API, TOK, OP, N = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
API = f"{API}/mcp/operator/{OP}/"
def call(tool, args):
    body = json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call",
                       "params":{"name":tool,"arguments":args}}).encode()
    req = urllib.request.Request(API, data=body, headers={
        "Authorization": f"Bearer {TOK}", "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream"})
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=30) as r:
        for line in r:
            if line.startswith(b"data: "): d = json.loads(line[6:])
    return (time.perf_counter()-t0)*1000, d["result"].get("isError", False)
def bench(name, tool, args, target=None):
    lat, errs = [], 0
    for _ in range(N):
        ms, err = call(tool, args); lat.append(ms); errs += err
    lat.sort()
    p50, p95 = lat[len(lat)//2], lat[int(len(lat)*0.95)-1]
    mark = "" if not target else ("ĐẠT" if p95 < target and errs == 0 else "VƯỢT")
    tgt = f"  (mục tiêu <{target}ms)" if target else ""
    print(f"{name:28} p50={p50:7.1f}ms p95={p95:7.1f}ms err={errs}/{N}  {mark}{tgt}")
bench("recall_knowledge", "recall_knowledge", {"query":"giá vé Sài Gòn Đà Lạt","max_tokens":600}, 150)
bench("get_knowledge_pack", "get_knowledge_pack", {}, 800)
bench("validate_artifact (tĩnh)", "validate_artifact", {"kind":"faq"}, 300)
bench("get_coverage", "get_coverage", {})
bench("query_data", "query_data", {"topics":["fare"]})
PY
