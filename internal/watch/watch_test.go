package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// httpProviderYAML is a minimal valid providers.yaml with one http provider.
func httpProviderYAML(name, urlTemplate string) string {
	return `providers:
  - name: ` + name + `
    type: http
    exec:
      method: POST
      url_template: ` + urlTemplate + `
`
}

// reloadLog records every onChange delivery.
type reloadLog struct {
	mu      sync.Mutex
	sources []string
	names   [][]string
}

func (l *reloadLog) record(source string, provs []provider.Provider) {
	names := make([]string, 0, len(provs))
	for _, p := range provs {
		names = append(names, p.Capability().Name)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sources = append(l.sources, source)
	l.names = append(l.names, names)
}

func (l *reloadLog) snapshot() (sources []string, names [][]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.sources...),
		append([][]string(nil), l.names...)
}

// waitFor polls until cond reports true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startWatch(t *testing.T, opts Options) *reloadLog {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := &reloadLog{}
	go Watch(ctx, opts, log.record)
	t.Cleanup(cancel)
	return log
}

// --- Resolve ---------------------------------------------------------------

func TestResolveFallsBackWhenWorkspaceFileMissing(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")
	write(t, fallback, httpProviderYAML("fallback.cap", "http://127.0.0.1:9/x"))

	provs, source, err := Resolve(Options{WorkspaceFile: WorkspaceProvidersPath(ws), FallbackFile: fallback})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if source != fallback {
		t.Fatalf("source = %q, want fallback %q", source, fallback)
	}
	if len(provs) != 1 {
		t.Fatalf("got %d providers, want 1", len(provs))
	}
}

func TestResolveWorkspaceFileHasPriority(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")
	write(t, fallback, httpProviderYAML("fallback.cap", "http://127.0.0.1:9/x"))
	workspaceFile := WorkspaceProvidersPath(ws)
	write(t, workspaceFile, httpProviderYAML("workspace.cap", "http://127.0.0.1:9/y"))

	provs, source, err := Resolve(Options{WorkspaceFile: workspaceFile, FallbackFile: fallback})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if source != workspaceFile {
		t.Fatalf("source = %q, want workspace file %q", source, workspaceFile)
	}
	if len(provs) != 1 {
		t.Fatalf("got %d providers, want 1", len(provs))
	}
}

func TestResolveBothMissingIsEmptyNoError(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")

	provs, source, err := Resolve(Options{WorkspaceFile: WorkspaceProvidersPath(ws), FallbackFile: fallback})
	if err != nil {
		t.Fatalf("Resolve with both files missing must not error: %v", err)
	}
	if len(provs) != 0 {
		t.Fatalf("got %d providers, want 0", len(provs))
	}
	if source != "" {
		t.Fatalf("source = %q, want empty", source)
	}
}

func TestResolveInvalidWorkspaceFileIsError(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")
	workspaceFile := WorkspaceProvidersPath(ws)
	write(t, workspaceFile, "providers: [ { type: nope } ]")

	_, _, err := Resolve(Options{WorkspaceFile: workspaceFile, FallbackFile: fallback})
	if err == nil {
		t.Fatal("invalid workspace file must surface an error (startup refuses to run)")
	}
}

// --- Watch loop -------------------------------------------------------------

func TestWatchWorkspaceFileAppearsChangesAndDisappears(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")
	write(t, fallback, httpProviderYAML("fallback.cap", "http://127.0.0.1:9/x"))
	workspaceFile := WorkspaceProvidersPath(ws)

	log := startWatch(t, Options{
		WorkspaceFile: workspaceFile,
		FallbackFile:  fallback,
		Interval:      20 * time.Millisecond,
	})

	// Initial: fallback registry (workspace file absent).
	waitFor(t, "initial fallback reload", func() bool {
		sources, _ := log.snapshot()
		return len(sources) >= 1 && sources[0] == fallback
	})

	// File appears → workspace registry wins.
	write(t, workspaceFile, httpProviderYAML("workspace.cap", "http://127.0.0.1:9/y"))
	waitFor(t, "workspace reload after file appears", func() bool {
		sources, names := log.snapshot()
		return len(sources) >= 2 && sources[1] == workspaceFile &&
			len(names[1]) == 1 && names[1][0] == "workspace.cap"
	})

	// File changes → registry changes.
	write(t, workspaceFile, httpProviderYAML("workspace2.cap", "http://127.0.0.1:9/z"))
	waitFor(t, "reload after file change", func() bool {
		sources, names := log.snapshot()
		return len(sources) >= 3 && len(names[2]) == 1 && names[2][0] == "workspace2.cap"
	})

	// File disappears → back to the fallback registry.
	if err := os.Remove(workspaceFile); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fallback reload after file disappears", func() bool {
		sources, _ := log.snapshot()
		return len(sources) >= 4 && sources[3] == fallback
	})
}

func TestWatchInvalidContentKeepsLastGoodRegistry(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")
	workspaceFile := WorkspaceProvidersPath(ws)
	write(t, workspaceFile, httpProviderYAML("good.cap", "http://127.0.0.1:9/x"))

	log := startWatch(t, Options{
		WorkspaceFile: workspaceFile,
		FallbackFile:  fallback,
		Interval:      20 * time.Millisecond,
	})

	waitFor(t, "initial good reload", func() bool {
		sources, _ := log.snapshot()
		return len(sources) >= 1
	})

	// Break the file: no reload may be delivered afterwards.
	write(t, workspaceFile, "providers: [ { type: nope } ]")
	time.Sleep(150 * time.Millisecond)
	sources, names := log.snapshot()
	if len(sources) != 1 {
		t.Fatalf("broken file triggered %d reloads, want 1 (only the initial good one)", len(sources))
	}
	if len(names[0]) != 1 || names[0][0] != "good.cap" {
		t.Fatalf("last good registry lost: %v", names[0])
	}
}

func TestWatchNoReloadWhenUnchanged(t *testing.T) {
	ws := t.TempDir()
	fallback := filepath.Join(t.TempDir(), "providers.yaml")
	workspaceFile := WorkspaceProvidersPath(ws)
	write(t, workspaceFile, httpProviderYAML("stable.cap", "http://127.0.0.1:9/x"))

	log := startWatch(t, Options{
		WorkspaceFile: workspaceFile,
		FallbackFile:  fallback,
		Interval:      20 * time.Millisecond,
	})

	waitFor(t, "initial reload", func() bool {
		sources, _ := log.snapshot()
		return len(sources) >= 1
	})
	time.Sleep(120 * time.Millisecond)
	if sources, _ := log.snapshot(); len(sources) != 1 {
		t.Fatalf("unchanged file triggered %d reloads, want 1", len(sources))
	}
}
