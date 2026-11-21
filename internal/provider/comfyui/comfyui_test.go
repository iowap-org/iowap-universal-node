package comfyui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// fakeComfy simulates the ComfyUI HTTP API: /prompt, /history/{id}, /view.
type fakeComfy struct {
	mux         *http.ServeMux
	server      *httptest.Server
	promptSeen  atomic.Value // last /prompt body
	promptResp  atomic.Value // status code + body
	historySeen atomic.Int32 // number of history polls
	historyResp atomic.Value // func() (int, string)
	viewResp    atomic.Value // (int, []byte)
	rootHealthy atomic.Bool  // when false, GET / returns 503 (start-on-demand test)
}

func newFakeComfy(t *testing.T) *fakeComfy {
	t.Helper()
	f := &fakeComfy{mux: http.NewServeMux()}
	f.rootHealthy.Store(true)
	f.promptResp.Store([2]any{http.StatusOK, `{"prompt_id": "fake-prompt-id", "number": 1}`})
	f.historyResp.Store(func() (int, string) {
		hist := map[string]any{}
		b, _ := json.Marshal(hist)
		return http.StatusOK, string(b)
	})
	f.viewResp.Store([2]any{http.StatusOK, []byte("PNGDATA")})

	f.mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		f.promptSeen.Store(string(buf))
		tup := f.promptResp.Load().([2]any)
		w.WriteHeader(tup[0].(int))
		_, _ = w.Write([]byte(tup[1].(string)))
	})
	f.mux.HandleFunc("GET /history/", func(w http.ResponseWriter, r *http.Request) {
		f.historySeen.Add(1)
		fn := f.historyResp.Load().(func() (int, string))
		status, body := fn()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	f.mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		tup := f.viewResp.Load().([2]any)
		w.WriteHeader(tup[0].(int))
		_, _ = w.Write(tup[1].([]byte))
	})
	f.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if !f.rootHealthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f.server = httptest.NewServer(f.mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeComfy) setCompleted(promptID string) {
	hist := map[string]any{
		promptID: map[string]any{
			"status": map[string]any{"status_str": "success", "completed": true},
			"outputs": map[string]any{
				"9": map[string]any{
					"images": []map[string]any{
						{"filename": "ComfyUI_00001_.png", "subfolder": "", "type": "output"},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(hist)
	f.historyResp.Store(func() (int, string) { return http.StatusOK, string(b) })
}

func newTestProvider(t *testing.T, url string) *ComfyUI {
	t.Helper()
	c := New(url, "")
	c.pollInterval = 5 * time.Millisecond
	return c
}

const workflowParams = `{"workflow":{"3":{"class_type":"KSampler"}}}`

func TestExecHappyPath(t *testing.T) {
	f := newFakeComfy(t)
	f.setCompleted("fake-prompt-id")
	c := newTestProvider(t, f.server.URL)

	ws := t.TempDir()
	result, artifacts, execErr := c.Exec(context.Background(), "task-1", json.RawMessage(workflowParams), 10*time.Second, ws)
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	if result["prompt_id"] != "fake-prompt-id" {
		t.Errorf("result prompt_id = %v", result["prompt_id"])
	}
	images, ok := result["images"].([]map[string]any)
	if !ok || len(images) != 1 {
		t.Fatalf("result images = %#v", result["images"])
	}
	if images[0]["name"] != "ComfyUI_00001_.png" {
		t.Errorf("image name = %v", images[0]["name"])
	}
	wantRel := filepath.Join("task-1", "ComfyUI_00001_.png")
	if images[0]["path"] != wantRel {
		t.Errorf("image path = %v, want %v", images[0]["path"], wantRel)
	}
	if len(artifacts) != 1 || artifacts[0].Path != wantRel || artifacts[0].Name != "ComfyUI_00001_.png" {
		t.Errorf("artifacts = %+v", artifacts)
	}
	// The image was written into the workspace.
	data, err := os.ReadFile(filepath.Join(ws, wantRel))
	if err != nil {
		t.Fatalf("artifact not written to workspace: %v", err)
	}
	if string(data) != "PNGDATA" {
		t.Errorf("artifact content = %q", data)
	}
	// The workflow was forwarded as the ComfyUI /prompt payload.
	seen := f.promptSeen.Load().(string)
	var sent map[string]any
	if err := json.Unmarshal([]byte(seen), &sent); err != nil {
		t.Fatalf("/prompt body not JSON: %v (%q)", err, seen)
	}
	if _, ok := sent["prompt"]; !ok {
		t.Errorf("/prompt body %q has no prompt field", seen)
	}
	// History was polled at least once before completion.
	if f.historySeen.Load() < 1 {
		t.Error("history was never polled")
	}
}

func TestExecWaitsForHistoryCompletion(t *testing.T) {
	f := newFakeComfy(t)
	c := newTestProvider(t, f.server.URL)
	// Completion appears only after the 2nd poll.
	var polls atomic.Int32
	base := f.historyResp.Load().(func() (int, string))
	f.historyResp.Store(func() (int, string) {
		if polls.Add(1) < 2 {
			return base()
		}
		f.setCompleted("fake-prompt-id")
		return base()
	})
	ws := t.TempDir()
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), 10*time.Second, ws)
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	if f.historySeen.Load() < 2 {
		t.Errorf("history polls = %d, want >= 2", f.historySeen.Load())
	}
}

func TestExecMissingWorkflowParam(t *testing.T) {
	f := newFakeComfy(t)
	c := newTestProvider(t, f.server.URL)
	for _, params := range []string{`{}`, `{"prompt":{}}`, `{"workflow":"nope"}`} {
		_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(params), time.Second, t.TempDir())
		if execErr == nil || execErr.Code != provider.CodeInvalidParams || execErr.Retryable {
			t.Errorf("params %s: execErr = %+v, want terminal invalid_params", params, execErr)
		}
	}
}

func TestExecPromptValidationError(t *testing.T) {
	f := newFakeComfy(t)
	f.promptResp.Store([2]any{http.StatusBadRequest, `{"error": {"type": "prompt_outputs_failed_validation"}, "node_errors": {}}`})
	c := newTestProvider(t, f.server.URL)
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), time.Second, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeInvalidParams || execErr.Retryable {
		t.Errorf("execErr = %+v, want terminal invalid_params", execErr)
	}
}

func TestExecPromptServerError(t *testing.T) {
	f := newFakeComfy(t)
	f.promptResp.Store([2]any{http.StatusInternalServerError, `boom`})
	c := newTestProvider(t, f.server.URL)
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), time.Second, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeCapabilityFailed || execErr.Retryable {
		t.Errorf("execErr = %+v, want terminal capability.failed", execErr)
	}
}

