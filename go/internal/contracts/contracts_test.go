// Package contracts kiểm tra JSON Schema trong contracts/schemas với fixture dùng chung (Go ⇄ Python).
package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const root = "../../../contracts"

// schemaFor ánh xạ thư mục fixture → file schema: "jobs.ingest" → schemas/jobs/ingest.schema.json.
func schemaFor(fixtureDir string) string {
	return filepath.Join(root, "schemas", strings.ReplaceAll(fixtureDir, ".", "/")+".schema.json")
}

func TestSchemasAgainstSharedFixtures(t *testing.T) {
	dirs, err := os.ReadDir(filepath.Join(root, "fixtures", "schemas"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		sch, err := c.Compile(schemaFor(d.Name()))
		if err != nil {
			t.Fatalf("%s: compile: %v", d.Name(), err)
		}
		files, _ := filepath.Glob(filepath.Join(root, "fixtures", "schemas", d.Name(), "*.json"))
		for _, f := range files {
			raw, err := os.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(raw)
			raw.Close()
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			verr := sch.Validate(doc)
			name := filepath.Base(f)
			switch {
			case strings.HasPrefix(name, "valid_") && verr != nil:
				t.Errorf("%s/%s phải hợp lệ: %v", d.Name(), name, verr)
			case strings.HasPrefix(name, "invalid_") && verr == nil:
				t.Errorf("%s/%s phải bị từ chối", d.Name(), name)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("không tìm thấy fixture nào")
	}
}

func TestFixturesAreValidJSON(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(root, "fixtures", "schemas", "*", "*.json"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if !json.Valid(b) {
			t.Errorf("%s không phải JSON hợp lệ", f)
		}
	}
}
