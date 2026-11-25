package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/comfyui"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/httpprovider"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// --- Loading semantics ----------------------------------------------------------

// A missing providers file is not an error: the daemon serves an empty
// manifest (board T-019: "wenn fehlt/leer → leerer Manifest, kein Crash").
func TestLoadMissingFile(t *testing.T) {
	provs, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if len(provs) != 0 {
		t.Errorf("providers = %d, want 0", len(provs))
	}
}

func TestLoadEmptyFile(t *testing.T) {
	for _, content := range []string{"", "\n\n", "# only a comment\n", "providers: []\n", "providers:\n"} {
		provs, err := Load(write(t, content))
		if err != nil {
			t.Fatalf("Load(%q): %v", content, err)
		}
		if len(provs) != 0 {
			t.Errorf("Load(%q): providers = %d, want 0", content, len(provs))
		}
	}
}

// Broken YAML and unknown provider types are loud config errors — the
// daemon refuses to start rather than silently dropping capabilities.
func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name, content, wantInErr string
	}{
		{"bad yaml", "providers:\n  - name: [unclosed\n", "yaml"},
		{"unknown type", "providers:\n  - name: x.cap\n    type: teleport\n", "teleport"},
		{"missing name", "providers:\n  - type: http\n    exec:\n      url_template: http://x/\n", "name"},
		{"invalid cap name", "providers:\n  - name: 1Bad/Name\n    type: http\n    exec:\n      url_template: http://x/\n", "1Bad/Name"},
		{"http without exec", "providers:\n  - name: x.cap\n    type: http\n", "exec"},
		{"http without url_template", "providers:\n  - name: x.cap\n    type: http\n    exec:\n      method: GET\n", "url_template"},
		{"comfyui without url", "providers:\n  - name: comfy.generate\n    type: comfyui\n", "url"},
		{"process without command", "providers:\n  - name: s.run\n    type: process\n", "command"},
		{"empty command", "providers:\n  - name: s.run\n    type: process\n    command: []\n", "command"},
		{"duplicate name", "providers:\n  - name: x.cap\n    type: process\n    command: [true]\n  - name: x.cap\n    type: http\n    exec:\n      url_template: http://x/\n", "x.cap"},
		{"unsafe workspace", "providers:\n  - name: x.cap\n    type: process\n    command: [true]\n    workspace: ../escape\n", "workspace"},
	}
	for _, c := range cases {
		_, err := Load(write(t, c.content))
		if err == nil {
			t.Errorf("%s: Load = nil error, want error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantInErr) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.wantInErr)
		}
	}
}

// --- Scheduling hints (board T-030) -----------------------------------------

