package kb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trieungoctam/biva-brain/brain-api/internal/testdb"
)

const (
	realKB  = "../../../kb"
	schemas = "../../../contracts/schemas"
)

func TestLoadRealKB(t *testing.T) {
	b, err := Load(realKB, schemas)
	if err != nil {
		t.Fatal(err)
	}
	tpl, ok := b.Templates["xe-khach"]
	if !ok || len(tpl.Sections) == 0 {
		t.Fatal("thiếu template xe-khach")
	}
	required := map[string]bool{}
	for _, s := range tpl.Sections {
		if s.Level == "required" {
			required[s.Topic] = true
		}
	}
	// Mục bắt buộc theo build-flow: tuyến, giá, giờ, điểm đón, huỷ vé.
	for _, topic := range []string{"route", "fare", "schedule", "pickup", "cancellation"} {
		if !required[topic] {
			t.Errorf("template thiếu mục bắt buộc %s", topic)
		}
	}
	locked := 0
	for _, r := range b.Rules {
		if r.Layer == 0 && r.Locked {
			locked++
		}
	}
	if locked == 0 {
		t.Error("L0 phải có rule locked")
	}
}

// writeKB tạo kb/ tạm: L0 + L1 xe-khach (template thật) với rules tuỳ biến.
func writeKB(t *testing.T, l0, l1 string) string {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "L0"), 0o755))
	must(os.MkdirAll(filepath.Join(dir, "L1", "xe-khach"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "L0", "rules.yaml"), []byte(l0), 0o644))
	must(os.WriteFile(filepath.Join(dir, "L1", "xe-khach", "rules.yaml"), []byte(l1), 0o644))
	tpl, err := os.ReadFile(filepath.Join(realKB, "L1", "xe-khach", "template.yaml"))
	must(err)
	must(os.WriteFile(filepath.Join(dir, "L1", "xe-khach", "template.yaml"), tpl, 0o644))
	return dir
}

const l0OK = `layer: 0
rules:
  - {key: l0.test.a, kind: policy, topic: grounding, locked: true, text: "Không bịa thông tin cho khách."}
  - {key: l0.test.b, kind: persona, topic: tone, locked: false, text: "Trả lời lịch sự và ngắn gọn."}
`
const l1OK = `layer: 1
industry: xe-khach
rules:
  - {key: l1.test.luggage, kind: policy, topic: luggage, locked: false, text: "Thường được mang 20kg hành lý."}
`

func TestLoadRejects(t *testing.T) {
	cases := map[string][2]string{
		"trùng key":            {l0OK + `  - {key: l0.test.a, kind: policy, topic: x, locked: true, text: "Bản trùng key ở đây."}` + "\n", l1OK},
		"sai prefix tầng":      {strings.Replace(l0OK, "l0.test.b", "l1.test.b", 1), l1OK},
		"topic ngoài template": {l0OK, strings.Replace(l1OK, "topic: luggage", "topic: karaoke", 1)},
		"thiếu industry L1":    {l0OK, strings.Replace(l1OK, "industry: xe-khach\n", "", 1)},
		"kind lạ":              {strings.Replace(l0OK, "kind: persona", "kind: logic", 1), l1OK},
		"text quá ngắn":        {strings.Replace(l0OK, "Trả lời lịch sự và ngắn gọn.", "ngắn", 1), l1OK},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeKB(t, c[0], c[1]), schemas); err == nil {
				t.Fatal("lẽ ra phải bị từ chối")
			}
		})
	}
	if _, err := Load(writeKB(t, l0OK, l1OK), schemas); err != nil {
		t.Fatalf("bộ hợp lệ bị từ chối: %v", err)
	}
}

func TestSyncLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t)
	cleanup := func() {
		pool.Exec(ctx, `DELETE FROM items WHERE key LIKE 'l_.test.%'`)
		pool.Exec(ctx, `DELETE FROM audit_log WHERE target LIKE 'kb:l_.test.%'`)
		pool.Exec(ctx, `DELETE FROM operations WHERE kind = 'index.items' AND operator_id IS NULL`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// Chỉ đụng tới rule test: các item kb/ khác (nếu có) cũng bị retire nếu không có trong bundle,
	// nên test dùng DB riêng và xoá mọi item kb/ thật trước khi chạy.
	pool.Exec(ctx, `DELETE FROM items WHERE metadata->>'source' LIKE 'kb/%'`)

	sync := func(l0, l1 string, dry bool) Report {
		t.Helper()
		b, err := Load(writeKB(t, l0, l1), schemas)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Sync(ctx, pool, b, "test", dry)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	countActive := func() int {
		var n int
		pool.QueryRow(ctx, `SELECT count(*) FROM items WHERE key LIKE 'l_.test.%' AND status = 'active'`).Scan(&n)
		return n
	}

	// dry-run: báo thay đổi nhưng không ghi.
	if rep := sync(l0OK, l1OK, true); rep.Count("add") != 3 || countActive() != 0 {
		t.Fatalf("dry-run: %+v, active=%d", rep, countActive())
	}

	rep := sync(l0OK, l1OK, false)
	if rep.Count("add") != 3 || rep.IndexJob == "" || countActive() != 3 {
		t.Fatalf("lần đầu: %+v active=%d", rep, countActive())
	}
	var label string
	pool.QueryRow(ctx, `SELECT metadata->>'label' FROM items WHERE key = 'l1.test.luggage'`).Scan(&label)
	if label != "thông lệ chung" {
		t.Errorf("L1 không locked phải có nhãn thông lệ chung, có %q", label)
	}

	// Chạy lại không đổi gì → không ghi, không enqueue.
	if rep := sync(l0OK, l1OK, false); len(rep.Changes) != 0 || rep.Unchanged != 3 || rep.IndexJob != "" {
		t.Fatalf("idempotent: %+v", rep)
	}

	// Sửa text một rule → update: bản cũ superseded trỏ sang bản mới.
	l1v2 := strings.Replace(l1OK, "20kg", "25kg", 1)
	if rep := sync(l0OK, l1v2, false); rep.Count("update") != 1 || rep.Unchanged != 2 {
		t.Fatalf("update: %+v", rep)
	}
	var oldStatus, newText string
	err := pool.QueryRow(ctx, `
		SELECT o.status, n.text FROM items o JOIN items n ON n.id = o.superseded_by
		WHERE o.key = 'l1.test.luggage' AND o.text LIKE '%20kg%'`).Scan(&oldStatus, &newText)
	if err != nil || oldStatus != "superseded" || !strings.Contains(newText, "25kg") {
		t.Fatalf("supersede: status=%s new=%q err=%v", oldStatus, newText, err)
	}

	// Bỏ rule khỏi kb/ → retracted.
	l0v2 := strings.SplitAfter(l0OK, "\n")
	if rep := sync(strings.Join(l0v2[:3], ""), l1v2, false); rep.Count("retire") != 1 || countActive() != 2 {
		t.Fatalf("retire: %+v active=%d", rep, countActive())
	}

	var audits int
	pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target LIKE 'kb:l_.test.%' AND actor = 'test'`).Scan(&audits)
	if audits != 5 { // 3 add + 1 update + 1 retire
		t.Errorf("audit = %d dòng, muốn 5", audits)
	}
}
