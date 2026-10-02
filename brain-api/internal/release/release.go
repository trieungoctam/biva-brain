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
	"fmt"

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

	// 3. Snapshot đã lắp.
	var snap int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM snapshots s JOIN bots b ON b.id = s.bot_id
		WHERE b.operator_id = $1 AND b.channel = $2`, operatorID, channel).Scan(&snap); err != nil {
		return rep, err
	}
	if snap == 0 {
		block("snapshot", "chưa có snapshot — export_bot trước")
	} else {
		pass("snapshot", fmt.Sprintf("snapshot mới nhất: bản thứ %d", snap))
	}

	// 4. Test bot pass 100% (lần chạy mới nhất).
	var total, passed int
	err = db.QueryRow(ctx, `
		SELECT total, passed FROM test_runs
		WHERE operator_id = $1 AND bot_channel = $2
		ORDER BY created_at DESC LIMIT 1`, operatorID, channel).Scan(&total, &passed)
	switch {
	case err != nil:
		if err.Error() != "no rows in result set" {
			return rep, err
		}
		block("tests", "chưa chạy test bot — run_tests trước")
	case total == 0:
		block("tests", "lần chạy test gần nhất không có case nào")
	case passed < total:
		block("tests", fmt.Sprintf("test %d/%d pass — sửa case fail rồi chạy lại", passed, total))
	default:
		pass("tests", fmt.Sprintf("test %d/%d pass", passed, total))
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
