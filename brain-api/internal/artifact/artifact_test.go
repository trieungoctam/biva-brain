package artifact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestParseCitations(t *testing.T) {
	cites, errs := ParseCitations("Dòng 1 [[0f8fad5b-d9cb-469f-a165-70867728950e]]\nkhông có\n" +
		"hai [[0F8FAD5B-D9CB-469F-A165-70867728950E]] [[ 7c9e6679-7425-40de-944b-e07fc1f90ae7 ]] và [[it_311]]")
	if len(cites) != 3 || cites[0].Line != 1 || cites[1].Line != 3 || cites[1].ItemID != "0f8fad5b-d9cb-469f-a165-70867728950e" ||
		cites[2].ItemID != "7c9e6679-7425-40de-944b-e07fc1f90ae7" {
		t.Fatalf("cites = %+v", cites)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "dòng 3: [[it_311]]") {
		t.Fatalf("errs = %v", errs)
	}
}

func TestSaveGetList(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op, other := "ar"+sfx, "arb"+sfx
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'A'), ($2, 'B')`, op, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM audit_log WHERE actor = 'ai:test'`)
		pool.Exec(ctx, `DELETE FROM operators WHERE id IN ($1, $2)`, op, other)
	})
	item := func(operator, status string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
			VALUES (2, $1, 'policy', 'pets', 'Không nhận chó mèo', $2) RETURNING id::text`, operator, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pets, old, foreign := item(op, "active"), item(op, "superseded"), item(other, "active")
	pool.Exec(ctx, `UPDATE items SET superseded_by = $1 WHERE id = $2`, pets, old)

	save := func(content string, base int) (SaveResult, error) {
		return Save(ctx, pool, SaveInput{OperatorID: op, Kind: "faq", Content: content, BaseVersion: base,
			Author: "ai:test", KnowledgeVersion: "1.2", Note: "thử"})
	}
	// Trích dẫn item của nhà xe khác / không tồn tại → lỗi theo dòng, không lưu.
	_, err := save("Hỏi: chó?\nKhông nhận. [["+foreign+"]]", 0)
	var ie *InputError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "dòng 2") {
		t.Fatalf("trích dẫn nhà xe khác: %v", err)
	}
	if _, err := Save(ctx, pool, SaveInput{OperatorID: op, Kind: "menu", Content: "x", Author: "ai:test"}); !errors.As(err, &ie) {
		t.Fatalf("kind sai: %v", err)
	}

	v1, err := save("### Có chở chó mèo không?\nNhà xe không nhận chó mèo. [["+pets+"]]\nCũ: [["+old+"]]", 0)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Version != 1 || v1.Status != "draft" || v1.Citations != 2 || v1.BotID != op+":zalo" ||
		len(v1.Warnings) != 1 || v1.Warnings[0].Line != 3 || !strings.Contains(v1.Warnings[0].Note, pets) {
		t.Fatalf("v1 = %+v", v1)
	}
	// Nội dung y hệt → không tạo version.
	if again, _ := save("### Có chở chó mèo không?\nNhà xe không nhận chó mèo. [["+pets+"]]\nCũ: [["+old+"]]", 0); !again.Unchanged || again.Version != 1 {
		t.Fatalf("lưu lại y hệt: %+v", again)
	}
	v2, err := save("### Có chở chó mèo không?\nNhà xe không nhận chó mèo. [["+pets+"]]", 1)
	if err != nil || v2.Version != 2 || len(v2.Warnings) != 0 {
		t.Fatalf("v2 = %+v %v", v2, err)
	}
	// Sửa từ bản cũ (v1) trong khi đã có v2 → xung đột.
	if _, err := save("khác", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("base_version cũ: %v", err)
	}

	a, err := Get(ctx, pool, op, "", "faq", 0)
	if err != nil || a.Version != 2 || len(a.Citations) != 1 || a.Citations[0].Line != 2 || len(a.Versions) != 2 ||
		a.KnowledgeVersion != "1.2" || a.Author != "ai:test" {
		t.Fatalf("get = %+v %v", a, err)
	}
	if a, _ := Get(ctx, pool, op, "zalo", "faq", 1); a.Version != 1 || len(a.Citations) != 2 {
		t.Fatalf("get v1 = %+v", a)
	}
	if _, err := Get(ctx, pool, other, "zalo", "faq", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nhà xe khác: %v", err)
	}
	list, err := List(ctx, pool, op, "")
	if err != nil || len(list) != 1 || list[0].Version != 2 || list[0].Citations != 1 || list[0].Channel != "zalo" {
		t.Fatalf("list = %+v %v", list, err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'artifact.save' AND payload->>'operator_id' = $1`, op).Scan(&n)
	if n != 2 {
		t.Fatalf("audit = %d", n)
	}
}

// base_version trên kind CHƯA TỪNG có artifact → báo "chưa có" rõ ràng (từng trả
// "đã có version mới hơn (v0, bạn sửa từ v1)" — lạc hướng cho builder).
func TestSaveBaseVersionOnMissingKindClearMessage(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := context.Background()
	op := "bv" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'B')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')`, op+":zalo", op); err != nil {
		t.Fatal(err)
	}
	_, err := Save(ctx, pool, SaveInput{OperatorID: op, Kind: "faq", Content: "Nội dung mẫu đủ dài.",
		BaseVersion: 1, Author: "t"})
	if err == nil || !strings.Contains(err.Error(), "chưa có artifact") {
		t.Fatalf("muốn thông báo 'chưa có artifact', được: %v", err)
	}
}
