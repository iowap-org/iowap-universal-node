package config

import (
	"net"
	"strings"
	"testing"
)

func load(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	return Load(func(key string) string { return env[key] })
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(t, map[string]string{"IOWAP_BRIDGE_TOKEN": "secret"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Token != "secret" {
		t.Errorf("Token = %q", cfg.Token)
	}
	if cfg.Port != 7780 {
		t.Errorf("Port = %d, want 7780", cfg.Port)
	}
	if cfg.WorkspaceDir != "./workspace" {
		t.Errorf("WorkspaceDir = %q, want ./workspace", cfg.WorkspaceDir)
	}
	if cfg.ComfyUIURL != "http://127.0.0.1:8188" {
		t.Errorf("ComfyUIURL = %q", cfg.ComfyUIURL)
	}
	if cfg.ComfyUICmd != "" {
		t.Errorf("ComfyUICmd = %q, want empty", cfg.ComfyUICmd)
	}
	if got := cfg.BindAddr(); got != "127.0.0.1:7780" {
		t.Errorf("BindAddr = %q, want 127.0.0.1:7780", got)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"IOWAP_BRIDGE_TOKEN":  "t",
		"IOWAP_BRIDGE_PORT":   "7781",
		"IOWAP_WORKSPACE_DIR": "/data/ws",
		"IOWAP_COMFYUI_URL":   "http://127.0.0.1:9999",
		"IOWAP_COMFYUI_CMD":   "C:/comfy/run.bat",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 7781 || cfg.WorkspaceDir != "/data/ws" ||
		cfg.ComfyUIURL != "http://127.0.0.1:9999" || cfg.ComfyUICmd != "C:/comfy/run.bat" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if got := cfg.BindAddr(); got != "127.0.0.1:7781" {
		t.Errorf("BindAddr = %q", got)
	}
}

// Fail closed: without a token the daemon must refuse to start.
func TestLoadFailClosedWithoutToken(t *testing.T) {
	for _, env := range []map[string]string{{}, {"IOWAP_BRIDGE_TOKEN": ""}, {"IOWAP_BRIDGE_TOKEN": "   "}} {
		_, err := load(t, env)
		if err == nil {
			t.Fatalf("Load(%v) = nil error, want fail-closed", env)
		}
		if !strings.Contains(err.Error(), "IOWAP_BRIDGE_TOKEN") {
			t.Errorf("error %q does not name IOWAP_BRIDGE_TOKEN", err)
		}
	}
}

func TestLoadInvalidPort(t *testing.T) {
	_, err := load(t, map[string]string{"IOWAP_BRIDGE_TOKEN": "t", "IOWAP_BRIDGE_PORT": "http"})
	if err == nil {
		t.Fatal("Load with non-numeric port = nil error, want error")
	}
	if !strings.Contains(err.Error(), "IOWAP_BRIDGE_PORT") {
		t.Errorf("error %q does not name IOWAP_BRIDGE_PORT", err)
	}
}

// SPEC §2: the daemon binds loopback only, never a foreign interface.
func TestBindAddrIsLoopback(t *testing.T) {
	cfg, err := load(t, map[string]string{"IOWAP_BRIDGE_TOKEN": "t", "IOWAP_BRIDGE_PORT": "7780"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	host, port, err := net.SplitHostPort(cfg.BindAddr())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", cfg.BindAddr(), err)
	}
	if port != "7780" {
		t.Errorf("port = %q", port)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("bind host %q is not a loopback address", host)
	}
}
