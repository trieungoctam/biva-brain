package textnorm

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSharedFixtures: cùng file với test Python — hai bên phải khớp từng byte.
func TestSharedFixtures(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "contracts", "textnorm", "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c struct{ Input, Fold, SearchText string }
		var raw map[string]string
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			t.Fatalf("dòng %d: %v", n+1, err)
		}
		c.Input, c.Fold, c.SearchText = raw["input"], raw["fold"], raw["search_text"]
		n++
		if got := Fold(c.Input); got != c.Fold {
			t.Errorf("Fold(%q) = %q, muốn %q", c.Input, got, c.Fold)
		}
		if got := SearchText(c.Input); got != c.SearchText {
			t.Errorf("SearchText(%q) = %q, muốn %q", c.Input, got, c.SearchText)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 200 {
		t.Fatalf("chỉ có %d câu fixture, cần ≥ 200", n)
	}
}
