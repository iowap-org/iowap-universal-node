// Package config loads the daemon configuration from the environment.
//
// Tokens are NOT config's concern (v0.1.1): IOWAP_BRIDGE_TOKEN and
// IOWAP_MCP_BEARER are resolved by internal/tokenres (env override →
// container inspect → generate). The bridge server itself stays fail-closed
// (SPEC §2 — every endpoint requires the Bearer token).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Default values (SPEC §4 / task T-006).
const (
	DefaultPort         = 7780
	DefaultWorkspaceDir = "./workspace"
	// ProvidersFileName is the providers registry looked up in the config
	// dir when IOWAP_PROVIDERS_FILE is unset (board T-019).
	ProvidersFileName = "providers.yaml"
	// BindHost is fixed: the daemon never binds a foreign interface (SPEC §2).
	BindHost = "127.0.0.1"
)

// DefaultProvidersFile returns the default providers registry path:
// <configdir>/iowap-bridge/providers.yaml (Windows: %AppData%,
// Linux: ~/.config). Falls back to the current directory when no user
// config dir exists.
func DefaultProvidersFile() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ProvidersFileName
	}
	return filepath.Join(dir, "iowap-bridge", ProvidersFileName)
}

// Config is the resolved daemon configuration.
type Config struct {
	Port          int
	WorkspaceDir  string
	ProvidersFile string // provider registry (board T-019)
}

// Load reads the environment via the given getter (injected for tests).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:          DefaultPort,
		WorkspaceDir:  DefaultWorkspaceDir,
		ProvidersFile: DefaultProvidersFile(),
	}
	if v := getenv("IOWAP_BRIDGE_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil || port <= 0 || port > 65535 {
			return cfg, fmt.Errorf("IOWAP_BRIDGE_PORT %q is not a valid port number", v)
		}
		cfg.Port = port
	}
	if v := getenv("IOWAP_WORKSPACE_DIR"); v != "" {
		cfg.WorkspaceDir = v
	}
	if v := getenv("IOWAP_PROVIDERS_FILE"); v != "" {
		cfg.ProvidersFile = v
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
