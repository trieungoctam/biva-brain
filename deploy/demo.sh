#!/usr/bin/env bash
# Demo M5 "promote toàn cục" (exit M5): 3 nhà xe cùng một thông lệ → Brain tự gom observation,
# đề xuất promote lên L1, lead duyệt → thông lệ thành tri thức ngành cho MỌI nhà xe.
#
# Cần: stack đang chạy (make up hoặc compose local) + GEMINI key KHÔNG bắt buộc
# (consolidate/promote không dùng LLM). Chạy: deploy/demo.sh
set -euo pipefail

API="${BIVA_API:-http://localhost:8080}"
OPS_TOKEN="${BIVA_DEMO_OPS_TOKEN:?đặt BIVA_DEMO_OPS_TOKEN (token role ops)}"
LEAD_TOKEN="${BIVA_DEMO_LEAD_TOKEN:?đặt BIVA_DEMO_LEAD_TOKEN (token role lead)}"
OPS=(demoa demob democ)
TEXT="Không nhận chó mèo trên xe"

mcp() {  # mcp <operator|platform> <token> <tool> <json-args>
  local op="$1" tok="$2" tool="$3" args="${4:-{\}}"
  local path="operator/$op"; [[ "$op" == platform ]] && path=platform
  curl -s -m 60 -X POST "$API/mcp/$path/" -H "Authorization: Bearer $tok" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}" \
    | grep '^data: ' | sed 's/^data: //' | python3 -c 'import json,sys;d=json.load(sys.stdin)["result"];
c=d.get("structuredContent") or {};
err=d.get("isError");
print(json.dumps({"isError":err,**c},ensure_ascii=False))'
}

echo "== 1) Ba nhà xe gửi cùng một thông lệ (submit_knowledge — topic pets, NEW rủi ro thấp → tự apply)"
for op in "${OPS[@]}"; do
  out=$(mcp "$op" "$OPS_TOKEN" submit_knowledge "{\"source\":\"other\",\"received_at\":\"2026-10-02T09:00:00+07:00\",\"items\":[{\"kind\":\"policy\",\"topic\":\"pets\",\"key\":\"pets.cho_meo\",\"text\":\"$TEXT\"}]}")
  echo "   $op → $(echo "$out" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("summary") or d)')"
done

echo "== 2) apply_review (SQL) tự enqueue job consolidate — kể cả auto-apply của worker (migration 000023)"
sleep 12
for op in "${OPS[@]}"; do
  n=$(docker compose -f "$(dirname "$0")/docker-compose.yml" exec -T postgres psql -U biva -d biva -qtAc \
    "SELECT count(*) FROM items WHERE operator_id='$op' AND kind='observation' AND status='active'")
  echo "   $op: observation = $n"
done

echo "== 3) Job promote gom cụm ≥ 3 nhà xe → đề xuất PROMOTE"
docker compose -f "$(dirname "$0")/docker-compose.yml" exec -T postgres psql -U biva -d biva -c \
  "INSERT INTO operations (kind, idempotency_key) VALUES ('promote', 'demo:' || floor(extract(epoch from now()))::text)" >/dev/null
sleep 8
cand=$(mcp platform "$LEAD_TOKEN" list_promotion_candidates '{}')
echo "$cand" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for c in d.get("candidates",[]):
    if c.get("kind")=="knowledge":
        print("   ứng viên:", c["review_id"], "—", len(c["operators"]), "nhà xe:", ", ".join(c["operators"]))'

review_id=$(echo "$cand" | python3 -c '
import json,sys
d=json.load(sys.stdin)
print(next((c["review_id"] for c in d.get("candidates",[]) if c.get("kind")=="knowledge"), ""))')
[[ -n "$review_id" ]] || { echo "Không có ứng viên promote — kiểm tra consolidate ở bước 2"; exit 1; }

echo "== 4) Lead duyệt qua apply_review (preview → confirm_token → apply)"
prev=$(mcp demoa "$OPS_TOKEN" apply_review "{\"review_id\":\"$review_id\",\"decision\":\"approve\"}")
token=$(echo "$prev" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("confirm_token",""))')
[[ -n "$token" ]] || { echo "Không nhận được confirm_token: $prev"; exit 1; }
echo "   preview ok — token nhận được"
out=$(mcp demoa "$OPS_TOKEN" apply_review "{\"review_id\":\"$review_id\",\"decision\":\"approve\",\"confirm_token\":\"$token\"}")
echo "   apply → $(echo "$out" | python3 -c 'import json,sys;d=json.load(sys.stdin);o=d.get("outcome") or {};print(o.get("status"), d.get("message",""))')"

echo "== 5) Thông lệ lên L1 — mọi nhà xe đều thấy (không còn là tri thức riêng)"
docker compose -f "$(dirname "$0")/docker-compose.yml" exec -T postgres psql -U biva -d biva -c \
  "SELECT layer, status, left(text,45) AS text FROM items WHERE key LIKE 'l1.promoted.pets%' ORDER BY created_at DESC LIMIT 1"

echo "== 6) run_regression_all (platform) — enqueue test mọi nhà xe có snapshot"
mcp platform "$LEAD_TOKEN" run_regression_all '{}' | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("   đã enqueue:", d.get("count", 0), "job —", ", ".join(o["operator"] for o in d.get("operations",[])) or "(chưa nhà xe nào có snapshot)")'

echo "XONG — chuỗi: submit (auto-apply) → consolidate → promote → lead duyệt → L1 toàn cục."
