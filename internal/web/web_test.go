package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/nodecli"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provision"
)

// fakeEngine is the mocked container-status source.
type fakeEngine struct {
	running bool
	err     error
}

func (f *fakeEngine) ContainerRunning(ctx context.Context, name string) (bool, error) {
	return f.running, f.err
}

// fakeExecer is the mocked docker exec — it records argv and answers per
// command shape.
type fakeExecer struct {
	calls [][]string
	// answer decides the reply for the most recent call.
	stdout   string
	stderr   string
	exitCode int
	err      error
}

func (f *fakeExecer) Exec(ctx context.Context, container string, cmd []string, timeout time.Duration) (nodecli.Result, error) {
	f.calls = append(f.calls, cmd)
	if f.err != nil {
		return nodecli.Result{}, f.err
	}
	return nodecli.Result{Stdout: f.stdout, Stderr: f.stderr, ExitCode: f.exitCode}, nil
}

func (f *fakeExecer) lastCall() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func newUI(eng EngineStatus, execer ContainerExecer, view *provision.OnceView) *UI {
	return New(eng, execer, view, 7781)
}

func getIndex(t *testing.T, u *UI) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	u.Handler().ServeHTTP(rec, req)
	return rec
}

func postRegister(t *testing.T, u *UI, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	u.Handler().ServeHTTP(rec, req)
	return rec
}

func TestGetShowsFormAndContainerStatus(t *testing.T) {
	eng := &fakeEngine{running: true}
	ui := newUI(eng, &fakeExecer{}, nil)

	rec := getIndex(t, ui)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{"running", `name="name"`, `name="server"`, "Register"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q\n%s", want, body)
		}
	}
}

func TestGetPrefillsServerFromDiscovery(t *testing.T) {
	eng := &fakeEngine{running: true}
	execer := &fakeExecer{stdout: "http://192.0.2.60:8788\n  service: IOWAP Relay Service (_http._tcp)\n", exitCode: 0}
	ui := newUI(eng, execer, nil)

	body := getIndex(t, ui).Body.String()
	if !strings.Contains(body, `value="http://192.0.2.60:8788"`) {
		t.Fatalf("server field not pre-filled with the discovered URL:\n%s", body)
	}
	// The prefill must come from the container CLI, exact argv.
	want := nodecli.DiscoverArgv()
	if !reflect.DeepEqual(execer.lastCall(), want) {
		t.Fatalf("discover argv = %v, want %v", execer.lastCall(), want)
	}
}

func TestGetLeavesServerEmptyWhenDiscoveryFindsNothing(t *testing.T) {
	eng := &fakeEngine{running: true}
	execer := &fakeExecer{stdout: "relay not found: no mDNS service named 'IOWAP Relay Service' (watched 2s)\n", exitCode: 1}
	ui := newUI(eng, execer, nil)

	body := getIndex(t, ui).Body.String()
	if strings.Contains(body, "relay not found") {
		t.Fatal("diagnostic must not leak into the form field")
	}
	if !strings.Contains(body, `value=""`) {
		t.Fatalf("server field must be empty for manual entry:\n%s", body)
	}
}

func TestGetLeavesServerEmptyWhenDiscoveryErrors(t *testing.T) {
	eng := &fakeEngine{running: true}
	execer := &fakeExecer{err: errors.New("exec: container not running")}
	ui := newUI(eng, execer, nil)

	rec := getIndex(t, ui)
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery failure must not break the page, status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "container not running") {
		t.Fatal("discovery transport error must not surface on the page")
	}
}

func TestPostRegisterRunsRegisterExecAndShowsSuccess(t *testing.T) {
	eng := &fakeEngine{running: true}
	execer := &fakeExecer{stdout: `{"ok": true, "node_id": "nd_123", "node_name": "bench-pc"}`, exitCode: 0}
	ui := newUI(eng, execer, nil)

	rec := postRegister(t, ui, url.Values{"name": {"bench-pc"}, "server": {"192.0.2.60:8788"}})
	body := rec.Body.String()

	wantArgv := nodecli.RegisterArgv("192.0.2.60:8788", "bench-pc")
	if !reflect.DeepEqual(execer.lastCall(), wantArgv) {
		t.Fatalf("register argv = %v, want %v", execer.lastCall(), wantArgv)
	}
	if !strings.Contains(body, "Node registriert — Heartbeat läuft") {
		t.Fatalf("success message missing:\n%s", body)
	}
	if strings.Contains(body, `placeholder="e.g. 192.0.2.10:8788" value=""`) && !strings.Contains(body, `value="192.0.2.60:8788"`) {
		t.Fatal("the submitted server value should be preserved")
	}
}

