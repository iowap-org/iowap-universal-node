package process

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// The process tests spawn real shell commands — POSIX only (CI and the
// local check run the golang:1.23 linux image).
func needShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process provider tests need a POSIX shell")
	}
}

func testConfig(command []string) Config {
	return Config{Name: "script.cleanup", Summary: "Run a script", Command: command}
}

// Happy path: argv substitution, stdin JSON, stdout in the result and
// artifact glob collection.
func TestExecHappyPath(t *testing.T) {
	needShell(t)
	ws := t.TempDir()
	// Script: read the stdin JSON, write two artifacts, echo a line.
	script := `
in=$(cat)
echo "$in" > in.json
mkdir -p out
echo "A" > out/a.png
echo "B" > out/b.png
echo "done"`
	cfg := testConfig([]string{"sh", "-c", script})
	cfg.ArtifactsGlob = "out/*.png"
	p := New(cfg)

	result, artifacts, execErr := p.Exec(context.Background(), "t-1",
		json.RawMessage(`{"target":"x","n":2}`), time.Minute, ws)
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}

	// stdin received the raw params JSON.
	raw, err := os.ReadFile(filepath.Join(ws, "in.json"))
	if err != nil || strings.TrimSpace(string(raw)) != `{"target":"x","n":2}` {
		t.Errorf("stdin JSON = %q, err %v", raw, err)
	}
	// Artifacts globbed, sorted, workspace-relative.
	if len(artifacts) != 2 || artifacts[0].Path != "out/a.png" || artifacts[0].Name != "a.png" ||
		artifacts[1].Path != "out/b.png" {
		t.Errorf("artifacts = %+v", artifacts)
	}
	// Result carries exit code and stdout.
	if result["exit_code"] != 0 || result["stdout"] != "done" {
		t.Errorf("result = %v", result)
	}
}

// Argv templates substitute params and the task ID; a missing variable is
// invalid_params.
func TestExecArgvSubstitution(t *testing.T) {
	needShell(t)
	ws := t.TempDir()
	p := New(testConfig([]string{"sh", "-c", `echo "$1" > out.txt`, "echo", "{{task_id}}-{{name}}"}))
	result, _, execErr := p.Exec(context.Background(), "t-7", json.RawMessage(`{"name":"doc"}`), time.Minute, ws)
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, "out.txt"))
	if strings.TrimSpace(string(raw)) != "t-7-doc" {
		t.Errorf("argv substitution produced %q", raw)
	}
	if result["exit_code"] != 0 {
		t.Errorf("result = %v", result)
	}

	// Missing variable → terminal invalid_params, nothing executed.
	p = New(testConfig([]string{"sh", "-c", `echo boom > shouldNotExist.txt`, "echo", "{{missing}}"}))
	_, _, execErr = p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, ws)
	if execErr == nil || execErr.Code != provider.CodeInvalidParams {
		t.Errorf("missing variable: %v", execErr)
	}
	if _, err := os.Stat(filepath.Join(ws, "shouldNotExist.txt")); err == nil {
		t.Error("command ran despite missing substitution variable")
	}
}

// Non-object params are invalid_params.
func TestExecNonObjectParams(t *testing.T) {
	needShell(t)
	p := New(testConfig([]string{"sh", "-c", "true"}))
	if _, _, err := p.Exec(context.Background(), "t", json.RawMessage(`[1]`), time.Minute, ""); err == nil || err.Code != provider.CodeInvalidParams {
		t.Errorf("err = %v, want invalid_params", err)
	}
}

// A non-zero exit is a terminal capability.failed carrying the stderr tail.
func TestExecNonZeroExit(t *testing.T) {
	needShell(t)
	p := New(testConfig([]string{"sh", "-c", "echo some stderr >&2; exit 3"}))
	_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeCapabilityFailed || execErr.Retryable {
		t.Fatalf("execErr = %v, want terminal capability.failed", execErr)
	}
	if !strings.Contains(execErr.Message, "some stderr") {
		t.Errorf("message %q lacks the stderr tail", execErr.Message)
	}
}

// A command that cannot be started is provider.unavailable (retryable).
func TestExecStartFailure(t *testing.T) {
	needShell(t)
	p := New(testConfig([]string{"/nonexistent/binary-xyz"}))
	_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeProviderUnavail || !execErr.Retryable {
		t.Errorf("execErr = %v, want retryable provider.unavailable", execErr)
	}
}

