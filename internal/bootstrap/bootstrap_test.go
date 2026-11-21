package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeEngine is the mocked docker engine client.
type fakeEngine struct {
	pingErr    error
	running    bool
	runningErr error
}

func (f *fakeEngine) Ping(ctx context.Context) error { return f.pingErr }
func (f *fakeEngine) ContainerRunning(ctx context.Context, name string) (bool, error) {
	return f.running, f.runningErr
}

// recordingRunner captures the compose invocation.
type recordingRunner struct {
	called bool
	yaml   []byte
	env    map[string]string
	err    error
}

func (r *recordingRunner) run(ctx context.Context, yaml []byte, env map[string]string) error {
	r.called = true
	r.yaml = yaml
	r.env = env
	return r.err
}

func genBearer() (string, error) { return "iw_mcp_fixed_test_bearer", nil }

func opts(runner *recordingRunner) Options {
	return Options{
		BridgeToken:    "bridge-token",
		WorkspaceDir:   "/ws",
		GenerateBearer: genBearer,
		RunCompose:     runner.run,
	}
}

func TestRunEngineDownReturnsActionableError(t *testing.T) {
	eng := &fakeEngine{pingErr: errors.New("dial pipe: access denied")}
	res, err := Run(context.Background(), eng, opts(&recordingRunner{}))
	if err == nil {
		t.Fatal("expected an error when the engine is unreachable")
	}
	if !strings.Contains(err.Error(), "Docker Desktop") {
		t.Fatalf("error must point at Docker Desktop, got: %v", err)
	}
	if res.Bearer != "" {
		t.Fatal("no bearer must be generated when the engine is down")
	}
}

func TestRunContainerRunningSkipsBootstrap(t *testing.T) {
	runner := &recordingRunner{}
	eng := &fakeEngine{running: true}
	res, err := Run(context.Background(), eng, opts(runner))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Skipped {
		t.Fatal("expected Skipped=true when the container is running")
	}
	if res.Bearer != "" {
		t.Fatal("skip must not generate a bearer")
	}
	if runner.called {
		t.Fatal("skip must not run the compose profile")
	}
}

func TestRunCreatesContainerWithVendoredProfileAndEnv(t *testing.T) {
	runner := &recordingRunner{}
	eng := &fakeEngine{running: false}
	res, err := Run(context.Background(), eng, opts(runner))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Skipped || res.Bearer != "iw_mcp_fixed_test_bearer" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !runner.called {
		t.Fatal("compose must have been executed")
	}
	if !strings.Contains(string(runner.yaml), "iowap-universal-node") {
		t.Fatal("compose must receive the vendored universal-node profile")
	}
	want := map[string]string{
		"IOWAP_BRIDGE_TOKEN":  "bridge-token",
		"IOWAP_MCP_BEARER":    "iw_mcp_fixed_test_bearer",
		"IOWAP_WORKSPACE_DIR": "/ws",
	}
	for k, v := range want {
		if runner.env[k] != v {
			t.Fatalf("compose env[%s] = %q, want %q", k, runner.env[k], v)
		}
	}
}

func TestRunComposeFailureSurfacesError(t *testing.T) {
	runner := &recordingRunner{err: fmt.Errorf("exit status 1 — pull failed")}
	_, err := Run(context.Background(), &fakeEngine{}, opts(runner))
	if err == nil || !strings.Contains(err.Error(), "compose") {
		t.Fatalf("expected the compose error to surface, got: %v", err)
	}
}

func TestRunChecksContainerError(t *testing.T) {
	runner := &recordingRunner{}
	eng := &fakeEngine{runningErr: errors.New("inspect failed")}
	_, err := Run(context.Background(), eng, opts(runner))
	if err == nil {
		t.Fatal("expected an error when the running-check fails")
	}
	if runner.called {
		t.Fatal("must not run compose when the container state is unknown")
	}
}

func TestRunRequiresDeps(t *testing.T) {
	if _, err := Run(context.Background(), &fakeEngine{}, Options{RunCompose: func(context.Context, []byte, map[string]string) error { return nil }}); err == nil {
		t.Fatal("missing bearer generator must fail")
	}
	if _, err := Run(context.Background(), &fakeEngine{}, Options{GenerateBearer: genBearer}); err == nil {
		t.Fatal("missing compose runner must fail")
	}
	if _, err := Run(context.Background(), nil, opts(&recordingRunner{})); err == nil {
		t.Fatal("nil engine must fail")
	}
}

func TestVendoredComposeFileIsTheSourceOfTruth(t *testing.T) {
	// DEC #7: the daemon never invents its own container config — the
	// embedded file must be the (provenance-marked) iowap-docker profile.
	s := string(composeFile)
	if !strings.HasPrefix(s, "# Vendored from iowap-docker@") {
		t.Fatal("vendored file must carry a provenance comment")
	}
	for _, marker := range []string{
		"container_name: iowap-universal-node",
		`${IOWAP_BRIDGE_TOKEN:-}`,
		`${IOWAP_MCP_BEARER:-}`,
		`${IOWAP_WORKSPACE_DIR:-`,
		"127.0.0.1:7781:7781",
	} {
		if !strings.Contains(s, marker) {
			t.Fatalf("vendored profile lost marker %q", marker)
		}
	}
}
