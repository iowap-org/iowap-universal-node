package subst

import (
	"strings"
	"testing"
)

func TestExpandStringAndTaskID(t *testing.T) {
	vars, err := Vars("t-1", []byte(`{"file":"report.docx","pages":3}`))
	if err != nil {
		t.Fatalf("Vars: %v", err)
	}
	got, err := Expand("http://127.0.0.1:9000/api/{{task_id}}/render/{{file}}?pages={{pages}}", vars)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := "http://127.0.0.1:9000/api/t-1/render/report.docx?pages=3"
	if got != want {
		t.Errorf("Expand = %q, want %q", got, want)
	}
}

// Object/array params are substituted as compact JSON.
func TestExpandObjectParam(t *testing.T) {
	vars, err := Vars("t", []byte(`{"options":{"a":1,"b":[2,3]}}`))
	if err != nil {
		t.Fatalf("Vars: %v", err)
	}
	got, err := Expand(`{"options": {{options}}}`, vars)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != `{"options": {"a":1,"b":[2,3]}}` {
		t.Errorf("Expand = %q", got)
	}
}

// Placeholder names tolerate whitespace around the variable name.
func TestExpandWhitespace(t *testing.T) {
	vars, _ := Vars("t", []byte(`{"x":"v"}`))
	got, err := Expand("a {{ x }} b", vars)
	if err != nil || got != "a v b" {
		t.Errorf("Expand = %q, err %v", got, err)
	}
}

// Unknown (and null) variables must fail loudly, never substitute empty.
func TestExpandUnknownVariableFails(t *testing.T) {
	vars, _ := Vars("t", []byte(`{"x":null}`))
	if _, err := Expand("{{y}}", vars); err == nil {
		t.Error("Expand of unknown variable = nil error, want error")
	}
	if _, err := Expand("{{x}}", vars); err == nil {
		t.Error("Expand of null variable = nil error, want error")
	}
	if _, err := Vars("t", []byte(`[1,2]`)); err == nil {
		t.Error("Vars with non-object params = nil error, want error")
	}
}

// Text without placeholders passes through unchanged; unclosed "{{" is
// left as literal text.
func TestExpandPassthrough(t *testing.T) {
	got, err := Expand("no placeholders at all", map[string]any{})
	if err != nil || got != "no placeholders at all" {
		t.Errorf("Expand = %q, err %v", got, err)
	}
	got, err = Expand("unclosed {{ here", map[string]any{})
	if err != nil || got != "unclosed {{ here" {
		t.Errorf("Expand = %q, err %v", got, err)
	}
}

// Vars exposes the task ID even with empty params.
func TestVarsTaskIDOnly(t *testing.T) {
	vars, err := Vars("t9", nil)
	if err != nil {
		t.Fatalf("Vars: %v", err)
	}
	got, err := Expand("{{task_id}}", vars)
	if err != nil || got != "t9" {
		t.Errorf("task_id substitution = %q, err %v", got, err)
	}
	if !strings.Contains(got, "t9") {
		t.Error("unreachable")
	}
}
