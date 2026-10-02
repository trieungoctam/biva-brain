// Package release kiểm cổng phát hành (M4, S4.1.1): mọi điều kiện phải đạt trước khi request_publish.
//
// Điều kiện (docs/architecture.md §6 "Release gate"):
//   - coverage mục bắt buộc 100% (mục khuyến nghị chỉ báo cáo);
//   - mọi artifact bắt buộc của kênh tồn tại và `valid`, không artifact nào `stale`;
//   - snapshot đã lắp (export_bot từng chạy) — bản mới nhất không lỗi;
//   - test mới nhất của bot pass 100% (test_runs);
//   - không đề xuất review/conflict đang mở.
//
// Gate trả về danh sách lý do cụ thể khi chặn (blocked) — AI/builder biết phải sửa gì.
package release

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Report struct {
	Passed  bool     `json:"passed"`
	Blocked []string `json:"blocked,omitempty"` // lý do cụ thể khi chặn
	Checks  []Check  `json:"checks"`
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Gate kiểm cho bot (operator + channel) theo template (required artifacts, required topics).
func Gate(ctx context.Context, db *pgxpool.Pool, operatorID, channel string, requiredArtifacts, requiredTopics []string) (Report, error) {
	rep := Report{Passed: true, Blocked: []string{}, Checks: []Check{}}
	block := func(name, detail string) {
		rep.Blocked = append(rep.Blocked, detail)
		rep.Checks = append(rep.Checks, Check{Name: name, Passed: false, Detail: detail})
	}
	pass := func(name, detail string) {
		rep.Checks = append(rep.Checks, Check{Name: name, Passed: true, Detail: detail})
	}

	// 1. Artifact: đủ bắt buộc, valid, không stale — bản mới nhất mỗi kind.
	rows, err := db.Query(ctx, `
		SELECT DISTINCT ON (kind) kind, status FROM bot_artifacts a
		JOIN bots b ON b.id = a.bot_id AND b.operator_id = $1 AND b.channel = $2
		ORDER BY kind, version DESC`, operatorID, channel)
	if err != nil {
		return rep, err
	}
	latest := map[string]string{}
	for rows.Next() {
		var kind, status string
		if err := rows.Scan(&kind, &status); err != nil {
			rows.Close()
			return rep, err
		}
		latest[kind] = status
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for _, kind := range requiredArtifacts {
		st, ok := latest[kind]
		switch {
		case !ok:
			block("artifact", kind+": chưa có (bắt buộc) — viết rồi save_artifact")
		case st == "valid":
			// đủ
		case st == "stale":
			block("artifact", kind+": stale — prompt refresh_bot rồi sửa")
		default:
			block("artifact", fmt.Sprintf("%s: %s — sửa theo validate_artifact", kind, st))
		}
	}
	for kind, st := range latest {
		if st == "stale" {
			block("artifact", kind+": stale — prompt refresh_bot rồi sửa")
		}
	}
	if len(latest) > 0 {
		pass("artifact", fmt.Sprintf("%d artifact, bản mới nhất đều valid và không stale", len(latest)))
	} else {
		block("artifact", "chưa có artifact nào — build_bot trước")
	}

	// 2. Coverage mục bắt buộc 100%.
	missing, err := countTopics(ctx, db, operatorID, requiredTopics)
	if err != nil {
		return rep, err
	}
	if missing > 0 {
		block("coverage", fmt.Sprintf("%d mục bắt buộc chưa phủ (missing/ambiguous) — get_coverage để xem", missing))
	} else {
		pass("coverage", "mọi mục bắt buộc đã phủ")
	}

	// 3. Snapshot đã lắp (đồng thời lấy id bản mới nhất để gắn test).
	var snapVer int
	var snapID string
	if err := db.QueryRow(ctx, `
		SELECT s.id::text, s.version FROM snapshots s JOIN bots b ON b.id = s.bot_id
		WHERE b.operator_id = $1 AND b.channel = $2 ORDER BY s.version DESC LIMIT 1`,
		operatorID, channel).Scan(&snapID, &snapVer); err != nil {
		if err.Error() == "no rows in result set" {
			block("snapshot", "chưa có snapshot — export_bot trước")
		} else {
			return rep, err
		}
	} else {
		pass("snapshot", fmt.Sprintf("snapshot mới nhất: v%d", snapVer))
	}

	// 4. Test bot pass 100% — lần chạy mới nhất PHẢI gắn đúng snapshot sẽ phát hành
	// (test của snapshot cũ không chốt cửa cho snapshot mới).
	if snapID == "" {
		block("tests", "chưa có snapshot nên chưa test được")
	} else {
		var total, passed int
		err = db.QueryRow(ctx, `
			SELECT total, passed FROM test_runs
			WHERE operator_id = $1 AND bot_channel = $2 AND snapshot_id = $3::uuid
			ORDER BY created_at DESC LIMIT 1`, operatorID, channel, snapID).Scan(&total, &passed)
		switch {
		case err != nil:
			if err.Error() != "no rows in result set" {
				return rep, err
			}
			block("tests", "chưa chạy test bot cho snapshot mới nhất — run_tests trước")
		case total == 0:
			block("tests", "lần chạy test gần nhất không có case nào")
		case passed < total:
			block("tests", fmt.Sprintf("test %d/%d pass — sửa case fail rồi chạy lại", passed, total))
		default:
			pass("tests", fmt.Sprintf("test %d/%d pass", passed, total))
		}
	}

	// 5. Không đề xuất đang mở (conflict/đề xuất chờ duyệt).
	var open int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM review_items WHERE operator_id = $1 AND status = 'open'`, operatorID).
		Scan(&open); err != nil {
		return rep, err
	}
	if open > 0 {
		block("review", fmt.Sprintf("%d đề xuất đang chờ duyệt — list_review_queue", open))
	} else {
		pass("review", "không có đề xuất đang mở")
	}

	rep.Passed = len(rep.Blocked) == 0
	if rep.Passed {
		rep.Blocked = nil
	}
	return rep, nil
}

func countTopics(ctx context.Context, db *pgxpool.Pool, operatorID string, requiredTopics []string) (int, error) {
	// Mục bắt buộc = có item active của nhà xe (L2) hoặc item pending/conflict đang mở (ambiguous).
	rows, err := db.Query(ctx, `
		SELECT t.topic, count(i.id) FILTER (WHERE i.id IS NOT NULL) AS covered
		FROM unnest($2::text[]) AS t(topic)
		LEFT JOIN items i ON i.topic = t.topic AND i.operator_id = $1
		     AND i.status = 'active' AND i.kind <> 'observation'
		GROUP BY t.topic`, operatorID, requiredTopics)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	missing := 0
	for rows.Next() {
		var topic string
		var covered int
		if err := rows.Scan(&topic, &covered); err != nil {
			return 0, err
		}
		if covered == 0 {
			missing++
		}
	}
	return missing, rows.Err()
}

// ───────────────────────── S4.1.2: request_publish / rollback ─────────────────────────

var ErrGateBlocked = errors.New("release gate chưa đạt")

// Publish yêu cầu phát hành snapshot mới nhất. stage=staging: gate đạt → published ngay.
// stage=production: tạo bản requested — chờ lead duyệt (Approve) rồi mới published.
func Publish(ctx context.Context, db *pgxpool.Pool, operatorID, channel, stage, actor string,
	requiredArtifacts, requiredTopics []string) (string, string, error) {
	gate, err := Gate(ctx, db, operatorID, channel, requiredArtifacts, requiredTopics)
	if err != nil {
		return "", "", err
	}
	if !gate.Passed {
		return "", "", fmt.Errorf("%w: %s", ErrGateBlocked, strings.Join(gate.Blocked, "; "))
	}
	// Một transaction + advisory lock theo (operator, kênh): chọn snapshot, publish, rollback
	// bản cũ không thể interleaved giữa hai lời gọi (A/B cùng kênh từng ra cả hai rolled_back).
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`,
		operatorID, channel); err != nil {
		return "", "", err
	}
	// Chống TOCTOU với export_bot (snapshot vN+1 có thể commit giữa Gate và đây): dưới
	// advisory lock, kiểm tra lại test 100% PHẢI gắn đúng snapshot sắp phát hành — snapshot
	// mới hơn chưa test thì từ chối, không phát hành bản chưa kiểm.
	var gatedSnap, gatedVer string
	var gatedTotal, gatedPassed int
	if err := tx.QueryRow(ctx, `
		SELECT s.id::text, s.version::text, t.total, t.passed FROM snapshots s
		JOIN bots b ON b.id = s.bot_id AND b.operator_id = $1 AND b.channel = $2
		LEFT JOIN LATERAL (
			SELECT total, passed FROM test_runs r
			WHERE r.operator_id = $1 AND r.bot_channel = $2 AND r.snapshot_id = s.id
			ORDER BY r.created_at DESC LIMIT 1) t ON true
		ORDER BY s.version DESC LIMIT 1`, operatorID, channel).
		Scan(&gatedSnap, &gatedVer, &gatedTotal, &gatedPassed); err != nil {
		if err.Error() == "no rows in result set" {
			return "", "", fmt.Errorf("chưa có snapshot cho %s/%s — export_bot trước", operatorID, channel)
		}
		return "", "", err
	}
	if gatedTotal == 0 || gatedPassed < gatedTotal {
		return "", "", fmt.Errorf("%w: snapshot v%s chưa có test pass 100%% (snapshot mới phải run_tests lại)",
			ErrGateBlocked, gatedVer)
	}
	var id, status string
	err = tx.QueryRow(ctx, `
		INSERT INTO releases (operator_id, bot_channel, snapshot_id, snapshot_ver, stage,
			status, requested_by, published_at)
		SELECT $1, $2, $3::uuid, $4::int, $5,
		       CASE WHEN $5 = 'staging' THEN 'published' ELSE 'requested' END, $6,
		       CASE WHEN $5 = 'staging' THEN now() END
		RETURNING id::text, status`,
		operatorID, channel, gatedSnap, mustAtoi(gatedVer), stage, actor).Scan(&id, &status)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "", "", fmt.Errorf("chưa có snapshot cho %s/%s — export_bot trước", operatorID, channel)
		}
		return "", "", err
	}
	// Staging published → mọi bản staging cũ của kênh này thành rolled_back (chỉ 1 bản chạy).
	if stage == "staging" {
		if _, err := tx.Exec(ctx, `UPDATE releases SET status = 'rolled_back', rolled_back_at = now()
			WHERE operator_id = $1 AND bot_channel = $2 AND stage = 'staging'
			  AND status = 'published' AND id <> $3::uuid`, operatorID, channel, id); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return id, status, nil
}

