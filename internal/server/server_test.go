package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// stubProvider is a scripted provider for handler tests.
type stubProvider struct {
	cap    provider.Capability
	execFn func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError)
}

func (s *stubProvider) Capability() provider.Capability { return s.cap }

func (s *stubProvider) Exec(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
	return s.execFn(ctx, taskID, params, timeout, ws)
}

const testToken = "test-token-1234"

func newTestServer(t *testing.T, p provider.Provider) *Server {
	t.Helper()
	return New(testToken, t.TempDir(), p)
}

func doReq(t *testing.T, s *Server, method, target, token string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func newComfyStub() *stubProvider {
	return &stubProvider{
		cap: provider.Capability{
			Name:        "comfy.generate",
			Summary:     "Stable Diffusion via local ComfyUI",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"workflow":{"type":"object"}},"required":["workflow"]}`),
			LongRun:     true,
			Health:      provider.HealthOK,
		},
	}
}

// --- Auth (SPEC §2) ---------------------------------------------------------

func TestAuthRequiredOnAllEndpoints(t *testing.T) {
	s := newTestServer(t, newComfyStub())

	cases := []struct {
		method, target string
	}{
		{"GET", "/v1/manifest"},
		{"POST", "/v1/exec/comfy.generate"},
		{"POST", "/v1/exec/unknown.thing"},
		{"GET", "/v1/anything-else"},
	}
	for _, c := range cases {
		// No token at all.
		rec := doReq(t, s, c.method, c.target, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", c.method, c.target, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s %s WWW-Authenticate = %q, want Bearer", c.method, c.target, got)
		}
		// Wrong token.
		rec = doReq(t, s, c.method, c.target, "wrong-token", "{}")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with wrong token = %d, want 401", c.method, c.target, rec.Code)
		}
	}
}

func TestAuthAcceptsCorrectToken(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	rec := doReq(t, s, "GET", "/v1/manifest", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest with valid token = %d, want 200", rec.Code)
	}
}

// --- Manifest (SPEC §3) -----------------------------------------------------

func TestManifestShape(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	rec := doReq(t, s, "GET", "/v1/manifest", testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest = %d, want 200", rec.Code)
	}
	var m struct {
		Version  int `json:"version"`
		Provider struct {
			Name    string `json:"name"`
			HostOS  string `json:"host_os"`
			Version string `json:"version"`
		} `json:"provider"`
		Capabilities []struct {
			Name        string          `json:"name"`
			Summary     string          `json:"summary"`
			InputSchema json.RawMessage `json:"input_schema"`
			LongRun     bool            `json:"long_run"`
			Health      string          `json:"health"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("version = %d, want 1", m.Version)
	}
	if m.Provider.HostOS != runtime.GOOS {
		t.Errorf("provider.host_os = %q, want %q", m.Provider.HostOS, runtime.GOOS)
	}
	if m.Provider.Name == "" || m.Provider.Version == "" {
		t.Errorf("provider name/version empty: %+v", m.Provider)
	}
	if len(m.Capabilities) != 1 {
		t.Fatalf("capabilities = %d, want 1", len(m.Capabilities))
	}
	cap := m.Capabilities[0]
	if cap.Name != "comfy.generate" {
		t.Errorf("cap name = %q, want comfy.generate (no host. prefix)", cap.Name)
	}
	if !cap.LongRun {
		t.Error("cap long_run = false, want true")
	}
	if cap.Health != "ok" {
		t.Errorf("cap health = %q, want ok", cap.Health)
	}
	var schema map[string]any
	if err := json.Unmarshal(cap.InputSchema, &schema); err != nil {
		t.Fatalf("input_schema not JSON: %v", err)
	}
	req, _ := schema["required"].([]any)
	found := false
	for _, r := range req {
		if r == "workflow" {
			found = true
		}
	}
	if !found {
		t.Errorf("input_schema required = %v, want [workflow]", schema["required"])
	}
}

func TestManifestHealthDown(t *testing.T) {
	p := newComfyStub()
	p.cap.Health = provider.HealthDown
	s := newTestServer(t, p)
	rec := doReq(t, s, "GET", "/v1/manifest", testToken, "")
	var m struct {
		Capabilities []map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0]["health"] != "down" {
		t.Errorf("capabilities = %v, want one cap with health=down", m.Capabilities)
	}
}

// --- Exec (SPEC §5) ---------------------------------------------------------

