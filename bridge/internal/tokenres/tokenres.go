// Package tokenres resolves the daemon's tokens (v0.1.1, DEC #5 / DEC #6):
// the daemon never requires the user to set token env vars — a fresh
// Windows machine must start with a plain exe double-click.
//
// Resolution order per token (IOWAP_BRIDGE_TOKEN, IOWAP_MCP_BEARER):
//
//  1. Explicit env override — power users keep today's behavior.
//  2. Existing iowap-universal-node container — tokens are reused from
//     the container env (docker inspect). The container IS the secret
//     store: this covers daemon restarts beside a running node (autostart,
//     PC reboot) without the daemon persisting any secret.
//  3. Fresh install — the daemon generates both tokens (crypto/rand,
//     same logic as internal/provision) and passes them to bootstrap.
//
// Fail-closed is preserved as the LAST stage: only if generation fails
// (practically never) does Resolve return an error and the daemon exit —
// the daemon never serves tokenless. The bridge server itself still
// requires a valid Bearer on every endpoint (SPEC §2); what changed is
// where the token comes from, not whether it is required.
package tokenres

import (
	"context"
	"fmt"
	"strings"
)

// Env keys resolved by this package.
const (
	BridgeEnvKey = "IOWAP_BRIDGE_TOKEN"
	BearerEnvKey = "IOWAP_MCP_BEARER"
)

// Source records where a resolved token came from.
type Source string

const (
	SourceEnv       Source = "env"
	SourceContainer Source = "container"
	SourceGenerated Source = "generated"
)

// Tokens is the resolved token pair.
type Tokens struct {
	BridgeToken  string
	BridgeSource Source
	MCPBearer    string
	BearerSource Source
}

// Inspector reads a container's configured environment (implemented by
// internal/dockerx via the Docker Engine inspect API).
type Inspector interface {
	// InspectEnv returns the container's env entries and whether the
	// container exists. Missing container: (nil, false, nil).
	InspectEnv(ctx context.Context, name string) ([]string, bool, error)
}

// Options configures one Resolve run. All fields are required.
type Options struct {
	// GetEnv reads the process environment (injected for tests).
	GetEnv func(string) string
	// ContainerName is the universal-node container name.
	ContainerName string
	// InspectEnv reads the existing container's env (may be nil only when
	// both tokens are guaranteed to come from the env — tests).
	InspectEnv func(ctx context.Context, name string) ([]string, bool, error)
	// GenerateBridgeToken produces the bridge token (production:
	// provision.GenerateBridgeToken).
	GenerateBridgeToken func() (string, error)
	// GenerateBearer produces the MCP bearer (production:
	// provision.GenerateBearer).
	GenerateBearer func() (string, error)
}

// Resolve applies the resolution order to both tokens. Per token the stages
// run independently: a mixed state (e.g. bridge token overridden via env,
// bearer reused from the container) resolves per-token, not all-or-nothing.
func Resolve(ctx context.Context, opts Options) (Tokens, error) {
	if opts.GetEnv == nil {
		return Tokens{}, fmt.Errorf("tokenres: no env getter configured")
	}
	if opts.GenerateBridgeToken == nil || opts.GenerateBearer == nil {
		return Tokens{}, fmt.Errorf("tokenres: no token generator configured")
	}
	if opts.ContainerName == "" {
		return Tokens{}, fmt.Errorf("tokenres: no container name configured")
	}

	var containerEnv map[string]string
	needContainer := strings.TrimSpace(opts.GetEnv(BridgeEnvKey)) == "" ||
		strings.TrimSpace(opts.GetEnv(BearerEnvKey)) == ""
	if needContainer && opts.InspectEnv != nil {
		entries, exists, err := opts.InspectEnv(ctx, opts.ContainerName)
		if err != nil {
			// Fail closed: an unreadable container env is not silently
			// papered over with a fresh token — the daemon would then
			// disagree with the node about the bridge token.
			return Tokens{}, fmt.Errorf("reading env of existing container %s: %w", opts.ContainerName, err)
		}
		if exists {
			containerEnv = ParseContainerEnv(entries)
		}
	}

	bridge, bridgeSrc, err := resolveToken(opts.GetEnv(BridgeEnvKey), containerEnv, BridgeEnvKey, opts.GenerateBridgeToken)
	if err != nil {
		return Tokens{}, err
	}
	bearer, bearerSrc, err := resolveToken(opts.GetEnv(BearerEnvKey), containerEnv, BearerEnvKey, opts.GenerateBearer)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{
		BridgeToken:  bridge,
		BridgeSource: bridgeSrc,
		MCPBearer:    bearer,
		BearerSource: bearerSrc,
	}, nil
}

// resolveToken runs the three stages for one token.
func resolveToken(override string, containerEnv map[string]string, key string, generate func() (string, error)) (string, Source, error) {
	if v := strings.TrimSpace(override); v != "" {
		return v, SourceEnv, nil
	}
	if containerEnv != nil {
		if v := strings.TrimSpace(containerEnv[key]); v != "" {
			return v, SourceContainer, nil
		}
	}
	v, err := generate()
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve %s (env unset, container does not provide it, generation failed): %w", key, err)
	}
	return v, SourceGenerated, nil
}

// ParseContainerEnv parses the docker container-env format ("KEY=value")
// into a map. Values may contain '='; entries without '=' are skipped.
func ParseContainerEnv(entries []string) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			continue
		}
		m[k] = v
	}
	return m
}
