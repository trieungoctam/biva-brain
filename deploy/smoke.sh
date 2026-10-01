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

# Dữ liệu thử: nhà xe, builder, token.
sfx=$(date +%s)
$C exec -T brain-api brain-api operator add "smoke$sfx" "Smoke $sfx"
$C exec -T brain-api brain-api user add "smoke$sfx" --email "smoke$sfx@biva.local" --name Smoke --role builder
$C exec -T brain-api brain-api user grant "smoke$sfx" "smoke$sfx"
token=$($C exec -T brain-api brain-api token issue "smoke$sfx" --name smoke --ttl 1h | tail -1)

# Job ping → ai-worker phải xử lý xong.
job=$($C exec -T postgres psql -U biva -d biva -Atc \
  "INSERT INTO operations (kind, operator_id) VALUES ('system.ping', 'smoke$sfx') RETURNING id" | head -1)
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
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/mcp/operator/khac$sfx/" -H "Authorization: Bearer $token" \
  -H "Content-Type: application/json" -d '{}')
[[ "$code" == 403 ]] && echo "✓ sai phạm vi → 403" || { echo "✗ sai phạm vi → $code"; exit 1; }

if [[ "${SMOKE_SKIP_TEI:-}" != 1 ]]; then
  wait_for "TEI embed (bge-m3, CPU)" 900 curl -fsS 127.0.0.1:8081/embed \
    -H 'Content-Type: application/json' -d '{"inputs":"xe giường nằm Sài Gòn Đà Lạt"}'
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
fi
echo "smoke OK"