func TestExecHappyPath(t *testing.T) {
	p := newComfyStub()
	var gotTaskID, gotWS string
	var gotParams json.RawMessage
	var gotTimeout time.Duration
	p.execFn = func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
		gotTaskID, gotParams, gotTimeout, gotWS = taskID, params, timeout, ws
		return map[string]any{"prompt_id": "p1"}, []provider.Artifact{{Name: "out.png", Path: "t1/out.png"}}, nil
	}
	s := newTestServer(t, p)
	body := `{"task_id":"t1","params":{"workflow":{"1":{}}},"timeout_s":60}`
	rec := doReq(t, s, "POST", "/v1/exec/comfy.generate", testToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if gotTaskID != "t1" {
		t.Errorf("provider got task_id %q", gotTaskID)
	}
	if gotTimeout != 60*time.Second {
		t.Errorf("provider got timeout %v, want 60s", gotTimeout)
	}
	if gotWS == "" {
		t.Error("provider got empty workspace dir")
	}
	if string(gotParams) != `{"workflow":{"1":{}}}` {
		t.Errorf("provider got params %s", gotParams)
	}
	var resp struct {
		Status    string           `json:"status"`
		Result    map[string]any   `json:"result"`
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q, want completed", resp.Status)
	}
	if resp.Result["prompt_id"] != "p1" {
		t.Errorf("result = %v", resp.Result)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0]["path"] != "t1/out.png" || resp.Artifacts[0]["name"] != "out.png" {
		t.Errorf("artifacts = %v", resp.Artifacts)
	}
}

func TestExecUnknownCapability(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	rec := doReq(t, s, "POST", "/v1/exec/no.such.cap", testToken, `{"task_id":"t1","params":{},"timeout_s":10}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown cap = %d, want 404", rec.Code)
	}
}

// All four error codes map onto a 200 "failed" response with the right
// code and retryable flag (SPEC §5).
func TestExecErrorMapping(t *testing.T) {
	cases := []struct {
		code      string
		retryable bool
	}{
		{provider.CodeCapabilityFailed, false},
		{provider.CodeInvalidParams, false},
		{provider.CodeProviderUnavail, true},
		{provider.CodeTimeout, true},
	}
	for _, c := range cases {
		p := newComfyStub()
		p.execFn = func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
			return nil, nil, &provider.ExecError{Code: c.code, Message: "boom", Retryable: c.retryable}
		}
		s := newTestServer(t, p)
		rec := doReq(t, s, "POST", "/v1/exec/comfy.generate", testToken, `{"task_id":"t1","params":{},"timeout_s":10}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: http = %d, want 200", c.code, rec.Code)
		}
		var resp struct {
			Status string `json:"status"`
			Error  struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: response not JSON: %v", c.code, err)
		}
		if resp.Status != "failed" || resp.Error.Code != c.code || resp.Error.Retryable != c.retryable {
			t.Errorf("%s: got status=%q code=%q retryable=%v, want failed/%s/%v",
				c.code, resp.Status, resp.Error.Code, resp.Error.Retryable, c.code, c.retryable)
		}
	}
}

func TestExecRejectsMalformedRequests(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	cases := []string{
		`not json at all`,
		`{"params":{},"timeout_s":10}`,                // missing task_id
		`{"task_id":"t1","timeout_s":10}`,             // missing params
		`{"task_id":"t1","params":{},"timeout_s":-5}`, // invalid timeout
	}
	for _, body := range cases {
		rec := doReq(t, s, "POST", "/v1/exec/comfy.generate", testToken, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, rec.Code)
		}
	}
}

// A provider-reported artifact path that escapes the workspace must be
// rejected, never echoed to the node (the node would upload files from
// outside the shared volume).
func TestExecRejectsArtifactPathTraversal(t *testing.T) {
	for _, bad := range []string{"../evil.png", "/etc/passwd", "a/../../evil.png"} {
		p := newComfyStub()
		p.execFn = func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
			return map[string]any{"ok": true}, []provider.Artifact{{Name: "evil.png", Path: bad}}, nil
		}
		s := newTestServer(t, p)
		rec := doReq(t, s, "POST", "/v1/exec/comfy.generate", testToken, `{"task_id":"t1","params":{},"timeout_s":10}`)
		var resp struct {
			Status string `json:"status"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response not JSON: %v", err)
		}
		if resp.Status != "failed" {
			t.Errorf("path %q: status = %q, want failed", bad, resp.Status)
		}
		if resp.Error.Code != provider.CodeCapabilityFailed {
			t.Errorf("path %q: code = %q, want capability.failed", bad, resp.Error.Code)
		}
		if !strings.Contains(resp.Error.Message, "artifact") {
			t.Errorf("path %q: message %q does not mention artifact", bad, resp.Error.Message)
		}
	}
}
