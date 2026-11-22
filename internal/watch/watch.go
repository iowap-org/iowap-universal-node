// Package watch keeps the provider registry current (board T-022): the
// daemon observes <workspace>/bridge/providers.yaml — the shared channel
// the node dashboard (T-021) writes capability definitions through — and
// hot-reloads the registry when it appears, changes or disappears.
//
// Precedence rule: the workspace file OVERRIDES IOWAP_PROVIDERS_FILE
// whenever it exists. Missing workspace file → fall back to the
// IOWAP_PROVIDERS_FILE registry (T-019 semantics).
//
// Watch semantics (deliberately different from startup): a broken file
// logs a warning and keeps the last good registry — the watch loop never
// crashes the daemon and never serves an empty manifest over a
// transiently broken edit. Startup (Resolve) stays loud: an invalid file
// refuses to start.
package watch

import (
	"context"
	"crypto/sha256"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/registry"
)

// DefaultInterval is the poll cadence (T-022: 10 s — plain polling, no
// fsnotify: Windows-compatible and simple).
const DefaultInterval = 10 * time.Second

// WorkspaceProvidersPath returns the workspace-side providers file:
// <workspace>/bridge/providers.yaml (the bridge/ subdir is shared with
// the node container's dashboard, T-021).
func WorkspaceProvidersPath(workspace string) string {
	return filepath.Join(workspace, "bridge", "providers.yaml")
}

// Options configures resolution and watching.
type Options struct {
	// WorkspaceFile is <workspace>/bridge/providers.yaml (highest precedence).
	WorkspaceFile string
	// FallbackFile is the IOWAP_PROVIDERS_FILE registry (T-019).
	FallbackFile string
	// Interval is the poll cadence; DefaultInterval when zero.
	Interval time.Duration
	// Logf receives warnings; log.Printf when nil.
	Logf func(format string, args ...any)
}

func (o Options) interval() time.Duration {
	if o.Interval > 0 {
		return o.Interval
	}
	return DefaultInterval
}

func (o Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
		return
	}
	log.Printf("iowap-bridge: "+format, args...)
}

// fileState identifies a file version by content hash: mtime alone is
// unreliable across filesystems and same-size rewrites.
type fileState struct {
	exists bool
	hash   [sha256.Size]byte
}

func statState(path string) fileState {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, hash: sha256.Sum256(raw)}
}

// Resolve loads the effective registry once: the workspace file when it
// exists, otherwise the fallback file. Returns the providers and the
// source path actually used ("" when no file exists anywhere). An invalid
// file is an error — startup refuses to run on one (T-019 semantics).
func Resolve(opts Options) ([]provider.Provider, string, error) {
	if st := statState(opts.WorkspaceFile); st.exists {
		provs, err := registry.Load(opts.WorkspaceFile)
		if err != nil {
			return nil, opts.WorkspaceFile, err
		}
		return provs, opts.WorkspaceFile, nil
	}
	provs, err := registry.Load(opts.FallbackFile)
	if err != nil {
		return nil, opts.FallbackFile, err
	}
	if len(provs) == 0 && !statState(opts.FallbackFile).exists {
		return provs, "", nil
	}
	return provs, opts.FallbackFile, nil
}

// ChangeFunc is called with every (re)loaded good registry: once
// synchronously at Watch start, then on every detected change. It runs on
// the watch goroutine — keep it fast; it must never panic the daemon.
type ChangeFunc func(source string, provs []provider.Provider)

// Watch resolves the registry and polls for changes until ctx is done.
// A parse error on change logs a warning and keeps the last good
// registry (the callback is not called with broken state).
func Watch(ctx context.Context, opts Options, onChange ChangeFunc) {
	if onChange == nil {
		return
	}
	logf := opts.logf

	provs, source, err := Resolve(opts)
	if err != nil {
		// The initial resolution is the caller's startup decision —
		// deliver empty state and let the loud failure happen in main.
		// (main calls Resolve itself before Watch for exactly that.)
		logf("workspace provider watch: initial resolution failed: %v", err)
		provs, source = nil, ""
	} else {
		onChange(source, provs)
		logf("provider registry loaded from %s (%d provider(s))", sourceOr(source), len(provs))
	}

	last := statePair{
		workspace: statState(opts.WorkspaceFile),
		fallback:  statState(opts.FallbackFile),
	}
	ticker := time.NewTicker(opts.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := statePair{
			workspace: statState(opts.WorkspaceFile),
			fallback:  statState(opts.FallbackFile),
		}
		if now == last {
			continue
		}
		last = now
		newProvs, newSource, err := Resolve(opts)
		if err != nil {
			// Broken edit: warn loudly, keep serving the last good
			// registry — never crash, never drop to empty.
			logf("workspace provider watch: ignoring broken providers file (%v) — keeping the last good registry", err)
			continue
		}
		provs, source = newProvs, newSource
		logf("provider registry reloaded from %s (%d provider(s))", sourceOr(source), len(provs))
		onChange(source, provs)
	}
}

// statePair tracks both watched files: the workspace file (so an
// APPEARING workspace file is picked up even while the fallback registry
// is the current source) and the fallback file (so fallback edits reach a
// daemon whose current source is the fallback).
type statePair struct {
	workspace fileState
	fallback  fileState
}

func sourceOr(source string) string {
	if source == "" {
		return "(no providers file)"
	}
	return source
}
