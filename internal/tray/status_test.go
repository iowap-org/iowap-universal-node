package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Node registration state (pragmatic: dashboard probe) --------------------

func TestRegistrationState(t *testing.T) {
	cases := []struct {
		name string
		body string
		want State
	}{
		{"blank form", "<h1>IOWAP node setup</h1><form method=\"post\" action=\"/configure\">", StateUnregistered},
		{"configured status page", "<h1>IOWAP node setup</h1><dl><dt>Node</dt></dl>", StateRegistered},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(tc.body))
		}))
		got := RegistrationState(context.Background(), srv.Client(), srv.URL)
		if got != tc.want {
			t.Errorf("%s: state = %v, want %v", tc.name, got, tc.want)
		}
		srv.Close()
	}
}

func TestRegistrationStateUnknownWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	state := RegistrationState(context.Background(), srv.Client(), srv.URL)
	if state != StateUnknown {
		t.Fatalf("state = %v, want StateUnknown", state)
	}

	// Connection refused → unknown, never a crash.
	state = RegistrationState(context.Background(), srv.Client(), "http://127.0.0.1:1")
	if state != StateUnknown {
		t.Fatalf("state = %v, want StateUnknown", state)
	}
}

// --- Status widget assembly ---------------------------------------------------

type stubStatusController struct{ running bool }

func (s stubStatusController) Running(ctx context.Context, name string) (bool, error) {
	return s.running, nil
}
func (s stubStatusController) Stop(ctx context.Context, name string) error  { return nil }
func (s stubStatusController) Start(ctx context.Context, name string) error { return nil }

func TestStatusLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<dl><dt>Node</dt></dl>")) // registered
	}))
	defer srv.Close()

	ctrl := stubStatusController{running: true}
	lines := StatusLines(context.Background(), ctrl, ContainerName, srv.URL, "0.3.0", "sha256:abc")

	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{
		"Container: running",
		"Node: registered",
		"Daemon: 0.3.0",
		"Image: sha256:abc",
	} {
		if !contains(joined, want) {
			t.Errorf("status lines missing %q — got:\n%s", want, joined)
		}
	}
}

func TestStatusLinesStoppedAndUnregistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<form method=\"post\" action=\"/configure\">"))
	}))
	defer srv.Close()

	ctrl := stubStatusController{running: false}
	lines := StatusLines(context.Background(), ctrl, "node", srv.URL, "0.3.0", "")

	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	if !contains(joined, "Container: stopped") {
		t.Errorf("missing stopped state:\n%s", joined)
	}
	if !contains(joined, "Node: not registered") {
		t.Errorf("missing unregistered state:\n%s", joined)
	}
	if contains(joined, "Image: ") {
		t.Errorf("no image digest → no image line:\n%s", joined)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
