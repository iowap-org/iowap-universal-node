package tokenres

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeInspector records the InspectEnv calls.
type fakeInspector struct {
	entries []string
	exists  bool
	err     error
	calls   int
}

func (f *fakeInspector) InspectEnv(ctx context.Context, name string) ([]string, bool, error) {
	f.calls++
	return f.entries, f.exists, f.err
}

func genBridge() (string, error) { return "gen-bridge-token", nil }
func genBearer() (string, error) { return "gen-mcp-bearer", nil }

func testOpts(inspect *fakeInspector, genBridgeErr error) Options {
	return Options{
		GetEnv:        func(string) string { return "" },
		ContainerName: "iowap-universal-node",
		InspectEnv:    inspect.InspectEnv,
		GenerateBridgeToken: func() (string, error) {
			if genBridgeErr != nil {
				return "", genBridgeErr
			}
			return genBridge()
		},
		GenerateBearer: genBearer,
	}
}

// 1. Env override: explicit env wins, the container is never consulted.
func TestResolveEnvOverrideWins(t *testing.T) {
	insp := &fakeInspector{exists: true, entries: []string{
		"IOWAP_BRIDGE_TOKEN=from-container",
		"IOWAP_MCP_BEARER=from-container",
	}}
	opts := testOpts(insp, nil)
	opts.GetEnv = func(k string) string {
		if k == BridgeEnvKey {
			return "env-bridge"
		}
		if k == BearerEnvKey {
			return "env-bearer"
		}
		return ""
	}
	tok, err := Resolve(context.Background(), opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.BridgeToken != "env-bridge" || tok.BridgeSource != SourceEnv {
		t.Errorf("BridgeToken = %q/%v, want env-bridge/env", tok.BridgeToken, tok.BridgeSource)
	}
	if tok.MCPBearer != "env-bearer" || tok.BearerSource != SourceEnv {
		t.Errorf("MCPBearer = %q/%v, want env-bearer/env", tok.MCPBearer, tok.BearerSource)
	}
	if insp.calls != 0 {
		t.Errorf("container inspected %d times; a complete env override must never inspect", insp.calls)
	}
}

// 2. Existing container: tokens are reused from the container env — the
// container IS the secret store (daemon restart beside a running node).
func TestResolveReusesContainerEnv(t *testing.T) {
	insp := &fakeInspector{exists: true, entries: []string{
		"IOWAP_BRIDGE_TOKEN=container-bridge",
		"IOWAP_MCP_BEARER=container-bearer",
		"PATH=/usr/bin",
	}}
	tok, err := Resolve(context.Background(), testOpts(insp, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.BridgeToken != "container-bridge" || tok.BridgeSource != SourceContainer {
		t.Errorf("BridgeToken = %q/%v, want container-bridge/container", tok.BridgeToken, tok.BridgeSource)
	}
	if tok.MCPBearer != "container-bearer" || tok.BearerSource != SourceContainer {
		t.Errorf("MCPBearer = %q/%v, want container-bearer/container", tok.MCPBearer, tok.BearerSource)
	}
	if insp.calls != 1 {
		t.Errorf("InspectEnv calls = %d, want 1", insp.calls)
	}
}

// 3. Fresh install: no env, no container — the daemon generates both tokens.
func TestResolveGeneratesForFreshInstall(t *testing.T) {
	insp := &fakeInspector{exists: false}
	tok, err := Resolve(context.Background(), testOpts(insp, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.BridgeToken != "gen-bridge-token" || tok.BridgeSource != SourceGenerated {
		t.Errorf("BridgeToken = %q/%v, want gen-bridge-token/generated", tok.BridgeToken, tok.BridgeSource)
	}
	if tok.MCPBearer != "gen-mcp-bearer" || tok.BearerSource != SourceGenerated {
		t.Errorf("MCPBearer = %q/%v, want gen-mcp-bearer/generated", tok.MCPBearer, tok.BearerSource)
	}
}

// Mixed sources are resolved per token, independently.
func TestResolveMixedSources(t *testing.T) {
	insp := &fakeInspector{exists: false}
	opts := testOpts(insp, nil)
	opts.GetEnv = func(k string) string {
		if k == BridgeEnvKey {
			return "env-bridge"
		}
		return ""
	}
	tok, err := Resolve(context.Background(), opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.BridgeSource != SourceEnv || tok.BearerSource != SourceGenerated {
		t.Errorf("sources = %v/%v, want env/generated", tok.BridgeSource, tok.BearerSource)
	}
}

// A container whose env lacks a key falls through to the next stage for
// that token only (e.g. a container created by an older daemon version).
func TestResolveFallsThroughOnPartialContainerEnv(t *testing.T) {
	insp := &fakeInspector{exists: true, entries: []string{
		"IOWAP_BRIDGE_TOKEN=container-bridge",
	}}
	tok, err := Resolve(context.Background(), testOpts(insp, nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.BridgeToken != "container-bridge" || tok.BridgeSource != SourceContainer {
		t.Errorf("BridgeToken = %q/%v, want container-bridge/container", tok.BridgeToken, tok.BridgeSource)
	}
	if tok.MCPBearer != "gen-mcp-bearer" || tok.BearerSource != SourceGenerated {
		t.Errorf("MCPBearer = %q/%v, want gen-mcp-bearer/generated", tok.MCPBearer, tok.BearerSource)
	}
}

// Whitespace-only env values are not overrides (same semantics as the old
// config.Load TrimSpace behavior).
func TestResolveWhitespaceEnvIsNotAnOverride(t *testing.T) {
	insp := &fakeInspector{exists: true, entries: []string{
		"IOWAP_BRIDGE_TOKEN=container-bridge",
		"IOWAP_MCP_BEARER=container-bearer",
	}}
	opts := testOpts(insp, nil)
	opts.GetEnv = func(k string) string {
		if k == BridgeEnvKey {
			return "   "
		}
		return ""
	}
	tok, err := Resolve(context.Background(), opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.BridgeSource != SourceContainer {
		t.Errorf("BridgeSource = %v, want container (whitespace env must not count)", tok.BridgeSource)
	}
}

// Fail closed: an inspect error is an error, not a silent generate.
func TestResolveInspectErrorFailsClosed(t *testing.T) {
	insp := &fakeInspector{err: errors.New("engine exploded")}
	_, err := Resolve(context.Background(), testOpts(insp, nil))
	if err == nil {
		t.Fatal("expected an error when InspectEnv fails")
	}
}

// Fail closed: if generation fails (crypto/rand exhausted), Resolve errors
// and the caller must exit instead of starting tokenless.
func TestResolveGeneratorErrorFailsClosed(t *testing.T) {
	insp := &fakeInspector{exists: false}
	_, err := Resolve(context.Background(), testOpts(insp, errors.New("rand: exhausted")))
	if err == nil || !strings.Contains(err.Error(), BridgeEnvKey) {
		t.Fatalf("error %v must name %s", err, BridgeEnvKey)
	}
}

// ParseContainerEnv understands the docker Container-Env format
// ("KEY=value"), keeps values containing '=', and skips malformed entries.
func TestParseContainerEnv(t *testing.T) {
	m := ParseContainerEnv([]string{
		"IOWAP_BRIDGE_TOKEN=tok",
		"IOWAP_MCP_BEARER=a=b=c",
		"PATH=/usr/bin:/bin",
		"NOEQUALS",
		"",
		"EMPTY=",
	})
	if m["IOWAP_BRIDGE_TOKEN"] != "tok" {
		t.Errorf("IOWAP_BRIDGE_TOKEN = %q", m["IOWAP_BRIDGE_TOKEN"])
	}
	if m["IOWAP_MCP_BEARER"] != "a=b=c" {
		t.Errorf("values containing '=' must survive, got %q", m["IOWAP_MCP_BEARER"])
	}
	if m["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q", m["PATH"])
	}
	if _, ok := m["NOEQUALS"]; ok {
		t.Error("entry without '=' must be skipped")
	}
	v, ok := m["EMPTY"]
	if !ok || v != "" {
		t.Errorf("EMPTY = %q/%v, want empty value present", v, ok)
	}
}
