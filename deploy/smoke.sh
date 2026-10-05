#!/usr/bin/env bash
# Smoke test môi trường compose: make up chạy đủ service, đi được luồng MCP → queue → ai-worker.
#   make smoke            (dựng từ đầu, giữ lại sau khi chạy; make clean để xoá)
#   SMOKE_SKIP_TEI=1 make smoke   bỏ qua TEI (lần đầu tải model bge-m3 ~2 GB)
set -euo pipefail
cd "$(dirname "$0")"
C="docker compose -f docker-compose.yml"
API=http://127.0.0.1:8080

wait_for() { # mô tả, số giây, lệnh...
  local what=$1 secs=$2; shift 2
  for _ in $(seq "$secs"); do
    if "$@" >/dev/null 2>&1; then echo "✓ $what"; return 0; fi
    sleep 1
  done
  echo "✗ $what (quá ${secs}s)"; $C ps; $C logs --tail=50; exit 1
}

services=(postgres redis s3 migrate brain-api ai-worker)
[[ "${SMOKE_SKIP_TEI:-}" == 1 ]] || services+=(tei-embed)
$C up -d --build "${services[@]}"

wait_for "brain-api /health/ready" 120 curl -fsS "$API/health/ready"
wait_for "S3 (SeaweedFS) trả lời ListBuckets" 60 bash -c "curl -sS 127.0.0.1:8333/ | grep -q ListAllMyBucketsResult"

# Tri thức nền L0/L1 từ kb/ (image brain-api) — chạy hai lần: lần hai không được đổi gì.
# TEI phải READY TRƯỚC kb sync: sync trigger job index.items cần embedding; chạy sớm hơn
# từng làm job cạn 5 lần retry (~20s) trong khi model bge-m3 load mất phút (r80).
if [[ "${SMOKE_SKIP_TEI:-}" != 1 ]]; then
  wait_for "TEI embed (bge-m3, CPU) — trước kb sync" 900 curl -fsS 127.0.0.1:8081/embed \
    -H 'Content-Type: application/json' -d '{"inputs":"xe giường nằm"}'
fi

$C exec -T brain-api brain-api kb sync
$C exec -T brain-api brain-api kb sync | grep -q "thêm 0, sửa 0, bỏ 0" && echo "✓ kb sync idempotent" \
  || { echo "✗ kb sync lần hai vẫn ghi"; exit 1; }

# Dữ liệu thử: nhà xe, builder, token.
sfx=$(date +%s)
$C exec -T brain-api brain-api operator add "smoke$sfx" "Smoke $sfx"
$C exec -T brain-api brain-api user add "smoke$sfx" --email "smoke$sfx@biva.local" --name Smoke --role builder
$C exec -T brain-api brain-api user grant "smoke$sfx" "smoke$sfx"
# Retry 1 lần: trên runner CI từng có lượt exit 255 không output ngay sau grant (flake
# hạ tầng docker/runner) — marker + retry giúp lần tái phát biết chính xác lệnh nào chết.
capture() { # capture <mô tả> <lệnh...> — marker ra STDERR (stdout bị $() nuốt), retry 1 lần
  local what=$1; shift
  echo "→ $what" >&2
  "$@" || { echo "✗ $what lỗi lần 1 — thử lại" >&2; "$@"; }
}
token=$(capture "token issue" bash -c   "$C exec -T brain-api brain-api token issue 'smoke$sfx' --name smoke --ttl 1h | tail -1")

# Job ping → ai-worker phải xử lý xong.
job=$(capture "psql insert operations" bash -c \
  "$C exec -T postgres psql -U biva -d biva -Atc \"INSERT INTO operations (kind, operator_id) VALUES ('system.ping', 'smoke$sfx') RETURNING id\" | head -1")
wait_for "ai-worker xử lý job $job" 60 bash -c \
  "$C exec -T postgres psql -U biva -d biva -Atc \"SELECT status FROM operations WHERE id='$job'\" | grep -qx done"

# MCP: đọc job qua get_operation với token builder; sai phạm vi → 403.
mcp() {
  curl -fsS "$API/mcp/operator/smoke$sfx/" -H "Authorization: Bearer $token" \
    -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "$1"
}
mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' >/dev/null
out=$(mcp "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"get_operation\",\"arguments\":{\"operation_id\":\"$job\"}}}")
grep -q '"status":"done"' <<<"$out" && echo "✓ MCP get_operation trả status done" || { echo "✗ MCP: $out"; exit 1; }
# OAuth (ChatGPT connector): metadata + 401 trỏ tới protected resource metadata.
curl -fsS "$API/.well-known/oauth-authorization-server" | grep -q '"registration_endpoint"' \
  && echo "✓ OAuth metadata" || { echo "✗ OAuth metadata"; exit 1; }
