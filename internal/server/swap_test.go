package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// --- Hot-swap (workspace provider watch, board T-022) -----------------------

type swapProbe struct {
	stubProvider
	ctxDone chan struct{}
}

func newSwapProbe(name string) *swapProbe {
	return &swapProbe{
		stubProvider: stubProvider{
			cap: provider.Capability{
				Name:   name,
				Health: provider.HealthOK,
			},
			execFn: func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
				return map[string]any{"ok": true}, nil, nil
			},
		},
		ctxDone: make(chan struct{}),
	}
}

func (s *swapProbe) ProbeOnce(ctx context.Context) string { return provider.HealthOK }
func (s *swapProbe) StartHealthLoop(ctx context.Context, interval time.Duration) {
	go func() {
		<-ctx.Done()
		close(s.ctxDone)
	}()
}
func (s *swapProbe) Health() string { return provider.HealthOK }

func manifestCapabilityNames(t *testing.T, s *Server) []string {
	t.Helper()
	rec := doReq(t, s, http.MethodGet, "/v1/manifest", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var manifest struct {
		Capabilities []struct {
			Name string `json:"name"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decoding manifest: %v", err)
	}
	names := make([]string, 0, len(manifest.Capabilities))
	for _, c := range manifest.Capabilities {
		names = append(names, c.Name)
	}
	return names
}

func TestReplaceProvidersSwapsManifestAndExecRouting(t *testing.T) {
	oldP := newSwapProbe("old.cap")
	newP := newSwapProbe("new.cap")
	s := New(testToken, t.TempDir(), oldP)

	if got := strings.Join(manifestCapabilityNames(t, s), ","); got != "old.cap" {
		t.Fatalf("initial manifest = %q, want old.cap", got)
	}

	s.ReplaceProviders(context.Background(), time.Hour, newP)

	if got := strings.Join(manifestCapabilityNames(t, s), ","); got != "new.cap" {
		t.Fatalf("manifest after replace = %q, want new.cap", got)
	}
	// Exec routing must follow: old capability gone, new one served.
	rec := doReq(t, s, http.MethodPost, "/v1/exec/old.cap", testToken, `{"task_id":"t","params":{}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("exec old.cap after replace = %d, want 404", rec.Code)
	}
	rec = doReq(t, s, http.MethodPost, "/v1/exec/new.cap", testToken, `{"task_id":"t","params":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec new.cap after replace = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}

func TestReplaceProvidersStopsOldHealthLoops(t *testing.T) {
	oldP := newSwapProbe("old.cap")
	newP := newSwapProbe("new.cap")
	s := New(testToken, t.TempDir())
	gen1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	s.ReplaceProviders(gen1, time.Hour, oldP)

	s.ReplaceProviders(context.Background(), time.Hour, newP)

	select {
	case <-oldP.ctxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("replaced provider's health loop was not stopped (context still alive)")
	}
}

func TestReplaceProvidersStartsNewHealthLoops(t *testing.T) {
	newP := newSwapProbe("new.cap")
	s := New(testToken, t.TempDir())
	s.ReplaceProviders(context.Background(), time.Hour, newP)
	// The new generation's loop runs until ITS context is canceled by the
	// next swap — prove it stays alive while current.
	select {
	case <-newP.ctxDone:
		t.Fatal("current provider's health loop was canceled without a swap")
	case <-time.After(100 * time.Millisecond):
	}
}
