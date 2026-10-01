package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/trieungoctam/biva-brain/brain-api/internal/artifact"
	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

func TestStripAndFAQ(t *testing.T) {
	if got := StripCitations("Không nhận chó mèo. [[a]] [[b]]\nDòng 2[[c]]"); got != "Không nhận chó mèo.\nDòng 2" {
		t.Fatalf("strip = %q", got)
	}
	csv := FAQCSV("# FAQ\n\n## Hành lý\n### Có chở chó mèo không?\nKhông nhận chó mèo ạ.\n\n" +
		"**Hỏi:** Trẻ em có mất vé không?\n**Đáp:** Thông thường trẻ nhỏ miễn vé,\nvui lòng xác nhận lại.\n\n" +
		"Giá vé bao nhiêu?\n- Dạ em kiểm tra theo ngày đi.")
	want := "question,answer\nCó chở chó mèo không?,Không nhận chó mèo ạ.\n" +
		"Trẻ em có mất vé không?,\"Thông thường trẻ nhỏ miễn vé, vui lòng xác nhận lại.\"\n" +
		"Giá vé bao nhiêu?,Dạ em kiểm tra theo ngày đi.\n"
	if csv != want {
		t.Fatalf("faq csv =\n%s\nmuốn\n%s", csv, want)
	}
}

func TestExport(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	op := fmt.Sprintf("ex%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO operators (id, name) VALUES ($1, 'E')`, op); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM operators WHERE id = $1`, op) })
	var pets string
	pool.QueryRow(ctx, `INSERT INTO items (layer, operator_id, kind, topic, text, status)
		VALUES (2, $1, 'policy', 'pets', 'Không nhận chó mèo', 'active') RETURNING id::text`, op).Scan(&pets)
	in := Input{OperatorID: op, Required: []string{"system_prompt", "faq"}, Actor: "ai:t", KnowledgeVersion: "3.4"}

	var nr *NotReadyError
	if _, err := Export(ctx, pool, in); !errors.As(err, &nr) {
		t.Fatalf("chưa có bot: %v", err)
	}
	save := func(kind, content string) {
		if _, err := artifact.Save(ctx, pool, artifact.SaveInput{OperatorID: op, Kind: kind, Content: content, Author: "ai:t"}); err != nil {
			t.Fatal(err)
		}
	}
	save("faq", "### Có chở chó mèo không?\nNhà xe không nhận chó mèo. [["+pets+"]]")
	_, err := Export(ctx, pool, in)
	if !errors.As(err, &nr) || !strings.Contains(err.Error(), "system_prompt: chưa có") ||
		!strings.Contains(err.Error(), "faq v1: chưa kiểm") {
		t.Fatalf("thiếu + draft: %v", err)
	}
	save("system_prompt", "Bạn là trợ lý nhà xe. [["+pets+"]]")
	pool.Exec(ctx, `UPDATE bot_artifacts SET status = 'valid' WHERE operator_id = $1`, op)

	res, err := Export(ctx, pool, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.SnapshotVersion != 1 || res.Reused || res.Filename != op+"-zalo-v1.json" || res.Artifacts["faq"] != 1 {
		t.Fatalf("export = %+v", res)
	}
	var def Definition
	if err := json.Unmarshal([]byte(res.Content), &def); err != nil {
		t.Fatal(err)
	}
	if def.KnowledgeVersion != "3.4" || def.Artifacts["faq"].Content != "### Có chở chó mèo không?\nNhà xe không nhận chó mèo." ||
		len(def.Artifacts["faq"].Citations) != 1 {
		t.Fatalf("definition = %+v", def)
	}
	// Không đổi gì → dùng lại snapshot; format khác vẫn cùng snapshot.
	md, err := Export(ctx, pool, Input{OperatorID: op, Format: "markdown", Required: in.Required, Actor: "ai:t"})
	if err != nil || !md.Reused || md.SnapshotVersion != 1 || !strings.Contains(md.Content, "Bạn là trợ lý nhà xe.") ||
		strings.Contains(md.Content, "[[") {
		t.Fatalf("markdown = %+v %v", md, err)
	}
	csv, _ := Export(ctx, pool, Input{OperatorID: op, Format: "faq_csv", Required: in.Required, Actor: "ai:t"})
	if csv.Content != "question,answer\nCó chở chó mèo không?,Nhà xe không nhận chó mèo.\n" {
		t.Fatalf("csv = %q", csv.Content)
	}

	// Tri thức đổi → faq stale → không xuất; sửa + valid → snapshot v2.
	pool.Exec(ctx, `UPDATE items SET status = 'retracted' WHERE id = $1`, pets)
	if _, err := Export(ctx, pool, in); !errors.As(err, &nr) || !strings.Contains(err.Error(), "refresh_bot") {
		t.Fatalf("stale: %v", err)
	}
	save("faq", "### Có chở chó mèo không?\nDạ em chuyển nhân viên hỗ trợ ạ.")
	save("system_prompt", "Bạn là trợ lý nhà xe.")
	pool.Exec(ctx, `UPDATE bot_artifacts SET status = 'valid' WHERE operator_id = $1 AND version = 2`, op)
	if res, err := Export(ctx, pool, in); err != nil || res.SnapshotVersion != 2 || res.Reused {
		t.Fatalf("v2 = %+v %v", res, err)
	}
	if _, err := Export(ctx, pool, Input{OperatorID: op, Format: "pdf", Required: in.Required}); !errors.As(err, &nr) {
		t.Fatal("format sai phải lỗi")
	}
}
