// iowap-bridge is the native host-side bridge daemon (board T-006):
// it exposes host capabilities to the universal node container over the
// Manifest/Exec contract (SPEC §3/§5), authenticated with a Bearer
// token and bound to loopback only.
//
// Since v0.2.0 (board DEC #8) the daemon is a pure execution layer: it
// runs the bootstrap (engine check, vendored compose profile, container
// lifecycle, board T-012), resolves its tokens automatically (v0.1.1,
// internal/tokenres) and serves the Manifest/Exec API for the providers
// configured in providers.yaml (board T-019). The onboarding UI and the
// MCP bearer/snippet display live in the node container (board T-017,
// port 7783) — not here.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/bootstrap"
	"github.com/iowap-org/iowap-bridge-daemon/internal/config"
	"github.com/iowap-org/iowap-bridge-daemon/internal/console"
	"github.com/iowap-org/iowap-bridge-daemon/internal/dockerx"
	"github.com/iowap-org/iowap-bridge-daemon/internal/logging"
	"github.com/iowap-org/iowap-bridge-daemon/internal/openbrowser"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provision"
	"github.com/iowap-org/iowap-bridge-daemon/internal/server"
	"github.com/iowap-org/iowap-bridge-daemon/internal/tokenres"
	"github.com/iowap-org/iowap-bridge-daemon/internal/tray"
	"github.com/iowap-org/iowap-bridge-daemon/internal/watch"
)

