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
			MaxParallel int             `json:"max_parallel"`
			Cost        int             `json:"cost"`
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
	// Scheduling hints (board T-030): the stub leaves both fields at 0,
	// the manifest must still emit the default of 1 — never a raw 0.
	if cap.MaxParallel != 1 || cap.Cost != 1 {
		t.Errorf("max_parallel/cost = %d/%d, want default 1/1", cap.MaxParallel, cap.Cost)
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

// T-048: health is ADVISORY — the manifest is a declaration, not a
// gate. A health:down entry stays IN the list with its metadata field;
// only the (non-existing) filtering behavior would drop it.
func TestManifestHealthDownIsAdvisoryEntryStays(t *testing.T) {
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

// Explicit scheduling hints from a provider pass through unchanged —
// only values below 1 are coerced to the default.
func TestManifestSchedulingValues(t *testing.T) {
	p := newComfyStub()
	p.cap.MaxParallel = 3
	p.cap.Cost = 7
	s := newTestServer(t, p)
	rec := doReq(t, s, "GET", "/v1/manifest", testToken, "")
	var m struct {
		Capabilities []struct {
			MaxParallel int `json:"max_parallel"`
			Cost        int `json:"cost"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if len(m.Capabilities) != 1 {
		t.Fatalf("capabilities = %d, want 1", len(m.Capabilities))
	}
	if m.Capabilities[0].MaxParallel != 3 || m.Capabilities[0].Cost != 7 {
		t.Errorf("max_parallel/cost = %d/%d, want 3/7", m.Capabilities[0].MaxParallel, m.Capabilities[0].Cost)
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

// --- Spec-mode exec (board T-044, exec contract v1.1) ------------------------

// (a) spec-exec process happy path: the daemon builds the provider from
// the SPEC (no registry entry involved) and echoes the command's stdout.
func TestExecSpecProcessHappy(t *testing.T) {
	s := newTestServer(t, newComfyStub()) // registry holds only comfy.generate
	body := `{"task_id":"t-spec","params":{},"timeout_s":30,
	          "spec":{"name":"sys.run","type":"process","command":["echo","hello-spec"],"stdin_json":false}}`
	rec := doReq(t, s, "POST", "/v1/exec/sys.run", testToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("spec exec = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bridge-Exec"); got != "spec" {
		t.Errorf("X-Bridge-Exec = %q, want spec", got)
	}
	var resp struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q (body: %s), want completed", resp.Status, rec.Body.String())
	}
	if resp.Result["stdout"] != "hello-spec" {
		t.Errorf("result.stdout = %v, want hello-spec", resp.Result["stdout"])
	}
}

// (b) spec-exec ignores the registry: the capability name is completely
// unknown at this provider and the task STILL runs.
func TestExecSpecIgnoresRegistry(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	body := `{"task_id":"t-spec","params":{},"timeout_s":10,
	          "spec":{"type":"process","command":["echo","no-registry-needed"],"stdin_json":false}}`
	rec := doReq(t, s, "POST", "/v1/exec/never.registered", testToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("spec exec on unknown name = %d, want 200 (no registry lookup, body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Status != "completed" || resp.Result["stdout"] != "no-registry-needed" {
		t.Errorf("status/result = %q/%v, want completed/no-registry-needed", resp.Status, resp.Result["stdout"])
	}
}

// (T-048) An unresolvable process spec is NOT gated away (the manifest
// is a declaration) — so the honest failure happens at RUN TIME: the
// daemon answers 200 with the SPEC §5 error envelope (status failed,
// provider.unavailable, the real cause text) instead of a silent
// success. The node's publish decision never saw a health gate, and
// the task carries the error.
func TestExecSpecUnresolvableCommandHonestError(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	body := `{"task_id":"t-honest","params":{},"timeout_s":10,
	          "spec":{"type":"process","command":["/definitely/missing/iowap-no-such-binary","run"],"stdin_json":false}}`
	rec := doReq(t, s, "POST", "/v1/exec/never.resolvable", testToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("spec exec = %d, want 200 (SPEC §5 task-envelope, body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bridge-Exec"); got != "spec" {
		t.Errorf("X-Bridge-Exec = %q, want spec", got)
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
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Status != "failed" {
		t.Errorf("status = %q (body: %s), want failed — an unresolvable command must report an honest exec error, not complete", resp.Status, rec.Body.String())
	}
	if resp.Error.Code != "provider.unavailable" {
		t.Errorf("error.code = %q, want provider.unavailable", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "/definitely/missing/iowap-no-such-binary") {
		t.Errorf("error.message = %q, want the honest cause text (starting <name>: ...)", resp.Error.Message)
	}
	if !resp.Error.Retryable {
		t.Errorf("error.retryable = false, want true (start failure is provider-level, not capability.failed)")
	}
}

// (c) mis-shaped specs are a 422 (not the legacy 404, not a 200 "failed").
func TestExecSpecInvalidShapes422(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	cases := []struct {
		name   string
		spec   string
		wantIn string
	}{
		{"unknown type", `{"type":"teleport"}`, "teleport"},
		{"missing type", `{"command":["echo","x"]}`, "unknown provider type"},
		{"process without command", `{"type":"process"}`, "command"},
		{"empty command", `{"type":"process","command":[]}`, "command"},
		{"http without exec", `{"type":"http"}`, "exec"},
		{"http without url_template", `{"type":"http","exec":{"method":"GET"}}`, "url_template"},
		{"comfyui without url", `{"type":"comfyui"}`, "url"},
		{"unsafe workspace", `{"type":"process","command":["echo","x"],"workspace":"../escape"}`, "workspace"},
	}
	for _, c := range cases {
		body := `{"task_id":"t-spec","params":{},"timeout_s":10,"spec":` + c.spec + `}`
		rec := doReq(t, s, "POST", "/v1/exec/no.such.cap", testToken, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: spec exec = %d, want 422 (body: %s)", c.name, rec.Code, rec.Body.String())
			continue
		}
		if got := rec.Header().Get("X-Bridge-Exec"); got != "" {
			t.Errorf("%s: X-Bridge-Exec = %q, want absent", c.name, got)
		}
		var resp struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: response not JSON: %v", c.name, err)
		}
		if resp.Error.Code != "spec.invalid" {
			t.Errorf("%s: error.code = %q, want spec.invalid", c.name, resp.Error.Code)
		}
		if !strings.Contains(resp.Error.Message, c.wantIn) {
			t.Errorf("%s: message %q does not mention %q", c.name, resp.Error.Message, c.wantIn)
		}
	}
	// A spec that is not an object at all is also a 422.
	rec := doReq(t, s, "POST", "/v1/exec/no.such.cap", testToken,
		`{"task_id":"t-spec","params":{},"timeout_s":10,"spec":"echo x"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("non-object spec = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
}

// (d) name-only legacy exec stays on the old registry path (pre-0.3.7
// nodes keep working) and is marked deprecated via X-Bridge-Exec.
func TestExecLegacyNameOnlyResolvesRegistry(t *testing.T) {
	p := newComfyStub()
	p.execFn = func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
		return map[string]any{"via": "registry"}, nil, nil
	}
	s := newTestServer(t, p)
	rec := doReq(t, s, "POST", "/v1/exec/comfy.generate", testToken,
		`{"task_id":"t1","params":{},"timeout_s":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy exec = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bridge-Exec"); got != "legacy" {
		t.Errorf("X-Bridge-Exec = %q, want legacy (deprecation note)", got)
	}
	var resp struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Status != "completed" || resp.Result["via"] != "registry" {
		t.Errorf("status/result = %q/%v, want completed/registry", resp.Status, resp.Result["via"])
	}
	// And the unknown-name behavior is unchanged for name-only execs.
	rec = doReq(t, s, "POST", "/v1/exec/no.such.cap", testToken,
		`{"task_id":"t1","params":{},"timeout_s":10}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("legacy unknown name = %d, want 404", rec.Code)
	}
}

// (e) bearer auth covers the spec path exactly like the legacy path.
func TestExecSpecBearerUnchanged(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	body := `{"task_id":"t-spec","params":{},"timeout_s":10,"spec":{"type":"process","command":["echo","x"],"stdin_json":false}}`
	for _, token := range []string{"", "wrong-token"} {
		rec := doReq(t, s, "POST", "/v1/exec/never.registered", token, body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("spec exec with token %q = %d, want 401", token, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("spec exec with token %q: WWW-Authenticate = %q, want Bearer", token, got)
		}
	}
}

// (f) params and task_id reach the spec-built runner: params ride the
// stdin JSON pipe, task_id is substituted into the argv template.
func TestExecSpecForwardsParamsAndTaskID(t *testing.T) {
	s := newTestServer(t, newComfyStub())
	// stdin_json defaults to true when absent — params arrive as JSON.
	rec := doReq(t, s, "POST", "/v1/exec/stdin.cap", testToken,
		`{"task_id":"t1","params":{"k":"v"},"timeout_s":10,"spec":{"type":"process","command":["cat"]}}`)
	var first struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if rec.Code != http.StatusOK || first.Status != "completed" {
		t.Fatalf("cat spec exec = %d/%q (body: %s)", rec.Code, first.Status, rec.Body.String())
	}
	if first.Result["stdout"] != `{"k":"v"}` {
		t.Errorf("params stdout = %v, want {\"k\":\"v\"}", first.Result["stdout"])
	}
	// argv template substitution carries task_id.
	rec = doReq(t, s, "POST", "/v1/exec/subst.cap", testToken,
		`{"task_id":"t-sf1","params":{},"timeout_s":10,"spec":{"type":"process","command":["echo","{{task_id}}"],"stdin_json":false}}`)
	var second struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if rec.Code != http.StatusOK || second.Status != "completed" {
		t.Fatalf("echo spec exec = %d/%q (body: %s)", rec.Code, second.Status, rec.Body.String())
	}
	if second.Result["stdout"] != "t-sf1" {
		t.Errorf("task_id stdout = %v, want t-sf1", second.Result["stdout"])
	}
}

// (d2) Unknown body keys never break the legacy path: this is the exact
// decode behavior that lets a 0.3.7 node send `spec` to a pre-0.3.7
// daemon — the old exe drops the key and registry-resolves the name.
func TestExecLegacyToleratesUnknownBodyKeys(t *testing.T) {
	p := newComfyStub()
	p.execFn = func(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, ws string) (map[string]any, []provider.Artifact, *provider.ExecError) {
		return map[string]any{"via": "registry"}, nil, nil
	}
	s := newTestServer(t, p)
	rec := doReq(t, s, "POST", "/v1/exec/comfy.generate", testToken,
		`{"task_id":"t1","params":{},"timeout_s":10,"future_unknown_key":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy exec with unknown key = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Bridge-Exec"); got != "legacy" {
		t.Errorf("X-Bridge-Exec = %q, want legacy", got)
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q, want completed", resp.Status)
	}
}
