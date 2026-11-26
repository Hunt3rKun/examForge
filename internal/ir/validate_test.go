package ir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenFixturesValidate(t *testing.T) {
	root := filepath.Join("..", "..")
	if err := ValidateFile(filepath.Join(root, "schemas", "exam-ir-v1.json"), filepath.Join(root, "testdata", "golden", "ir", "sample.json")); err != nil {
		t.Fatalf("golden Exam IR: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, "testdata", "golden", "layout", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no golden layout fixtures")
	}
	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := ValidateFile(filepath.Join(root, "schemas", "layout-spec-v1.json"), path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidExamIRRejected(t *testing.T) {
	schema := filepath.Join("..", "..", "schemas", "exam-ir-v1.json")
	base, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "ir", "sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, old, replacement string }{
		{"rect out of range", `"rect": [0.1, 0.2, 0.4, 0.5]`, `"rect": [0.1, 0.2, 1.2, 0.5]`},
		{"colspan zero", `"colspan": 2`, `"colspan": 0`},
		{"cell missing text", `{"text": "$f(1)$"}`, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modified := []byte(string(base))
			if !contains(string(modified), tt.old) {
				t.Fatalf("fixture does not contain target %q", tt.old)
			}
			modified = []byte(replaceFirst(string(modified), tt.old, tt.replacement))
			if err := ValidateSchema(schema, modified); err == nil {
				t.Fatal("expected schema validation failure")
			}
		})
	}
}

func TestTableHeadersNormalizeBothShapes(t *testing.T) {
	for _, raw := range []string{
		`[{"text":"A"},{"text":"B"}]`,
		`[[{"text":"A"},{"text":"B"}]]`,
	} {
		var table Table
		if err := json.Unmarshal([]byte(`{"headers":`+raw+`,"rows":[]}`), &table); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if len(table.Headers) != 1 || len(table.Headers[0]) != 2 || table.Headers[0][1].Text != "B" {
			t.Errorf("%s normalized to %+v, want one row of two cells", raw, table.Headers)
		}
	}
}

func TestDecodeExamIRRejectsOversizedInput(t *testing.T) {
	v, err := NewValidator(
		filepath.Join("..", "..", "schemas", "exam-ir-v1.json"),
		filepath.Join("..", "..", "schemas", "layout-spec-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.DecodeExamIR(strings.NewReader(strings.Repeat(" ", MaxExamBytes+1)))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want size limit", err)
	}
}

func TestInvalidLayoutRejected(t *testing.T) {
	schema := filepath.Join("..", "..", "schemas", "layout-spec-v1.json")
	base, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "layout", "student.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{replaceFirst(string(base), `"columns":1`, `"columns":3`), replaceFirst(string(base), `"paper_size":"a4"`, `"paper_size":"letter"`)} {
		if err := ValidateSchema(schema, []byte(invalid)); err == nil {
			t.Fatalf("expected invalid layout to fail: %s", invalid)
		}
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || index(s, sub) >= 0 }
func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func replaceFirst(s, old, replacement string) string {
	i := index(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + replacement + s[i+len(old):]
}
