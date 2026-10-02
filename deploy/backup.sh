#!/usr/bin/env bash
# Backup Postgres của BIVA Brain + (tuỳ chọn BIVA_RESTORE_VERIFY=1) khôi phục thử vào DB scratch
# để chứng minh bản backup dùng được (S5.2.1: "khôi phục thử từ backup thành công").
#
#   deploy/backup.sh                        # backup vào deploy/backups/
#   BIVA_BACKUP_DIR=/srv/backup deploy/backup.sh
#   BIVA_RESTORE_VERIFY=1 deploy/backup.sh   # backup + dựng DB scratch + restore + so khớp
#
# Host thiếu pg_dump/psql (vd macOS): script tự chạy client BÊN TRONG container postgres của stack.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
DB_URL="${BIVA_DATABASE_URL:-postgres://biva:biva@localhost:5432/biva?sslmode=disable}"
DB_NAME="$(basename "${DB_URL%%\?*}")"          # biva
VERIFY_DB="biva_verify_$(date +%s)"
BACKUP_DIR="${BIVA_BACKUP_DIR:-$HERE/backups}"
STAMP="$(date +%Y%m%d-%H%M)"
KEEP="${BIVA_BACKUP_KEEP:-14}"
IN_CONTAINER=""

if command -v pg_dump >/dev/null 2>&1; then
  pg() { "$@"; }
  FILE="$BACKUP_DIR/biva-$STAMP.dump"
else
  echo "== host thiếu pg client — dùng container postgres của stack"
  pg() { docker compose -f "$HERE/docker-compose.yml" exec -T postgres "$@"; }
  IN_CONTAINER="/tmp/biva-$STAMP.dump"          # dump trong container rồi chép ra host
  FILE="$IN_CONTAINER"
fi

mkdir -p "$BACKUP_DIR" 2>/dev/null || true
echo "== dump $DB_NAME"
pg pg_dump -U biva -d "$DB_NAME" --format=custom --no-owner --no-privileges -f "$FILE"
if [[ -n "$IN_CONTAINER" ]]; then
  docker compose -f "$HERE/docker-compose.yml" cp postgres:"$IN_CONTAINER" "$BACKUP_DIR/biva-$STAMP.dump"
fi
HOST_FILE="$BACKUP_DIR/biva-$STAMP.dump"
ls -lh "$HOST_FILE"

ls -1t "$BACKUP_DIR"/biva-*.dump | tail -n +$((KEEP + 1)) | xargs -r rm -f
echo "== giữ $KEEP bản gần nhất trong $BACKUP_DIR"

if [[ "${BIVA_RESTORE_VERIFY:-0}" != "1" ]]; then
  [[ -n "$IN_CONTAINER" ]] && docker compose -f "$HERE/docker-compose.yml" exec -T postgres rm -f "$IN_CONTAINER"
  exit 0
fi

# ── Khôi phục thử: DB scratch + restore + so khớp số bảng / hàng ──
echo "== verify: restore vào $VERIFY_DB"
pg createdb -U biva "$VERIFY_DB"
cleanup() {
  pg dropdb -U biva --if-exists "$VERIFY_DB" || true
  if [[ -n "$IN_CONTAINER" ]]; then
    docker compose -f "$HERE/docker-compose.yml" exec -T postgres rm -f "$IN_CONTAINER" || true
  fi
}
trap cleanup EXIT
pg pg_restore -U biva --no-owner --no-privileges -d "$VERIFY_DB" "$FILE"

cmp_count() {  # cmp_count <sql> <nhãn>
  local q="$1" label="$2" a b
  a=$(pg psql -U biva -d "$DB_NAME" -qtAc "$q")
  b=$(pg psql -U biva -d "$VERIFY_DB" -qtAc "$q")
  echo "== $label: $b/$a"
  [[ "$a" == "$b" ]]
}
ok=1
cmp_count "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" "bảng" || ok=0
cmp_count "SELECT count(*) FROM items" "items" || ok=0
cmp_count "SELECT count(*) FROM operations" "operations" || ok=0
[[ "$ok" == 1 ]] || { echo "LỖI: khôi phục thử không khớp" >&2; exit 1; }
echo "KHÔI PHỤC THỬ OK — bản backup dùng được"
