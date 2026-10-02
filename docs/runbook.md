# Runbook sự cố — BIVA Brain

Mỗi mục: **dấu hiệu → xử lý → xác nhận đã khắc phục**. Danh mục sự cố theo
[system-architecture.md §7.1](system-architecture.md#71-khi-một-thành-phần-hỏng).

Lệnh chung (chạy từ gốc repo; stack `deploy/docker-compose.yml`):

```bash
make ps                                   # trạng thái service
make logs                                 # theo dõi log (Ctrl-C thoát)
docker compose -f deploy/docker-compose.yml exec postgres psql -U biva -d biva
curl -s localhost:8080/health/ready       # health (đổi port theo máy)
```

---

## 1. ai-worker chết / chậm

**Dấu hiệu**
- `operations` dồn: `SELECT kind, status, count(*) FROM operations WHERE status IN ('queued','running')
  GROUP BY 1,2;` — queued tăng, running = 0.
- Log worker lặp "lần N lỗi" hoặc không có log mới.
- `get_operation` trả `running` quá lâu / `failed` với `attempts = max_attempts`.

**Xử lý**
1. `docker compose ... ps ai-worker` — container có đang chạy không; `restart` nếu chết.
2. Job hết lease tự requeue (scheduler, 15s/lần) — không cần can thiệp thủ công.
3. Job `failed` (hết 5 lần): đọc `error` trong `operations`, sửa nguyên nhân rồi đưa về hàng đợi:
   ```sql
   UPDATE operations SET status='queued', attempts=0, lease_until=NULL WHERE id='<uuid>';
   ```
4. Chậm do backlog: tăng concurrency (`--concurrency`, mặc định 4) hoặc thêm instance worker —
   claim dùng SKIP LOCKED nên chạy song song an toàn.

**Xác nhận**: queued giảm về ~0; `get_operation` của job mới trả `done` trong vài giây.

---

## 2. TEI (embedding/rerank) chết

**Dấu hiệu**
- Log brain-api: `recall: nhánh semantic lỗi` / `rerank: ...` trong `degraded`.
- `recall_knowledge` vẫn trả kết quả nhưng `arms` chỉ còn `keyword`/`graph`/`temporal`.

**Xử lý**
1. Không khẩn cấp: recall **tự degrade** về keyword + graph + temporal (thiết kế §7.2).
2. Khôi phục TEI: `docker compose ... up -d tei-embed` (profile `rerank` cho tei-rerank).
   Lần đầu tải model bge-m3 ~2GB — mất vài phút.
3. TEI OOM (exit 137): tăng `mem_limit` (máy ≥16GB) hoặc tắt hẳn — mọi nhánh vẫn hoạt động.
4. Item mới chưa có embedding: chạy lại `kb sync` hoặc reindex sau khi TEI lên.

**Xác nhận**: `recall_knowledge` trả hit có `arms` chứa `semantic`; log không còn WARNING degraded.

---

## 3. LLM provider (Gemini) lỗi / hết quota

**Dấu hiệu**: job `ingest`/`logic.spec`/`validate`/`bot.*` fail với lỗi provider
(429/5xx/quota) trong `llm_usage` (`SELECT purpose, ok, error_code, count(*) ... GROUP BY 1,2,3`).

**Xử lý**
1. Thư viện `llm/` tự fallback model dự phòng cùng provider + backoff — chờ vài phút trước khi can thiệp.
2. Sự cố cả provider: thêm provider khác trong `contracts/llm/llm.yaml` (một adapter mới) —
   hiện chỉ có gemini.
3. Hết quota thật: tạm tắt luồng nền (không enqueue `ingest`) — MCP đọc/validate tĩnh/export vẫn chạy.
4. Key mới: đổi `GEMINI_API_KEY` trong env của ai-worker rồi restart.

**Xác nhận**: `llm_usage` có dòng `ok=true` gần nhất; job mới `done`.

---

## 4. Postgres replica lỗi / trễ

**Dấu hiệu**: log "đọc chuyển về primary" hoặc trễ replication tăng; `get_operation`/recall chậm hơn SLO.

**Xử lý**
1. Cấu hình `BIVA_DATABASE_REPLICA_URL` trống → mọi đọc về primary (store tự xử lý).
2. Replica hỏng hoàn toàn: bỏ biến, restart brain-api.
3. Khôi phục replica theo thủ tục hạ tầng (managed: re-create standby).

**Xác nhận**: health/ready ok; p95 recall về < 150ms (Xem §7.3 SLO).

---

## 5. Postgres primary lỗi

**Dấu hướng**: health/ready `{"primary":"err"}`; mọi tool ghi lỗi 5xx.

**Xử lý**
1. Failover (managed) hoặc restart container: `docker compose ... restart postgres`.
2. Sau khi primary lên: brain-api tự kết nối lại (pool). Job đang `running` hết lease → requeue.
3. Kiểm tra `schema_migrations` — nếu `dirty=true` sau sự cố giữa migrate:
   ```bash
   brain-api migrate version   # xem version + dirty
   # sửa tay theo golang-migrate: force version rồi up lại
   ```

**Xác nhận**: health/ready `ok`; ghi thử `system.ping` qua MCP trả `done`.

---

## 6. Redis lỗi

**Dấu hiệu**: log quota LLM lỗi kết nối; cache pack miss nhiều (p95 pack tăng).

**Xử lý**: tự fallback — cache bỏ qua (đọc DB), quota giới hạn cứng theo process.
Khôi phục `docker compose ... up -d redis`; không mất dữ liệu nghiệp vụ.

**Xác nhận**: p95 `get_knowledge_pack` về < 800ms; log sạch.

---

## 7. Git (biva-integrations) không truy cập được

**Dấu hiệu**: job `index.code` fail "git clone/fetch lỗi"; log "merge PR không được index".

**Xử lý**: tri thức logic tiếp tục dùng index commit gần nhất (bảng `logic_syncs`) — không ảnh hưởng
build bot. Khôi phục mạng/token (`BIVA_INTEGRATIONS_TOKEN`), job tick sau tự bắt kịp (no-op theo HEAD).

**Xác nhận**: job `index.code` gần nhất `done`; `logic_syncs.commit` = HEAD repo.

---

## 8. Bản phát hành có lỗi (bot trả lời sai)

**Dấu hiệu**: nhà xe/khách báo sai; UAT fail.

**Xử lý**: MCP `rollback_release` (confirm_token) — bản trước phát hành lại trong 1 transaction (< 1 phút).
Xem `releases`: bản lỗi `rolled_back`, bản trước `published`.

**Xác nhận**: `SELECT stage, status, snapshot_ver FROM releases WHERE stage='production'
ORDER BY published_at DESC LIMIT 3;` — bản mong muốn đang `published`.

---

## Chaos: postgres chết giữa vận hành (đã kiểm chứng 02/10)

`docker stop postgres` 15s rồi start lại:
- health → 503, MCP → 500 từng request; **brain-api/ai-worker không restart** (pool tự nối lại).
- scheduler: mất lock (SQLSTATE 57P01) → tự **tái tuyển leader ~20s** sau khi DB về; goroutine
  phiên cũ thoát qua context con (không tích luỹ — fix vòng 3).
- worker: claim lỗi ghi WARNING, vòng slot sống tiếp; job enqueue trong outage được xử lý
  sau khi DB về (lease requeue không cần vì queue cũng nằm trong postgres).
- Verify khi tái diễn: `docker logs brain-api | grep -c "trở thành leader"` tăng đúng 1 sau
  mỗi lần mất-lại; job ping mới → status=done.

## Chaos: worker chết giữa job (đã kiểm chứng 02/10)

Giả lập worker chết giữa job (không cần SIGKILL thật — kết quả tương đương):
```sql
UPDATE operations SET status='running', lease_until = now() - interval '2 seconds',
  locked_by='deadworker' WHERE id='<job-id>'::uuid;
```
Scheduler chạy `requeue_expired` mỗi 15s → job về queued → worker khác claim và xử lý
(attempts tăng 1). Đã kiểm chứng: attempts 1→2, status done, result "pong".
Lưu ý: side effect của job phải idempotent — ingest đã chạy một transaction tất-cả-hoặc-không-gì
nên job bị claim lại sau khi chết giữa chừng xử lý lại từ đầu, không nhân đôi.

## Backup/restore — tái kiểm chứng sau migration 24–25 (02/10)

`BIVA_RESTORE_VERIFY=1 deploy/backup.sh` → 39/39 bảng, 45/45 items, 119/119 operations
khớp tuyệt đối giữa bản và DB scratch.

## Redis dùng làm gì & hành vi khi chết (kiểm chứng 03/10)

Redis chỉ giữ **quota LLM** (`RedisQuota` trong ai-worker). Khi Redis chết: quota
**fail-open** (cho phép gọi LLM, ghi WARNING) — việc bảo vệ ngân sách tạm mất nhưng
không job nào chết. Queue nằm trong Postgres nên không bị ảnh hưởng.

## Chaos: S3 chết giữa export (kiểm chứng 03/10)

`docker stop s3` → export_bot trả lỗi thân thiện ("Brain tạm thời không trả lời được"),
không treo, brain-api sống; `docker start s3` → export chạy lại được ngay (URL tải có
nonce không đoán được). Outage matrix đầy đủ: postgres ✓ · redis ✓ (fail-open) · s3 ✓.

## Phát hiện dò token trong log

Token sai → 401 + dòng WARN `mcp: token không hợp lệ` kèm `ip` và `path` (không ghi
audit_log để quét token không khuếch đại tải DB). Rà định kỳ:
`docker logs brain-api 2>&1 | grep "token không hợp lệ" | awk '{print $6}' | sort | uniq -c | sort -rn | head`
— một IP dày đặc là đang dò; chặn ở firewall/WAF.

## Trước khi lên production (security)

- **Bucket export**: key export đã có nonce ngẫu nhiên (URL không đoán được kể cả bucket công khai);
  production nên thêm bucket riêng tư + link presigned hết hạn ngắn (storage/s3.go tách public URL riêng).
- **OAuth register**: đặt `BIVA_OAUTH_REGISTRATION_SECRET` để bắt buộc initial access token khi đăng ký
  client (RFC 7591 §5); rate limit /oauth/register · /oauth/token · /oauth/authorize theo IP khi lên internet.
- **BIVA_INTEGRATIONS_REPO** chế độ path chỉ dùng dev; production đặt URL git + token
  (propose luôn tạo PR, index_code không đọc working tree local).
- Resource OAuth đã được whitelist chặt (issuer · /mcp/platform/ · /mcp/operator/<id>/ một segment);
  thêm scope mới thì phải kiểm scope khi Verify token.

## Backup / khôi phục dữ liệu

`deploy/backup.sh` — dump Postgres (custom format, nén) + dump schema-only để đối chiếu;
tệp lưu `deploy/backups/` (mặc định) hoặc `BIVA_BACKUP_DIR`.

```bash
deploy/backup.sh                        # backup ngay
BIVA_BACKUP_DIR=/mnt/backup deploy/backup.sh
BIVA_RESTORE_VERIFY=1 deploy/backup.sh  # backup + dựng DB scratch + khôi phục thử + so khớp số bảng/hàng
```

Khôi phục thật:

```bash
createdb biva_restore
pg_restore -d biva_restore backups/biva-YYYYmmdd-HHMM.dump
# kiểm tra rồi đưa vào sử dụng (đổi BIVA_DATABASE_URL) hoặc pg_dump/restore đè lên biva
```

Lịch đề nghị (1 VM): cron daily 2h sáng + giữ 14 bản:
```
0 2 * * * cd /srv/biva-brain && BIVA_BACKUP_DIR=/srv/backup ./deploy/backup.sh
```
PITR (point-in-time) khi chuyển lên managed Postgres — bật theo nhà cung cấp.