func TestExecHistoryError(t *testing.T) {
	f := newFakeComfy(t)
	hist := map[string]any{
		"fake-prompt-id": map[string]any{
			"status": map[string]any{"status_str": "error", "completed": true},
		},
	}
	b, _ := json.Marshal(hist)
	f.historyResp.Store(func() (int, string) { return http.StatusOK, string(b) })
	c := newTestProvider(t, f.server.URL)
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), time.Second, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeCapabilityFailed || execErr.Retryable {
		t.Errorf("execErr = %+v, want terminal capability.failed", execErr)
	}
}

func TestExecUnreachable(t *testing.T) {
	// Port 1 on loopback: nothing listens there.
	c := newTestProvider(t, "http://127.0.0.1:1")
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), 5*time.Second, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeProviderUnavail || !execErr.Retryable {
		t.Errorf("execErr = %+v, want retryable provider.unavailable", execErr)
	}
}

func TestExecTimeout(t *testing.T) {
	f := newFakeComfy(t)
	// History never completes; the request timeout must surface as `timeout`.
	c := newTestProvider(t, f.server.URL)
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), 100*time.Millisecond, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeTimeout || !execErr.Retryable {
		t.Errorf("execErr = %+v, want retryable timeout", execErr)
	}
}

func TestExecViewFailure(t *testing.T) {
	f := newFakeComfy(t)
	f.setCompleted("fake-prompt-id")
	f.viewResp.Store([2]any{http.StatusInternalServerError, []byte("disk error")})
	c := newTestProvider(t, f.server.URL)
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), time.Second, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeCapabilityFailed || execErr.Retryable {
		t.Errorf("execErr = %+v, want terminal capability.failed", execErr)
	}
}