curl -s -D - -o /dev/null -X POST "$API/mcp/operator/smoke$sfx/" | grep -qi 'resource_metadata=' \
  && echo "✓ 401 có WWW-Authenticate resource_metadata" || { echo "✗ thiếu WWW-Authenticate"; exit 1; }
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/mcp/operator/khac$sfx/" -H "Authorization: Bearer $token" \
  -H "Content-Type: application/json" -d '{}')
[[ "$code" == 403 ]] && echo "✓ sai phạm vi → 403" || { echo "✗ sai phạm vi → $code"; exit 1; }

if [[ "${SMOKE_SKIP_TEI:-}" != 1 ]]; then
  dim=$(curl -fsS 127.0.0.1:8081/embed -H 'Content-Type: application/json' -d '{"inputs":"xe"}' \
    | python3 -c 'import json,sys; print(len(json.load(sys.stdin)[0]))')
  [[ "$dim" == 1024 ]] && echo "✓ embedding 1024 chiều (khớp items.embedding)" || { echo "✗ dim=$dim"; exit 1; }

  # Job index.items: ai-worker → TEI thật → search_text + embedding; tìm được bằng query không dấu.
  psql_q() { $C exec -T postgres psql -U biva -d biva -Atc "$1"; }
  item=$(psql_q "INSERT INTO items (layer, operator_id, kind, topic, text, status)
    VALUES (2, 'smoke$sfx', 'policy', 'hanh_ly', 'Mỗi khách được mang 20kg hành lý miễn phí', 'active')
    RETURNING id" | head -1)
  psql_q "INSERT INTO operations (kind, operator_id) VALUES ('index.items', 'smoke$sfx')" >/dev/null
  wait_for "index.items ghi embedding cho item $item" 120 bash -c \
    "$C exec -T postgres psql -U biva -d biva -Atc \"SELECT embedding IS NOT NULL FROM items WHERE id='$item'\" | grep -qx t"
  hit=$(psql_q "SELECT count(*) FROM items WHERE id='$item' AND tsv @@ plainto_tsquery('simple', 'hanh ly mien phi')")
  [[ "$hit" == 1 ]] && echo "✓ tìm được item bằng query không dấu" || { echo "✗ không tìm thấy item"; exit 1; }

  # recall_knowledge qua MCP: brain-api nhúng query bằng TEI thật → item phải được nhánh semantic tìm ra.
  out=$(mcp '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall_knowledge","arguments":{"query":"khách mang theo bao nhiêu ký đồ"}}}')
  ITEM=$item python3 -c '
import json, os, sys
raw = sys.stdin.read()
msg = json.loads(next((l[5:] for l in raw.splitlines() if l.startswith("data:")), raw))
res = msg["result"]["structuredContent"]
hit = next((h for h in res["items"] if h["id"] == os.environ["ITEM"]), None)
assert hit and "semantic" in hit["arms"] and not res.get("degraded"), res
print("✓ recall_knowledge: nhánh semantic (TEI) tìm ra item, %d ms" % res["took_ms"])
' <<<"$out" || { echo "✗ recall_knowledge: $out"; exit 1; }

  # Regression xếp hạng (golden set, câu "hiếm token"): keyword-only từng cho L0/L1 boilerplate
  # (chứa "nhà xe") tràn top trên item hotline của chính nhà xe. Chế độ semantic (production)
  # phải tìm ra đúng item.
  hl=$(psql_q "INSERT INTO items (layer, operator_id, kind, topic, text, status)
    VALUES (2, 'smoke$sfx', 'policy', 'contact', 'Hotline hỗ trợ khách 24/7: 1900 6067', 'active')
    RETURNING id" | head -1)
  psql_q "INSERT INTO operations (kind, operator_id) VALUES ('index.items', 'smoke$sfx')" >/dev/null
  wait_for "index.items nhúng hotline $hl" 120 bash -c \
    "$C exec -T postgres psql -U biva -d biva -Atc \"SELECT embedding IS NOT NULL FROM items WHERE id='$hl'\" | grep -qx t"
  out=$(mcp '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall_knowledge","arguments":{"query":"Hotline của nhà xe số mấy"}}}')
  HL=$hl python3 -c '
import json, os, sys
raw = sys.stdin.read()
msg = json.loads(next((l[5:] for l in raw.splitlines() if l.startswith("data:")), raw))
res = msg["result"]["structuredContent"]
keys = [h.get("key", "") for h in res["items"][:5]]
assert any(h["id"] == os.environ["HL"] for h in res["items"][:5]), {"top5": keys, "degraded": res.get("degraded")}
print("✓ xếp hạng: query hiếm token (hotline) — item của nhà xe trong top-5 (semantic)")
' <<<"$out" || { echo "✗ xếp hạng hotline: $out"; exit 1; }
fi
echo "smoke OK"
