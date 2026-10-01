package artifact

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trieungoctam/biva-brain/brain-api/internal/scheduler"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func status(t *testing.T, pool *pgxpool.Pool, op, kind string, version int) (string, string) {
	t.Helper()
	var st, reasons string
	if err := pool.QueryRow(context.Background(), `SELECT status, stale_reasons::text FROM bot_artifacts
		WHERE bot_id = $1 AND kind = $2 AND version = $3`, BotID(op, "zalo"), kind, version).Scan(&st, &reasons); err != nil {
		t.Fatal(err)
	}
	return st, reasons
}

func TestMarkStale(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op := "st" + sfx
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'S')`, op); err != nil {
		t.Fatal(err)
	}
	var global []string
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, global)
		pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op)
	})
	item := func(layer int, topic, text string, locked bool, validTo *time.Time) string {
		var opArg any
		if layer == 2 {
			opArg = op
		}
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, status, locked, valid_to)
			VALUES ($1, $2, 'policy', $3, $4, $5, 'active', $6, $7) RETURNING id::text`, layer, opArg, topic,
			fmt.Sprintf("k%d.%s.%d", layer, topic, time.Now().UnixNano()), text, locked, validTo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if layer <= 1 {
			global = append(global, id)
		}
		return id
	}
	save := func(kind, content string) int {
		t.Helper()
		r, err := Save(ctx, pool, SaveInput{OperatorID: op, Kind: kind, Content: content, Author: "ai:t"})
		if err != nil {
			t.Fatal(err)
		}
		return r.Version
	}
	past := time.Now().Add(-time.Hour)
	pets := item(2, "pets", "Không nhận chó mèo", false, nil)
	kid := item(1, "children", "Thông thường trẻ nhỏ miễn vé", false, nil)
	tet := item(2, "fare", "Giá Tết áp dụng tới hết mùng 10", false, &past)

	v1 := save("faq", "### Chó mèo?\nKhông nhận chó mèo. [["+pets+"]]\n### Trẻ em?\nThông thường miễn vé. [["+kid+"]]")
	save("flows", "Giá dịp Tết theo bảng riêng. [["+tet+"]]")
	if st, _ := status(t, pool, op, "faq", v1); st != "draft" {
		t.Fatalf("ban đầu = %s", st)
	}

	// CHANGE: bản cũ superseded rồi mới ghi superseded_by (như apply_review) → stale kèm bản thay thế.
	newPets := item(2, "pets", "Nhận mèo nhỏ trong lồng", false, nil)
	tx, _ := pool.Begin(ctx)
	tx.Exec(ctx, `UPDATE items SET status = 'superseded' WHERE id = $1`, pets)
	tx.Exec(ctx, `UPDATE items SET superseded_by = $1 WHERE id = $2`, newPets, pets)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	st, reasons := status(t, pool, op, "faq", v1)
	if st != "stale" || !strings.Contains(reasons, `"superseded_by": "`+newPets) || !strings.Contains(reasons, `"line": 2`) {
		t.Fatalf("supersede: %s %s", st, reasons)
	}
	// newPets là L2 pets active nhưng faq không trích thông lệ pets → không thêm lý do overridden.
	// Nhà xe có chính sách trẻ em riêng → thông lệ children đang trích bị thay.
	ownKid := item(2, "children", "Trẻ dưới 5 tuổi miễn vé", false, nil)
	if _, reasons = status(t, pool, op, "faq", v1); !strings.Contains(reasons, `"reason": "overridden_default"`) ||
		!strings.Contains(reasons, ownKid) {
		t.Fatalf("override: %s", reasons)
	}

	// Expire (scheduler) → flows stale.
	if err := scheduler.ExpireItems(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if st, reasons := status(t, pool, op, "flows", 1); st != "stale" || !strings.Contains(reasons, `"reason": "expired"`) {
		t.Fatalf("expire: %s %s", st, reasons)
	}

	// Quy tắc bắt buộc mới → system_prompt chưa trích nó bị stale.
	sp := save("system_prompt", "Bạn là trợ lý. [["+newPets+"]]")
	locked := item(0, "other", "Luôn lịch sự", true, nil)
	if st, reasons := status(t, pool, op, "system_prompt", sp); st != "stale" || !strings.Contains(reasons, locked) {
		t.Fatalf("locked mới: %s %s", st, reasons)
	}

	// Chỉ bản mới nhất bị đánh dấu: faq v2 không trích ownKid's default nữa → retract newPets chỉ đụng v2 nếu v2 trích.
	v2 := save("faq", "### Chó mèo?\nNhận mèo nhỏ trong lồng. [["+newPets+"]]")
	if st, _ := status(t, pool, op, "faq", v2); st != "draft" {
		t.Fatalf("version mới = %s", st)
	}
	pool.Exec(ctx, `UPDATE items SET status = 'retracted' WHERE id = $1`, newPets)
	if st, r := status(t, pool, op, "faq", v2); st != "stale" || !strings.Contains(r, `"retracted"`) {
		t.Fatalf("retract v2: %s %s", st, r)
	}
	if _, r := status(t, pool, op, "faq", v1); strings.Contains(r, "retracted") {
		t.Fatalf("bản cũ v1 không được đụng: %s", r)
	}

	list, err := ListStale(ctx, pool, op, "")
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]StaleArtifact{}
	for _, a := range list {
		byKind[a.Kind] = a
	}
	if len(list) != 3 || byKind["faq"].Version != v2 {
		t.Fatalf("list_stale = %+v", list)
	}
	r := byKind["faq"].Reasons[0]
	if r.Line != 2 || r.LineText != "Nhận mèo nhỏ trong lồng. [["+newPets+"]]" || r.Instruction == "" || r.ItemText != "Nhận mèo nhỏ trong lồng" {
		t.Fatalf("reason = %+v", r)
	}
}