// A malicious/buggy ComfyUI must not make us write outside the workspace.
func TestExecRejectsTraversalOutputName(t *testing.T) {
	f := newFakeComfy(t)
	hist := map[string]any{
		"fake-prompt-id": map[string]any{
			"status":  map[string]any{"status_str": "success", "completed": true},
			"outputs": map[string]any{"9": map[string]any{"images": []map[string]any{{"filename": "../../evil.png", "subfolder": "", "type": "output"}}}},
		},
	}
	b, _ := json.Marshal(hist)
	f.historyResp.Store(func() (int, string) { return http.StatusOK, string(b) })
	c := newTestProvider(t, f.server.URL)
	_, _, execErr := c.Exec(context.Background(), "t", json.RawMessage(workflowParams), time.Second, t.TempDir())
	if execErr == nil || execErr.Code != provider.CodeCapabilityFailed || execErr.Retryable {
		t.Errorf("execErr = %+v, want terminal capability.failed", execErr)
	}
}

// --- Health ------------------------------------------------------------------

func TestProbeOK(t *testing.T) {
	f := newFakeComfy(t)
	c := newTestProvider(t, f.server.URL)
	if got := c.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("ProbeOnce = %q, want ok", got)
	}
	if c.Health() != provider.HealthOK {
		t.Errorf("Health = %q, want ok", c.Health())
	}
}

func TestHealthDownAfterThreeFailures(t *testing.T) {
	url := "http://127.0.0.1:1" // unreachable
	c := newTestProvider(t, url)
	for i := 1; i <= 2; i++ {
		if got := c.ProbeOnce(context.Background()); got != provider.HealthOK {
			t.Fatalf("probe %d = %q, want ok (below failure threshold)", i, got)
		}
		if c.Health() != provider.HealthOK {
			t.Errorf("after %d failures Health = %q, want ok", i, c.Health())
		}
	}
	if got := c.ProbeOnce(context.Background()); got != provider.HealthDown {
		t.Errorf("probe 3 = %q, want down (failure threshold reached)", got)
	}
	if c.Health() != provider.HealthDown {
		t.Errorf("after 3 failures Health = %q, want down", c.Health())
	}
}

func TestHealthRecovers(t *testing.T) {
	c := newTestProvider(t, "http://127.0.0.1:1")
	for i := 0; i < 3; i++ {
		_ = c.ProbeOnce(context.Background())
	}
	if c.Health() != provider.HealthDown {
		t.Fatal("expected down before recovery")
	}
	f := newFakeComfy(t)
	c.baseURL = f.server.URL
	if got := c.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("ProbeOnce after recovery = %q, want ok", got)
	}
	if c.Health() != provider.HealthOK {
		t.Errorf("Health after recovery = %q, want ok", c.Health())
	}
}

// Start-on-demand: when a start command is configured and the probe
// fails, the provider launches it and waits for health to come back.
func TestProbeStartsProcessOnDemand(t *testing.T) {
	f := newFakeComfy(t)
	// The fake is "down" until the starter runs.
	var healthy atomic.Bool
	f.rootHealthy.Store(false)

	started := make(chan struct{}, 1)
	c := New(f.server.URL, "fake-comfy --listen")
	c.pollInterval = 5 * time.Millisecond
	c.startFn = func(ctx context.Context) error {
		started <- struct{}{}
		healthy.Store(true)
		f.rootHealthy.Store(true)
		return nil
	}

	// First probe: fails, starts the process, waits for health — the
	// resulting health is ok without needing a second probe.
	if got := c.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("probe after auto-start = %q, want ok", got)
	}
	select {
	case <-started:
	default:
		t.Error("starter was never invoked")
	}
	if c.Health() != provider.HealthOK {
		t.Errorf("Health = %q, want ok", c.Health())
	}
}

func TestCapabilityManifest(t *testing.T) {
	c := New("http://127.0.0.1:8188", "")
	cap := c.Capability()
	if cap.Name != "comfy.generate" {
		t.Errorf("name = %q", cap.Name)
	}
	if !cap.LongRun {
		t.Error("long_run = false, want true")
	}
	if cap.Summary == "" {
		t.Error("summary empty")
	}
	var schema map[string]any
	if err := json.Unmarshal(cap.InputSchema, &schema); err != nil {
		t.Fatalf("input_schema not JSON: %v", err)
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "workflow" {
		t.Errorf("input_schema required = %v, want [workflow]", req)
	}
	if schema["type"] != "object" {
		t.Errorf("input_schema type = %v, want object", schema["type"])
	}
}