func main() {
	// ── Console visibility (v0.3.1, Ronny feedback #2): release builds run
	// windowless (windowsgui subsystem, see release.yml). --debug or
	// IOWAP_BRIDGE_DEBUG=1 allocates a console; without it, bootstrap
	// messages additionally land in %LOCALAPPDATA%\iowap-bridge\daemon.log.
	// Must run before any logging so even the earliest failure is visible
	// somewhere.
	debug := console.Debug(os.Args[1:], os.Getenv)
	console.Show(debug)
	setLogOutput(debug)

	cfg, err := config.LoadFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: refusing to start: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	eng, err := dockerx.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: %v\n", err)
		os.Exit(1)
	}

	// ── Token resolution (v0.1.1): explicit env override → reuse from the
	// existing universal-node container → generate (fresh install). The
	// daemon never demands tokens from the user; fail-closed remains as the
	// last stage (generation failure → exit 1, never serve tokenless).
	log.Printf("iowap-bridge: checking Docker Engine…")
	if err := eng.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: docker engine not reachable (%v) — install or start Docker Desktop, then run the daemon again\n", err)
		os.Exit(1)
	}
	tokens, err := tokenres.Resolve(ctx, tokenres.Options{
		GetEnv:              os.Getenv,
		ContainerName:       bootstrap.ContainerName,
		InspectEnv:          eng.InspectEnv,
		GenerateBridgeToken: provision.GenerateBridgeToken,
		GenerateBearer:      provision.GenerateBearer,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: refusing to start: %v\n", err)
		os.Exit(1)
	}
	switch {
	case tokens.BearerSource == tokenres.SourceContainer:
		log.Printf("iowap-bridge: tokens reused from existing %s container", bootstrap.ContainerName)
	case tokens.BearerSource == tokenres.SourceGenerated:
		log.Printf("iowap-bridge: fresh install — bridge token and MCP bearer generated (no env setup needed)")
	}

	if err := os.MkdirAll(cfg.WorkspaceDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: cannot create workspace dir %q: %v\n", cfg.WorkspaceDir, err)
		os.Exit(1)
	}
	workspace, err := filepath.Abs(cfg.WorkspaceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: cannot resolve workspace dir: %v\n", err)
		os.Exit(1)
	}

	// ── Bootstrap (T-012/T-014): container lifecycle with the resolved
	// tokens. Exits 1 with an actionable message when it cannot proceed.
	result, err := bootstrap.Run(ctx, eng, bootstrap.Options{
		BridgeToken:  tokens.BridgeToken,
		MCPBearer:    tokens.MCPBearer,
		WorkspaceDir: workspace,
		RunCompose:   bootstrap.ExecCompose,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: bootstrap failed: %v\n", err)
		os.Exit(1)
	}
	if result.Skipped {
		log.Printf("iowap-bridge: container %s already running — bootstrap skipped", bootstrap.ContainerName)
	}
	if result.Restarted {
		log.Printf("iowap-bridge: existing container %s restarted as-is — tokens unchanged", bootstrap.ContainerName)
	}

	// ── Hand-off to the node-side onboarding UI (v0.2.0, board T-017/T-018):
	// the natural end of every successful bootstrap — created, restarted or
	// already running — is opening the registration page (which doubles as
	// the bearer/snippet display) in the default browser. Best-effort: when
	// no browser can be launched, the URL is the last console line.
	const onboardingURL = "http://127.0.0.1:7783"
	if err := openbrowser.Open(onboardingURL); err != nil {
		log.Printf("iowap-bridge: open %s in your browser to register the node (bearer and config snippets are shown there as well)", onboardingURL)
	}

	// ── Provider registry (T-019/T-022): the effective registry is the
	// workspace file <workspace>/bridge/providers.yaml when it exists
	// (written by the node dashboard), otherwise IOWAP_PROVIDERS_FILE.
	// Missing or empty → empty manifest, never a crash; a broken file
	// refuses to start.
	workspaceWatch := watch.Options{
		WorkspaceFile: watch.WorkspaceProvidersPath(workspace),
		FallbackFile:  cfg.ProvidersFile,
	}
	provs, registrySource, err := watch.Resolve(workspaceWatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: %v\n", err)
		os.Exit(1)
	}
	if len(provs) == 0 {
		log.Printf("iowap-bridge: no providers configured (%s missing or empty) — serving an empty manifest", registrySource)
	}

	srv := server.New(tokens.BridgeToken, workspace)
	srv.ReplaceProviders(ctx, 15*time.Second, provs...)

	// One synchronous probe per provider up front so the manifest is
	// accurate from the first poll; the background loops keep it fresh
	// (15 s cadence, generalized health probing).
	for _, p := range provs {
		if hp, ok := p.(provider.HealthProber); ok {
			hp.ProbeOnce(ctx)
		}
	}

	// ── Workspace provider watch (T-022): hot-reload the registry when
	// <workspace>/bridge/providers.yaml appears, changes or disappears
	// (poll every 10 s). A broken file keeps the last good registry —
	// the daemon never crashes over an edit.
	go watch.Watch(ctx, workspaceWatch, func(source string, newProvs []provider.Provider) {
		for _, p := range newProvs {
			if hp, ok := p.(provider.HealthProber); ok {
				hp.ProbeOnce(ctx)
			}
		}
		srv.ReplaceProviders(ctx, 15*time.Second, newProvs...)
	})

	// ── Tray icon (T-023, Windows-first): status widget + menu. Best
	// effort — the daemon serves with or without a working tray.
	runTray(eng)

	ln, err := net.Listen("tcp", cfg.BindAddr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: cannot listen on %s: %v\n", cfg.BindAddr(), err)
		os.Exit(1)
	}
	log.Printf("iowap-bridge %s listening on %s (loopback only), workspace %s, %d provider(s) from %s",
		server.Version, cfg.BindAddr(), workspace, len(provs), registrySource)
	if err := http.Serve(ln, srv.Handler()); err != nil {
		log.Fatalf("iowap-bridge: server error: %v", err)
	}
}

// setLogOutput routes the standard logger. In debug mode stderr stays the
// sink (the console is visible). Without it, messages additionally append
// to %LOCALAPPDATA%\iowap-bridge\daemon.log (file first in the MultiWriter
// so a failed stderr write — invalid handle on windowsgui — never eats a
// log line). Best effort: a broken log dir never keeps the daemon from
// starting; logging then stays stderr-only.
func setLogOutput(debug bool) {
	if debug {
		return
	}
	path, err := logging.ResolvePath(os.Getenv, os.UserHomeDir)
	if err != nil {
		return
	}
	f, err := logging.Open(path, logging.MaxLogSize)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}

// trayController adapts the dockerx client method names onto the tray's
// ContainerController interface.
type trayController struct{ eng *dockerx.Client }

func (t trayController) Running(ctx context.Context, name string) (bool, error) {
	return t.eng.ContainerRunning(ctx, name)
}
func (t trayController) Stop(ctx context.Context, name string) error {
	return t.eng.StopContainer(ctx, name)
}
func (t trayController) Start(ctx context.Context, name string) error {
	return t.eng.StartContainer(ctx, name)
}

// runTray starts the Windows tray icon (board T-023). It runs entirely in
// a goroutine and is crash-proof: any panic or error is logged, the daemon
// keeps serving (a missing tray must never take down the bridge).
func runTray(eng *dockerx.Client) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("iowap-bridge: tray panicked (daemon keeps running): %v", r)
			}
		}()
		tray.Run(tray.App{
			Ctrl:          trayController{eng: eng},
			DaemonVersion: server.Version,
			ImageRef:      tray.NodeImage,
			RepoDigestsOf: eng.ImageRepoDigests,
			Shutdown: func() {
				log.Printf("iowap-bridge: quit requested from the tray")
				os.Exit(0)
			},
		})
	}()
}
