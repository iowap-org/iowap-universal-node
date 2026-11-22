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