func TestPostRegisterShowsFailure(t *testing.T) {
	eng := &fakeEngine{running: true}
	execer := &fakeExecer{stderr: "registration failed: HTTP 401: bad relay token", exitCode: 1}
	ui := newUI(eng, execer, nil)

	rec := postRegister(t, ui, url.Values{"name": {"bench-pc"}, "server": {"192.0.2.60:8788"}})
	body := rec.Body.String()
	if !strings.Contains(body, "Registration failed") || !strings.Contains(body, "HTTP 401") {
		t.Fatalf("failure message missing:\n%s", body)
	}
	if strings.Contains(body, "Node registriert") {
		t.Fatal("failed register must not show success")
	}
}

func TestPostRegisterValidatesInput(t *testing.T) {
	ui := newUI(&fakeEngine{running: true}, &fakeExecer{}, nil)

	rec := postRegister(t, ui, url.Values{"name": {""}, "server": {""}})
	if strings.Contains(rec.Body.String(), "Node registriert") {
		t.Fatal("empty form must not register")
	}
	if !strings.Contains(rec.Body.String(), "Both fields are required") {
		t.Fatalf("validation message missing:\n%s", rec.Body.String())
	}
}

func TestPostRegisterExecTransportError(t *testing.T) {
	eng := &fakeEngine{running: true}
	execer := &fakeExecer{err: errors.New("no such container")}
	ui := newUI(eng, execer, nil)

	rec := postRegister(t, ui, url.Values{"name": {"bench-pc"}, "server": {"10.0.0.9:8788"}})
	if !strings.Contains(rec.Body.String(), "register command failed") {
		t.Fatalf("transport error must surface:\n%s", rec.Body.String())
	}
}

func TestBearerShownExactlyOnceThenHashOnly(t *testing.T) {
	eng := &fakeEngine{running: true}
	view := provision.NewOnceView("iw_mcp_secret_bearer_abc")
	ui := newUI(eng, &fakeExecer{}, view)

	first := getIndex(t, ui).Body.String()
	if !strings.Contains(first, "iw_mcp_secret_bearer_abc") {
		t.Fatal("first page must show the plaintext bearer")
	}
	for _, want := range []string{"hermes mcp add iowap", "mcpServers", "[mcp_servers.iowap]", "curl -H"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first page lacks snippet marker %q", want)
		}
	}

	second := getIndex(t, ui).Body.String()
	if strings.Contains(second, "iw_mcp_secret_bearer_abc") {
		t.Fatal("the bearer must never appear again after the one-time display")
	}
	if !strings.Contains(second, provision.HashBearer("iw_mcp_secret_bearer_abc")) {
		t.Fatal("second page must show the sha256 hash instead")
	}
	if !strings.Contains(second, "rotation = container recreate") {
		t.Fatal("second page must carry the rotation note")
	}
}

func TestNoBearerWhenBootstrapSkipped(t *testing.T) {
	// Daemon found the container already running — no bearer in this run.
	ui := newUI(&fakeEngine{running: true}, &fakeExecer{}, nil)

	body := getIndex(t, ui).Body.String()
	if strings.Contains(body, "iw_mcp_") || strings.Contains(body, "sha256") {
		t.Fatalf("page must not pretend there is a bearer:\n%s", body)
	}
	if !strings.Contains(body, "already running") {
		t.Fatal("page should explain why no bearer is shown")
	}
}

func TestContainerStatusErrorShownAsUnknown(t *testing.T) {
	eng := &fakeEngine{err: errors.New("pipe unreachable")}
	ui := newUI(eng, &fakeExecer{}, nil)

	body := getIndex(t, ui).Body.String()
	if !strings.Contains(body, "unknown") {
		t.Fatalf("status error must render as unknown:\n%s", body)
	}
}
