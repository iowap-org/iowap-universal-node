// Package bootstrap implements the daemon's bootstrap mode (board T-012,
// DEC #5 / DEC #7): before serving, the daemon checks the Docker Engine,
// and — unless the universal-node container is already running — brings it
// up by executing the vendored iowap-docker compose profile via the
// `docker compose` CLI. The daemon never builds its own container config:
// the vendored profile file is the source of truth.
package bootstrap

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ContainerName is the universal-node container (compose container_name).
const ContainerName = "iowap-universal-node"

// composeFile is the vendored iowap-docker profile (DEC #7: source of truth).
//
//go:embed docker-compose.universal-node.yml
var composeFile []byte

// Engine is the subset of the Docker Engine API the bootstrap needs.
// Implemented by internal/dockerx; faked in tests.
type Engine interface {
	// Ping verifies the engine is reachable (default route: named pipe on
	// Windows, unix socket elsewhere — client.FromEnv).
	Ping(ctx context.Context) error
	// ContainerRunning reports whether the named container is running.
	ContainerRunning(ctx context.Context, name string) (bool, error)
}

// RunComposeFunc executes the compose profile with the given extra
// environment variables (the compose ${...:-} interpolation reads them).
// Injected for tests; the production impl shells out to `docker compose`.
type RunComposeFunc func(ctx context.Context, composeYAML []byte, env map[string]string) error

// Result describes what the bootstrap did.
type Result struct {
	// Skipped is true when the container was already running — nothing
	// was created and no bearer was (re)generated.
	Skipped bool
	// Bearer is the freshly generated MCP bearer; empty when skipped.
	Bearer string
}

// Options configures one bootstrap run.
type Options struct {
	// BridgeToken is the daemon's IOWAP_BRIDGE_TOKEN — injected into the
	// container under the same name (SPEC §2, same trust path).
	BridgeToken string
	// WorkspaceDir is the host workspace root, mounted at /workspace.
	WorkspaceDir string
	// GenerateBearer produces the MCP bearer (injectable for tests;
	// production: provision.GenerateBearer).
	GenerateBearer func() (string, error)
	// RunCompose executes the compose profile. Required.
	RunCompose RunComposeFunc
}

// Run performs the bootstrap decision flow:
//
//  1. Engine reachable? No → actionable error ("install/start Docker
//     Desktop"), the caller exits 1.
//  2. Container already running? → skip (no recreate, no new bearer).
//  3. Otherwise: generate the MCP bearer and run the vendored compose
//     profile with IOWAP_BRIDGE_TOKEN / IOWAP_MCP_BEARER /
//     IOWAP_WORKSPACE_DIR set.
func Run(ctx context.Context, eng Engine, opts Options) (Result, error) {
	if eng == nil {
		return Result{}, fmt.Errorf("bootstrap: no docker engine client")
	}
	if opts.RunCompose == nil {
		return Result{}, fmt.Errorf("bootstrap: no compose runner configured")
	}
	if opts.GenerateBearer == nil {
		return Result{}, fmt.Errorf("bootstrap: no bearer generator configured")
	}

	if err := eng.Ping(ctx); err != nil {
		return Result{}, fmt.Errorf("docker engine not reachable (%v) — install or start Docker Desktop, then run the daemon again", err)
	}

	running, err := eng.ContainerRunning(ctx, ContainerName)
	if err != nil {
		return Result{}, fmt.Errorf("checking container %s: %w", ContainerName, err)
	}
	if running {
		return Result{Skipped: true}, nil
	}

	bearer, err := opts.GenerateBearer()
	if err != nil {
		return Result{}, err
	}

	err = opts.RunCompose(ctx, composeFile, map[string]string{
		"IOWAP_BRIDGE_TOKEN":  opts.BridgeToken,
		"IOWAP_MCP_BEARER":    bearer,
		"IOWAP_WORKSPACE_DIR": opts.WorkspaceDir,
	})
	if err != nil {
		return Result{}, fmt.Errorf("starting %s via compose profile: %w", ContainerName, err)
	}
	return Result{Bearer: bearer}, nil
}

// ExecCompose is the production RunComposeFunc: it writes the profile to a
// temp file and runs `docker compose -f <file> up -d` (Docker Desktop ships
// the compose CLI; if it is missing that is an installation problem, not
// something the daemon works around).
func ExecCompose(ctx context.Context, composeYAML []byte, env map[string]string) error {
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("docker CLI not found — install or start Docker Desktop (the compose CLI ships with it)")
	}

	f, err := os.CreateTemp("", "iowap-universal-node-*.yml")
	if err != nil {
		return fmt.Errorf("writing compose profile: %w", err)
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	if _, err := f.Write(composeYAML); err != nil {
		f.Close()
		return fmt.Errorf("writing compose profile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing compose profile: %w", err)
	}

	cmd := exec.CommandContext(ctx, dockerBin, "compose", "-f", tmpName, "up", "-d")
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up failed: %v — output: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