// Entries without max_parallel/cost default both fields to 1 — across all
// three provider types, since the configured wrapper applies them centrally.
func TestLoadSchedulingDefaults(t *testing.T) {
	p := write(t, `
providers:
  - type: comfyui
    url: http://127.0.0.1:8188
  - name: api.cap
    type: http
    exec:
      url_template: http://x/{{task_id}}
  - name: script.cap
    type: process
    command: [sh, -c, "true"]
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(provs) != 3 {
		t.Fatalf("providers = %d, want 3", len(provs))
	}
	for i, prov := range provs {
		cap := prov.Capability()
		if cap.MaxParallel != 1 || cap.Cost != 1 {
			t.Errorf("provider %d: max_parallel/cost = %d/%d, want 1/1", i, cap.MaxParallel, cap.Cost)
		}
	}
}

// Explicit max_parallel/cost values land in the capability unchanged.
func TestLoadSchedulingValues(t *testing.T) {
	p := write(t, `
providers:
  - name: script.cap
    type: process
    command: [sh, -c, "true"]
    max_parallel: 4
    cost: 5
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cap := provs[0].Capability()
	if cap.MaxParallel != 4 || cap.Cost != 5 {
		t.Errorf("max_parallel/cost = %d/%d, want 4/5", cap.MaxParallel, cap.Cost)
	}
}

// Zero, negative and non-numeric values all degrade to the default of 1
// instead of failing daemon startup (the Node dashboard writes the fields;
// a stray value must not take the whole registry down).
func TestLoadSchedulingTolerant(t *testing.T) {
	cases := []struct {
		name, yaml string
	}{
		{"zero", "max_parallel: 0\ncost: 0\n"},
		{"negative", "max_parallel: -2\ncost: -7\n"},
		{"garbage strings", "max_parallel: \"abc\"\ncost: \"x\"\n"},
		{"garbage mapping", "max_parallel: {a: 1}\ncost: [1, 2]\n"},
	}
	for _, c := range cases {
		p := write(t, `
providers:
  - name: script.cap
    type: process
    command: [sh, -c, "true"]
`+c.yaml)
		provs, err := Load(p)
		if err != nil {
			t.Fatalf("%s: Load: %v", c.name, err)
		}
		cap := provs[0].Capability()
		if cap.MaxParallel != 1 || cap.Cost != 1 {
			t.Errorf("%s: max_parallel/cost = %d/%d, want 1/1", c.name, cap.MaxParallel, cap.Cost)
		}
	}
}

// --- Provider instantiation -------------------------------------------------------

// A comfyui entry instantiates the built-in ComfyUI provider with the
// config URL — same code, config-driven (board T-019). The capability
// name defaults to comfy.generate, matching the pre-registry behavior.
func TestLoadComfyUI(t *testing.T) {
	p := write(t, `
providers:
  - type: comfyui
    url: http://127.0.0.1:8188/
    start_cmd: "C:/comfy/run.bat"
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(provs) != 1 {
		t.Fatalf("providers = %d, want 1", len(provs))
	}
	inner := provs[0].(*configured).inner
	c, ok := inner.(*comfyui.ComfyUI)
	if !ok {
		t.Fatalf("provider type = %T, want *comfyui.ComfyUI", inner)
	}
	cap := c.Capability()
	if cap.Name != "comfy.generate" || !cap.LongRun {
		t.Errorf("capability = %+v", cap)
	}
	// HealthProber interface still reachable through the registry wrapper.
	if _, ok := provs[0].(provider.HealthProber); !ok {
		t.Error("comfyui provider lost its HealthProber through the registry")
	}
}

// An explicit name/summary/schema override lands in the capability.
func TestLoadOverrides(t *testing.T) {
	p := write(t, `
providers:
  - name: custom.gen
    type: comfyui
    url: http://127.0.0.1:8188
    summary: "Custom generator"
    long_run: true
    input_schema:
      type: object
      properties:
        prompt:
          type: string
      required: [prompt]
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cap := provs[0].Capability()
	if cap.Name != "custom.gen" || cap.Summary != "Custom generator" || !cap.LongRun {
		t.Errorf("capability = %+v", cap)
	}
	var schema map[string]any
	if err := json.Unmarshal(cap.InputSchema, &schema); err != nil {
		t.Fatalf("input_schema not JSON: %v", err)
	}
	if _, ok := schema["required"]; !ok {
		t.Errorf("schema = %v, want required preserved", schema)
	}
}

// An http entry instantiates the generic REST provider.
func TestLoadHTTP(t *testing.T) {
	p := write(t, `
providers:
  - name: render.doc
    type: http
    health_url: http://127.0.0.1:9000/health
    exec:
      method: PUT
      url_template: http://127.0.0.1:9000/api/{{task_id}}
      headers:
        X-Api-Key: key
      body_template: '{"file": "{{file}}"}'
      result_path: result
      artifact_paths: [files.0]
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hp := provs[0].(*configured).inner
	if _, ok := hp.(*httpprovider.Provider); !ok {
		t.Fatalf("provider type = %T, want *httpprovider.Provider", hp)
	}
	if cap := hp.Capability(); cap.Name != "render.doc" {
		t.Errorf("capability name = %q", cap.Name)
	}
}

// A process entry instantiates the host-command provider.
func TestLoadProcess(t *testing.T) {
	p := write(t, `
providers:
  - name: script.cleanup
    type: process
    command: ["python", "C:/scripts/cleanup.py", "--target", "{{target}}"]
    artifacts_glob: "out/*.png"
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cap := provs[0].Capability(); cap.Name != "script.cleanup" {
		t.Errorf("capability name = %q", cap.Name)
	}
}

// The optional per-provider `workspace` scopes Exec into a subdirectory of
// the daemon workspace (config-controlled artifact root).
func TestWorkspaceScoping(t *testing.T) {
	// Use the process provider as a probe: it runs with cmd.Dir = workspace.
	p := write(t, `
providers:
  - name: script.where
    type: process
    command: ["sh", "-c", "pwd > where.txt"]
    workspace: sub/dir
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ws := t.TempDir()
	if _, _, execErr := provs[0].Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, ws); execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	raw, err := os.ReadFile(filepath.Join(ws, "sub", "dir", "where.txt"))
	if err != nil {
		t.Fatalf("workspace scoping: %v", err)
	}
	gotDir := strings.TrimSpace(string(raw))
	if gotDir != filepath.Join(ws, "sub", "dir") {
		t.Errorf("exec cwd = %q, want %q", gotDir, filepath.Join(ws, "sub", "dir"))
	}
}

// Multiple entries load in order and keep their capability names — the
// server dispatches by name (the "dispatch registry").
func TestLoadMultiple(t *testing.T) {
	p := write(t, `
providers:
  - name: script.one
    type: process
    command: ["sh", "-c", "true"]
  - name: comfy.generate
    type: comfyui
    url: http://127.0.0.1:8188
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(provs) != 2 {
		t.Fatalf("providers = %d, want 2", len(provs))
	}
	if provs[0].Capability().Name != "script.one" || provs[1].Capability().Name != "comfy.generate" {
		t.Errorf("names = %q, %q", provs[0].Capability().Name, provs[1].Capability().Name)
	}
}

// --- Instantiate (spec-exec factory, board T-044) -------------------------------

// The spec mapping decodes through the SAME entry schema as providers.yaml
// (snake_case fields; JSON is a YAML subset): typed fields land on the
// runner, unknown keys are dropped, and the URL name overrides any spec
// name.
func TestInstantiateSpecDecodesEntrySchema(t *testing.T) {
	spec := map[string]any{
		"name":         "deliberately.wrong",
		"type":         "process",
		"command":      []any{"echo", "spec-built"},
		"stdin_json":   false,
		"workspace":    "spec-out",
		"max_parallel": float64(3),
		"summary":      "from spec",
	}
	p, err := Instantiate("spec.cap", spec)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	cap := p.Capability()
	if cap.Name != "spec.cap" {
		t.Errorf("capability name = %q, want spec.cap (URL label overrides spec name)", cap.Name)
	}
	if cap.Summary != "from spec" {
		t.Errorf("summary = %q, want from spec", cap.Summary)
	}
	result, _, err2 := p.Exec(context.Background(), "t1", json.RawMessage("{}"), 10*time.Second, t.TempDir())
	if err2 != nil {
		t.Fatalf("Exec: %v", err2)
	}
	if result["stdout"] != "spec-built" {
		t.Errorf("stdout = %v, want spec-built", result["stdout"])
	}
}

// Unknown keys in the spec are dropped (never invented at the runner),
// and mis-shaped specs fail loudly — the server maps the error to 422.
func TestInstantiateSpecErrors(t *testing.T) {
	if _, err := Instantiate("bad@name", map[string]any{"type": "process", "command": []any{"echo"}}); err == nil {
		t.Error("invalid capability name: want error, got nil")
	}
	if _, err := Instantiate("x.cap", map[string]any{"type": "smoke_signal"}); err == nil {
		t.Error("unknown type: want error, got nil")
	} else if !strings.Contains(err.Error(), "smoke_signal") {
		t.Errorf("unknown-type error %q does not mention the type", err)
	}
	// Unknown keys ignored: the same entry with an extra key still builds.
	if _, err := Instantiate("x.cap", map[string]any{"type": "process", "command": []any{"echo"}, "some_future_field": true}); err != nil {
		t.Errorf("unknown spec key = unexpected error: %v", err)
	}
}
