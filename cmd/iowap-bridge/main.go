// iowap-bridge is the native host-side bridge daemon (board T-006):
// it exposes host capabilities to the universal node container over the
// Manifest/Exec contract (SPEC §3/§5), authenticated with a Bearer
// token and bound to loopback only.
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

	"github.com/iowap-org/iowap-bridge-daemon/internal/config"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/comfyui"
	"github.com/iowap-org/iowap-bridge-daemon/internal/server"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		// Fail closed before any socket exists (SPEC §2).
		fmt.Fprintf(os.Stderr, "iowap-bridge: refusing to start: %v\n", err)
		os.Exit(1)
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

	comfy := comfyui.New(cfg.ComfyUIURL, cfg.ComfyUICmd)

	// One synchronous probe up front so the manifest is accurate from
	// the first poll; the background loop keeps it fresh (15 s cadence,
	// start-on-demand when IOWAP_COMFYUI_CMD is configured).
	comfy.ProbeOnce(context.Background())

	srv := server.New(cfg.Token, workspace, comfy)
	srv.StartHealthLoops(context.Background(), 15*time.Second)

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