// Approve: lead duyệt bản production requested → published. Rollback mọi bản production cũ.
// Cùng advisory lock + transaction với Publish/Rollback: không thể interleaved mất bản đang chạy.
func Approve(ctx context.Context, db *pgxpool.Pool, releaseID, approver string) (string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	// Thứ tự khoá thống nhất toàn package: advisory TRƯỚC row (Publish/Rollback đã theo
	// thứ tự này — Approve từng update row trước advisory, dễ sinh cycle khi có biến thể sau).
	var op, ch string
	if err := tx.QueryRow(ctx, `
		SELECT operator_id, bot_channel FROM releases WHERE id = $1::uuid
		  AND status = 'requested' AND stage = 'production'`, releaseID).Scan(&op, &ch); err != nil {
		if err.Error() == "no rows in result set" {
			return "", fmt.Errorf("bản phát hành %s không ở trạng thái requested (production)", releaseID)
		}
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`, op, ch); err != nil {
		return "", err
	}
	var status string
	err = tx.QueryRow(ctx, `
		UPDATE releases SET status = 'published', approved_by = $2, published_at = now()
		WHERE id = $1::uuid AND status = 'requested' AND stage = 'production'
		RETURNING status`, releaseID, approver).Scan(&status)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET status = 'rolled_back', rolled_back_at = now()
		WHERE operator_id = $1 AND bot_channel = $2 AND stage = 'production'
		  AND status = 'published' AND id <> $3::uuid`, op, ch, releaseID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return status, nil
}

// Rollback về bản production trước đó (< 1 phút): bản đang published → rolled_back,
// bản published gần nhất trước đó (đã rolled_back) → published lại.
func Rollback(ctx context.Context, db *pgxpool.Pool, operatorID, channel, actor string) (string, int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)
	// Cùng lock với Publish/Approve: rollback không đua với publish mới của cùng kênh.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1 || ':' || $2))`,
		operatorID, channel); err != nil {
		return "", 0, err
	}
	var cur, prev string
	var curVer int
	err = tx.QueryRow(ctx, `
		SELECT id::text, snapshot_ver FROM releases
		WHERE operator_id = $1 AND bot_channel = $2 AND stage = 'production' AND status = 'published'
		ORDER BY published_at DESC LIMIT 1`, operatorID, channel).Scan(&cur, &curVer)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return "", 0, fmt.Errorf("không có bản production nào đang phát hành")
		}
		return "", 0, err
	}
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM releases
		WHERE operator_id = $1 AND bot_channel = $2 AND stage = 'production'
		  AND status = 'rolled_back' AND id <> $3::uuid
		ORDER BY published_at DESC LIMIT 1`, operatorID, channel, cur).Scan(&prev)
	switch {
	case err == nil:
		if _, err := tx.Exec(ctx, `UPDATE releases SET status = 'published', rolled_back_at = NULL,
			published_at = now() WHERE id = $1::uuid`, prev); err != nil {
			return "", 0, err
		}
	case err.Error() == "no rows in result set":
		prev = "" // chưa có bản trước đó
	default:
		return "", 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE releases SET status = 'rolled_back', rolled_back_at = now()
		WHERE id = $1::uuid`, cur); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, err
	}
	var _ = actor
	return prev, curVer, nil
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
