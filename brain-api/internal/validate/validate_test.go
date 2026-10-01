package validate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/artifact"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestPatterns(t *testing.T) {
	money := []string{"Giá 320.000đ", "giá 320,000 VND", "chỉ 320k thôi", "1,2 triệu", "300 nghìn/vé", "250000 đồng"}
	notMoney := []string{"ở 2 đêm", "hành lý 20kg", "2 trẻ em", "trước 24 giờ", "5 km", "hotline 0909 123 456"}
	clock := []string{"xuất bến 22:00", "chuyến 22h", "22h30 tối", "lúc 7 giờ tối"}
	notClock := []string{"trước 24 giờ", "có mặt trước 30 phút", "đi khoảng 7 tiếng"}
	for _, s := range money {
		if !moneyRe.MatchString(s) {
			t.Errorf("phải bắt tiền: %q", s)
		}
	}
	for _, s := range notMoney {
		if moneyRe.MatchString(s) {
			t.Errorf("không phải tiền: %q → %q", s, moneyRe.FindString(s))
		}
	}
	for _, s := range clock {
		if !clockRe.MatchString(s) {
			t.Errorf("phải bắt giờ: %q", s)
		}
	}
	for _, s := range notClock {
		if clockRe.MatchString(s) {
			t.Errorf("không phải giờ chạy: %q → %q", s, clockRe.FindString(s))
		}
	}
}

func TestBlocks(t *testing.T) {
	bs := blocks("# Tiêu đề\nĐoạn một dòng một\ndòng hai [[x]]\n\n- mục 1\n  tiếp mục 1\n- mục 2\n```\ncode 22:00\n```\nsau code")
	if len(bs) != 4 || bs[0].start != 2 || bs[0].end != 3 || bs[0].heading != "Tiêu đề" || bs[1].end != 6 ||
		bs[2].start != 7 || bs[3].start != 11 {
		t.Fatalf("blocks = %+v", bs)
	}
}