// Exceeding the timeout kills the process → retryable timeout.
func TestExecTimeout(t *testing.T) {
	needShell(t)
	p := New(testConfig([]string{"sh", "-c", "sleep 5"}))
	_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), 100*time.Millisecond, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeTimeout || !execErr.Retryable {
		t.Errorf("execErr = %v, want retryable timeout", execErr)
	}
}

// stdin_json: false keeps the params away from the process stdin.
func TestExecStdinDisabled(t *testing.T) {
	needShell(t)
	ws := t.TempDir()
	no := false
	cfg := testConfig([]string{"sh", "-c", `wc -c < /dev/stdin > bytes.txt`})
	cfg.StdinJSON = &no
	p := New(cfg)
	if _, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{"a":1}`), time.Minute, ws); execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, "bytes.txt"))
	if strings.TrimSpace(string(raw)) != "0" {
		t.Errorf("stdin had %q bytes content", raw)
	}
}

// No artifacts_glob → no artifacts, no error.
func TestExecNoGlob(t *testing.T) {
	needShell(t)
	p := New(testConfig([]string{"sh", "-c", "true"}))
	_, artifacts, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, t.TempDir())
	if execErr != nil || artifacts != nil {
		t.Errorf("artifacts = %v, err = %v", artifacts, execErr)
	}
}

// An empty glob match set is fine (the command simply produced nothing).
func TestExecGlobNoMatches(t *testing.T) {
	needShell(t)
	cfg := testConfig([]string{"sh", "-c", "true"})
	cfg.ArtifactsGlob = "out/*.png"
	p := New(cfg)
	_, artifacts, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, t.TempDir())
	if execErr != nil || len(artifacts) != 0 {
		t.Errorf("artifacts = %v, err = %v", artifacts, execErr)
	}
}

// An invalid glob pattern fails the task terminally.
func TestExecInvalidGlob(t *testing.T) {
	needShell(t)
	cfg := testConfig([]string{"sh", "-c", "true"})
	cfg.ArtifactsGlob = "out/[" // malformed pattern
	p := New(cfg)
	_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeCapabilityFailed {
		t.Errorf("execErr = %v, want capability.failed", execErr)
	}
}

// --- Health --------------------------------------------------------------------

func TestHealthResolvable(t *testing.T) {
	needShell(t)
	p := New(testConfig([]string{"sh", "-c", "true"}))
	if got := p.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("ProbeOnce = %q, want ok", got)
	}
	if got := p.Health(); got != provider.HealthOK {
		t.Errorf("Health = %q, want ok", got)
	}

	missing := New(testConfig([]string{"/nonexistent/binary-xyz"}))
	for i := 0; i < 3; i++ {
		if got := missing.ProbeOnce(context.Background()); got != provider.HealthDown {
			t.Errorf("probe %d = %q, want down", i+1, got)
		}
	}
	cap := missing.Capability()
	if cap.Health != provider.HealthDown || cap.Name != "script.cleanup" {
		t.Errorf("capability = %+v", cap)
	}
}

// An empty command is never healthy.
func TestHealthEmptyCommand(t *testing.T) {
	p := New(Config{Name: "x"})
	if got := p.Health(); got != provider.HealthDown {
		t.Errorf("Health = %q, want down", got)
	}
}

// T-048 (advisory probe accuracy): a RELATIVE script path resolves
// against the daemon's process cwd — probe-ok exactly when the file
// exists there; the health field is advisory metadata either way.
func TestHealthRelativeScriptResolvesAgainstDaemonCwd(t *testing.T) {
	needShell(t)
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "scripts", "tool.py"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer os.Chdir(old)
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	present := New(testConfig([]string{"scripts/tool.py", "--run"}))
	if got := present.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("ProbeOnce (script present relative to daemon cwd) = %q, want ok", got)
	}

	// Same entry, but the file is gone → probes down (still only the
	// advisory field changes; the manifest keeps the entry).
	absent := New(testConfig([]string{"scripts/tool.py", "--run"}))
	if err := os.Remove(filepath.Join(tmp, "scripts", "tool.py")); err != nil {
		t.Fatalf("remove script: %v", err)
	}
	for i := 0; i < 3; i++ {
		if got := absent.ProbeOnce(context.Background()); got != provider.HealthDown {
			t.Errorf("probe %d (script gone) = %q, want down", i+1, got)
		}
	}
}
