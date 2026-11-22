// iowap-bridge is the native host-side bridge daemon (board T-006):
// it exposes host capabilities to the universal node container over the
// Manifest/Exec contract (SPEC §3/§5), authenticated with a Bearer
// token and bound to loopback only.
//
// The daemon never demands token env vars from the user (v0.1.1,
// DEC #5/#6): tokens are resolved via internal/tokenres — env override →
// existing container env → generated. Before serving, the daemon runs the
// bootstrap mode (board T-012): it checks the Docker Engine and — unless
// the universal-node container already exists — brings the container up via
// the vendored iowap-docker compose profile, generating the MCP bearer for
// one-time display (T-014). The local onboarding web UI (T-010) then serves
// on port 7782.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/bootstrap"
	"github.com/iowap-org/iowap-bridge-daemon/internal/config"
	"github.com/iowap-org/iowap-bridge-daemon/internal/dockerx"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/comfyui"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provision"
	"github.com/iowap-org/iowap-bridge-daemon/internal/server"
	"github.com/iowap-org/iowap-bridge-daemon/internal/tokenres"
	"github.com/iowap-org/iowap-bridge-daemon/internal/web"
)

func main() {
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
	var view *provision.OnceView
	// One-time display only for a freshly created container with a freshly
	// generated bearer — reused tokens are never re-displayed (DEC #6,
	// hash-only afterwards).
	if result.Bearer != "" && tokens.BearerSource == tokenres.SourceGenerated {
		view = provision.NewOnceView(result.Bearer)
		log.Printf("iowap-bridge: universal node container created — MCP bearer available once at http://%s:%d",
			config.BindHost, cfg.UIPort)
	}
	if result.Skipped {
		log.Printf("iowap-bridge: container %s already running — bootstrap skipped", bootstrap.ContainerName)
	}
	if result.Restarted {
		log.Printf("iowap-bridge: existing container %s restarted as-is — tokens unchanged", bootstrap.ContainerName)
	}

	// ── Onboarding web UI (T-010), loopback only, separate port.
	ui := web.New(eng, eng, view, cfg.MCPPort)
	uiLn, err := net.Listen("tcp", fmt.Sprintf("%s:%d", config.BindHost, cfg.UIPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: cannot listen on UI port %d: %v\n", cfg.UIPort, err)
		os.Exit(1)
	}
	go func() {
		if err := http.Serve(uiLn, ui.Handler()); err != nil {
			log.Printf("iowap-bridge: onboarding UI stopped: %v", err)
		}
	}()
	log.Printf("iowap-bridge: onboarding UI listening on %s:%d (loopback only)", config.BindHost, cfg.UIPort)

	// ── Bridge contract server (T-006/T-007).
	comfy := comfyui.New(cfg.ComfyUIURL, cfg.ComfyUICmd)

	// One synchronous probe up front so the manifest is accurate from
	// the first poll; the background loop keeps it fresh (15 s cadence,
	// start-on-demand when IOWAP_COMFYUI_CMD is configured).
	comfy.ProbeOnce(ctx)

	srv := server.New(tokens.BridgeToken, workspace, comfy)
	srv.StartHealthLoops(ctx, 15*time.Second)

	ln, err := net.Listen("tcp", cfg.BindAddr())
	if err != nil {
		fmt.Fprintf(os.Stderr, "iowap-bridge: cannot listen on %s: %v\n", cfg.BindAddr(), err)
		os.Exit(1)
	}
	log.Printf("iowap-bridge %s listening on %s (loopback only), workspace %s, comfyui %s",
		server.Version, cfg.BindAddr(), workspace, cfg.ComfyUIURL)
	if err := http.Serve(ln, srv.Handler()); err != nil {
		log.Fatalf("iowap-bridge: server error: %v", err)
	}
}
