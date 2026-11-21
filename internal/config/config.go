// Package config loads the daemon configuration from the environment.
//
// Fail-closed: without IOWAP_BRIDGE_TOKEN the daemon refuses to start
// (SPEC §2 — every endpoint requires the Bearer token).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Default values (SPEC §4 / task T-006).
const (
	DefaultPort         = 7780
	DefaultWorkspaceDir = "./workspace"
	DefaultComfyUIURL   = "http://127.0.0.1:8188"
	// DefaultUIPort is the local onboarding web UI (board T-010).
	DefaultUIPort = 7782
	// DefaultMCPPort is the loopback port the universal-node compose
	// profile publishes the HTTP MCP endpoint on (SPEC §9).
	DefaultMCPPort = 7781
	// BindHost is fixed: the daemon never binds a foreign interface (SPEC §2).
	BindHost = "127.0.0.1"
)

// Config is the resolved daemon configuration.
type Config struct {
	Token        string
	Port         int
	WorkspaceDir string
	ComfyUIURL   string
	ComfyUICmd   string // optional command to (re)start ComfyUI on failed health probes
	UIPort       int    // onboarding web UI port (loopback only)
	MCPPort      int    // MCP endpoint port (display/snippets only; the compose profile owns the real mapping)
}

// Load reads the environment via the given getter (injected for tests).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Token:        strings.TrimSpace(getenv("IOWAP_BRIDGE_TOKEN")),
		Port:         DefaultPort,
		WorkspaceDir: DefaultWorkspaceDir,
		ComfyUIURL:   DefaultComfyUIURL,
		ComfyUICmd:   getenv("IOWAP_COMFYUI_CMD"),
		UIPort:       DefaultUIPort,
		MCPPort:      DefaultMCPPort,
	}
	if cfg.Token == "" {
		return cfg, fmt.Errorf("IOWAP_BRIDGE_TOKEN must be set to a non-empty value (the node container receives the same token as its bridge auth)")
	}
	if v := getenv("IOWAP_BRIDGE_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return cfg, fmt.Errorf("IOWAP_BRIDGE_PORT %q is not a valid port number", v)
		}
		cfg.Port = port
	}
	if v := getenv("IOWAP_UI_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return cfg, fmt.Errorf("IOWAP_UI_PORT %q is not a valid port number", v)
		}
		cfg.UIPort = port
	}
	if v := getenv("IOWAP_MCP_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return cfg, fmt.Errorf("IOWAP_MCP_PORT %q is not a valid port number", v)
		}
		cfg.MCPPort = port
	}
	if v := getenv("IOWAP_WORKSPACE_DIR"); v != "" {
		cfg.WorkspaceDir = v
	}
	if v := getenv("IOWAP_COMFYUI_URL"); v != "" {
		cfg.ComfyUIURL = strings.TrimRight(v, "/")
	}
	return cfg, nil
}

// BindAddr returns the loopback-only listen address.
func (c Config) BindAddr() string {
	return BindHost + ":" + strconv.Itoa(c.Port)
}

// LoadFromEnv is the production entry point.
func LoadFromEnv() (Config, error) {
	return Load(os.Getenv)
}