func TestValidateCodes(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op := "va" + sfx
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'V')`, op); err != nil {
		t.Fatal(err)
	}
	var global []string
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM items WHERE id = ANY($1::uuid[])`, global)
		pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op)
	})
	item := func(layer int, topic, status string, locked bool) string {
		var opArg any
		if layer == 2 {
			opArg = op
		}
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, key, text, status, locked)
			VALUES ($1, $2, 'policy', $3, $4, 'nội dung', $5, $6) RETURNING id::text`,
			layer, opArg, topic, fmt.Sprintf("l%d.%s.%s", layer, topic, sfx), status, locked).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if layer <= 1 {
			global = append(global, id)
		}
		return id
	}
	pets := item(2, "pets", "active", false)
	oldPets := item(2, "pets", "superseded", false)
	pool.Exec(ctx, `UPDATE items SET superseded_by = $1 WHERE id = $2`, pets, oldPets)
	kidDefault := item(1, "children", "active", false)
	topics := []TopicSpec{{ID: "pets", Title: "Thú cưng", Required: true}, {ID: "fare" + sfx, Title: "Giá vé", Required: true}}

	run := func(kind, content string) Report {
		t.Helper()
		res, err := artifact.Save(ctx, pool, artifact.SaveInput{OperatorID: op, Kind: kind, Content: content, Author: "ai:t"})
		if err != nil {
			t.Fatal(err)
		}
		a, err := artifact.Get(ctx, pool, op, "", kind, res.Version)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Validate(ctx, pool, op, a, topics)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	codes := func(is []Issue) string {
		var out []string
		for _, i := range is {
			out = append(out, fmt.Sprintf("%s@%d", i.Code, i.Line))
		}
		return strings.Join(out, ",")
	}

	// FAQ sạch: câu hỏi không cần trích dẫn; thông lệ có nhãn; giá hướng gọi tool.
	clean := run("faq", "### Có chở chó mèo không?\nNhà xe không nhận chó mèo lên xe, mong anh/chị thông cảm. [["+pets+"]]\n\n"+
		"### Trẻ em có mất vé không?\nThông thường trẻ nhỏ ngồi chung ghế được miễn vé, anh/chị vui lòng xác nhận lại với nhà xe. [["+kidDefault+"]]\n\n"+
		"### Giá vé bao nhiêu?\nDạ để em kiểm tra giá theo ngày đi.")
	if !clean.Valid || clean.Status != "valid" || len(clean.Errors) != 0 {
		t.Fatalf("faq sạch: %+v", clean.Errors)
	}

	bad := run("faq", "### Giá?\nGiá vé giường nằm là 320.000đ, xuất bến lúc 22:00 mỗi ngày từ Sài Gòn. [["+pets+"]]\n"+
		"\n- Nhà xe nhận gửi hàng cồng kềnh với phí theo khối lượng\n"+
		"- Trẻ dưới 6 tuổi được miễn vé khi ngồi cùng bố mẹ. [["+kidDefault+"]]\n"+
		"- Không nhận chó mèo. [["+oldPets+"]]")
	got := codes(bad.Errors)
	for _, want := range []string{"HARDCODED_DATA@2", "UNCITED@4", "UNLABELED_DEFAULT@5", "STALE_CITATION@6"} {
		if !strings.Contains(got, want) {
			t.Errorf("thiếu %s trong %s", want, got)
		}
	}
	if bad.Valid || bad.Status != "invalid" {
		t.Fatal("faq lỗi phải invalid")
	}
	for _, e := range bad.Errors {
		if e.Code == "STALE_CITATION" && e.SupersededBy != pets {
			t.Fatalf("STALE_CITATION phải chỉ bản thay thế: %+v", e)
		}
	}
	var stored string
	pool.QueryRow(ctx, `SELECT status FROM bot_artifacts WHERE id = $1`, bad.ArtifactID).Scan(&stored)
	if stored != "invalid" {
		t.Fatalf("status lưu = %s", stored)
	}

	// Nhà xe vừa có chính sách riêng cho topic của thông lệ đã trích → STALE_CITATION chỉ sang item mới.
	ownKid := item(2, "children", "active", false)
	again := run("faq", "### Trẻ em có mất vé không?\nThông thường trẻ nhỏ ngồi chung ghế được miễn vé, anh/chị vui lòng xác nhận lại với nhà xe. [["+kidDefault+"]]")
	if len(again.Errors) != 1 || again.Errors[0].Code != "STALE_CITATION" || again.Errors[0].SupersededBy != ownKid {
		t.Fatalf("thông lệ bị nhà xe thay: %+v", again.Errors)
	}

	// system_prompt: thiếu rule locked (mọi rule locked đang active trong DB test) và COVERAGE.
	locked := item(0, "other", "active", true)
	sp := run("system_prompt", "Bạn là trợ lý của nhà xe.\nKhông nhận chó mèo lên xe trong mọi trường hợp. [["+pets+"]]")
	missing := false
	for _, e := range sp.Errors {
		if e.Code == "MISSING_LOCKED" && e.Item == locked {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("thiếu MISSING_LOCKED: %s", codes(sp.Errors))
	}
	// pets đã được cite → không COVERAGE; fare chưa có tri thức nhà xe → cảnh báo (cần fallbacks).
	if strings.Contains(codes(sp.Errors), "COVERAGE") || !strings.Contains(codes(sp.Warnings), "COVERAGE") {
		t.Fatalf("coverage: errors=%s warnings=%s", codes(sp.Errors), codes(sp.Warnings))
	}
	// Nhà xe có tri thức fare nhưng system_prompt không dùng → COVERAGE lỗi.
	item(2, "fare"+sfx, "active", false)
	sp2 := run("system_prompt", "Bạn là trợ lý của nhà xe, luôn lịch sự.\nKhông nhận chó mèo lên xe trong mọi trường hợp nhé. [["+pets+"]]")
	if !strings.Contains(codes(sp2.Errors), "COVERAGE") {
		t.Fatalf("thiếu COVERAGE: %s", codes(sp2.Errors))
	}
}

func TestNoCapability(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	op := "nc" + sfx
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'N')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })

	run := func(content string) Report {
		t.Helper()
		res, err := artifact.Save(ctx, pool, artifact.SaveInput{OperatorID: op, Kind: "tool_spec",
			Content: content, Author: "ai:t"})
		if err != nil {
			t.Fatal(err)
		}
		a, err := artifact.Get(ctx, pool, op, "", "tool_spec", res.Version)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Validate(ctx, pool, op, a, nil)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}

	spec := "- `get_fare` (capability: fare): tra giá theo ngày đi.\n- `check_seats`: xem ghế trống."
	rep := run(spec)
	if rep.Valid || len(rep.Errors) != 1 || rep.Errors[0].Code != "NO_CAPABILITY" {
		t.Fatalf("chưa có hồ sơ: %+v", rep)
	}
	if !strings.Contains(rep.Errors[0].Message, "plan_logic_implementation") {
		t.Fatalf("message phải hướng dẫn triển khai: %+v", rep.Errors[0])
	}
	// Có hồ sơ active → pass; tool không khai báo capability không bị kiểm.
	if _, err := pool.Exec(ctx, `INSERT INTO logic_profiles (operator_id, capability, mode, module_id,
		module_version, commit) VALUES ($1, 'fare', 'config', 'fare.standard', 1, 'c')`, op); err != nil {
		t.Fatal(err)
	}
	rep = run(spec)
	if !rep.Valid {
		t.Fatalf("đã có hồ sơ: %+v", rep)
	}
}
