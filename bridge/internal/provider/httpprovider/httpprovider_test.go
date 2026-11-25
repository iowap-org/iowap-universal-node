package httpprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// --- Exec --------------------------------------------------------------------

// The happy path: template substitution in URL, header and body; result_path
// and artifact_paths mapped onto the exec result.
func TestExecSubstitutionAndMapping(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth, gotCT = r.Method, r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type")
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"id":"job-7","status":"done"},"files":["out/report.pdf","out/extra.txt"]}`))
	}))
	defer srv.Close()

	p := New(Config{
		Name:          "render.doc",
		Summary:       "Render a document",
		LongRun:       true,
		Method:        "PUT",
		URLTemplate:   srv.URL + "/api/{{task_id}}/render/{{file}}",
		Headers:       map[string]string{"X-Api-Key": "key-{{task_id}}"},
		BodyTemplate:  `{"file": "{{file}}", "opts": {{opts}}}`,
		ResultPath:    "result",
		ArtifactPaths: []string{"files.0", "files.1"},
	})

	result, artifacts, execErr := p.Exec(context.Background(), "t-1",
		json.RawMessage(`{"file":"report.docx","opts":{"color":true}}`), time.Minute, t.TempDir())
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}

	if gotMethod != "PUT" {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if gotPath != "/api/t-1/render/report.docx" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "key-t-1" {
		t.Errorf("X-Api-Key = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody != `{"file": "report.docx", "opts": {"color":true}}` {
		t.Errorf("body = %q", gotBody)
	}
	if result["id"] != "job-7" || result["status"] != "done" {
		t.Errorf("result = %v, want the result_path object", result)
	}
	if len(artifacts) != 2 || artifacts[0].Path != "out/report.pdf" || artifacts[0].Name != "report.pdf" ||
		artifacts[1].Path != "out/extra.txt" {
		t.Errorf("artifacts = %+v", artifacts)
	}
}

// Without result_path the whole response object is the result; without
// artifact_paths no artifacts are returned.
func TestExecWholeResponseAsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value": 42, "note": "hi"}`))
	}))
	defer srv.Close()

	p := New(Config{Name: "x", URLTemplate: srv.URL})
	result, artifacts, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, "")
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	if result["value"] != float64(42) || len(artifacts) != 0 {
		t.Errorf("result = %v, artifacts = %v", result, artifacts)
	}
}

