package textnorm

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Cùng fixture với Python (tests/test_ingest.py): key do ingest (Python) và propose_item (Go) phải trùng nhau.
func TestNormalizeKeyShared(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "contracts", "textnorm", "keys.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var c struct{ Topic, Key, Expected string }
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		n++
		if got := NormalizeKey(c.Topic, c.Key); got != c.Expected {
			t.Errorf("NormalizeKey(%q, %q) = %q, muốn %q", c.Topic, c.Key, got, c.Expected)
		}
	}
	if n == 0 {
		t.Fatal("fixture rỗng")
	}
}

// Cùng fixture với Python (tests/test_index.py): item do Go (kb sync, propose_item) và Python (ingest, index.items)
// ghi phải có cùng search_text.
func TestItemSearchTextShared(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "contracts", "textnorm", "item_search.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var c struct{ Topic, Key, Text, Expected string }
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		n++
		if got := ItemSearchText(c.Topic, c.Key, c.Text); got != c.Expected {
			t.Errorf("ItemSearchText(%q, %q, %q) = %q, muốn %q", c.Topic, c.Key, c.Text, got, c.Expected)
		}
	}
	if n == 0 {
		t.Fatal("fixture rỗng")
	}
}