// A scalar result_path value is wrapped as {"value": ...}.
func TestExecScalarResultPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"job_id":"j-1"}`))
	}))
	defer srv.Close()
	p := New(Config{Name: "x", URLTemplate: srv.URL, ResultPath: "job_id"})
	result, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, "")
	if execErr != nil || result["value"] != "j-1" {
		t.Errorf("result = %v, err = %v", result, execErr)
	}
}

// Error mapping (SPEC §5): 400 → invalid_params, 5xx → capability.failed,
// unreachable → provider.unavailable (retryable).
func TestExecErrorMapping(t *testing.T) {
	cases := []struct {
		status      int
		wantCode    string
		wantRetry   bool
		unreachable bool
	}{
		{http.StatusBadRequest, provider.CodeInvalidParams, false, false},
		{http.StatusInternalServerError, provider.CodeCapabilityFailed, false, false},
		{0, provider.CodeProviderUnavail, true, true},
	}
	for _, c := range cases {
		var srvURL string
		var srv *httptest.Server
		if !c.unreachable {
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()
			srvURL = srv.URL
		} else {
			srvURL = "http://127.0.0.1:1" // nothing listens here
		}
		p := New(Config{Name: "x", URLTemplate: srvURL})
		_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, "")
		if execErr == nil || execErr.Code != c.wantCode || execErr.Retryable != c.wantRetry {
			t.Errorf("status %d: got %v, want %s retryable=%v", c.status, execErr, c.wantCode, c.wantRetry)
		}
	}
}

// A missing substitution variable is a terminal invalid_params error.
func TestExecMissingParam(t *testing.T) {
	p := New(Config{Name: "x", URLTemplate: "http://127.0.0.1:1/{{missing}}"})
	_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`{"other":1}`), time.Minute, "")
	if execErr == nil || execErr.Code != provider.CodeInvalidParams {
		t.Errorf("execErr = %v, want invalid_params", execErr)
	}
}

// Non-object params are rejected as invalid_params.
func TestExecNonObjectParams(t *testing.T) {
	p := New(Config{Name: "x", URLTemplate: "http://127.0.0.1:1/"})
	_, _, execErr := p.Exec(context.Background(), "t", json.RawMessage(`[1]`), time.Minute, "")
	if execErr == nil || execErr.Code != provider.CodeInvalidParams {
		t.Errorf("execErr = %v, want invalid_params", execErr)
	}
}

// result_path / artifact_paths that do not exist in the response fail the
// task terminally — silent empty results would be worse.
func TestExecMissingPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()

	p := New(Config{Name: "x", URLTemplate: srv.URL, ResultPath: "nope"})
	if _, _, err := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, ""); err == nil || err.Code != provider.CodeCapabilityFailed {
		t.Errorf("missing result_path: %v", err)
	}

	p = New(Config{Name: "x", URLTemplate: srv.URL, ArtifactPaths: []string{"nope"}})
	if _, _, err := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, ""); err == nil || err.Code != provider.CodeCapabilityFailed {
		t.Errorf("missing artifact path: %v", err)
	}

	p = New(Config{Name: "x", URLTemplate: srv.URL, ArtifactPaths: []string{"a"}})
	if _, _, err := p.Exec(context.Background(), "t", json.RawMessage(`{}`), time.Minute, ""); err == nil || err.Code != provider.CodeCapabilityFailed {
		t.Errorf("non-string artifact path: %v", err)
	}
}

// --- Health --------------------------------------------------------------------

func TestHealthProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := New(Config{Name: "x", URLTemplate: srv.URL, HealthURL: srv.URL + "/health"})
	if got := p.Health(); got != provider.HealthOK {
		t.Fatalf("fresh health = %q, want ok", got)
	}
	// Three consecutive failures → down (matches ComfyUI semantics: the
	// capability flips to down on the third failed probe).
	for i := 0; i < 3; i++ {
		want := provider.HealthOK
		if i == 2 {
			want = provider.HealthDown
		}
		if got := p.ProbeOnce(context.Background()); got != want {
			t.Errorf("probe %d = %q, want %q", i+1, got, want)
		}
	}
	if got := p.Capability().Health; got != provider.HealthDown {
		t.Errorf("capability health = %q, want down", got)
	}
	// Recovery resets the failure streak.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if got := p.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("probe after recovery = %q, want ok", got)
	}
}

// Without health_url the capability is always healthy and the probe loop
// is a no-op.
func TestHealthWithoutURL(t *testing.T) {
	p := New(Config{Name: "x", URLTemplate: "http://127.0.0.1:1"})
	if got := p.ProbeOnce(context.Background()); got != provider.HealthOK {
		t.Errorf("ProbeOnce = %q, want ok", got)
	}
	if got := p.Health(); got != provider.HealthOK {
		t.Errorf("Health = %q, want ok", got)
	}
	p.StartHealthLoop(context.Background(), time.Millisecond) // must not panic or spin
}

// Capability reports the configured name/summary/schema/long_run and the
// current health.
func TestCapability(t *testing.T) {
	p := New(Config{
		Name:    "render.doc",
		Summary: "Render",
		LongRun: true,
		Schema:  json.RawMessage(`{"type":"object"}`),
	})
	cap := p.Capability()
	if cap.Name != "render.doc" || cap.Summary != "Render" || !cap.LongRun {
		t.Errorf("capability = %+v", cap)
	}
	if string(cap.InputSchema) != `{"type":"object"}` {
		t.Errorf("schema = %s", cap.InputSchema)
	}
}
