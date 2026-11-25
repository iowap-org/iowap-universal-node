package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iowap-org/iowap-bridge-daemon/internal/server"
)

// End-to-end: a providers.yaml with two http providers feeds server.New —
// the manifest assembles from the registry and exec dispatches to the
// right provider (the "dispatch registry").
func TestManifestAssemblyAndDispatch(t *testing.T) {
	var hitsA, hitsB int
	backend := func(name string, hits *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*hits++
			_, _ = w.Write([]byte(`{"result":{"served":"` + name + `"}}`))
		}))
	}
	srvA, srvB := backend("A", &hitsA), backend("B", &hitsB)
	defer srvA.Close()
	defer srvB.Close()

	p := write(t, `
providers:
  - name: cap.one
    type: http
    exec:
      url_template: `+srvA.URL+`/run
      body_template: '{}'
  - name: cap.two
    type: http
    exec:
      url_template: `+srvB.URL+`/run
      body_template: '{}'
`)
	provs, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	s := server.New("tok", t.TempDir(), provs...)

	// Manifest contains both capabilities with health.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest = %d", rec.Code)
	}
	var m struct {
		Provider struct {
			Version string `json:"version"`
		} `json:"provider"`
		Capabilities []struct {
			Name   string `json:"name"`
			Health string `json:"health"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if m.Provider.Version != server.Version {
		t.Errorf("provider.version = %q, want %q", m.Provider.Version, server.Version)
	}
	if len(m.Capabilities) != 2 {
		t.Fatalf("capabilities = %d, want 2", len(m.Capabilities))
	}
	names := map[string]bool{m.Capabilities[0].Name: true, m.Capabilities[1].Name: true}
	if !names["cap.one"] || !names["cap.two"] {
		t.Errorf("capabilities = %+v", m.Capabilities)
	}
	for _, c := range m.Capabilities {
		if c.Health != "ok" {
			t.Errorf("cap %s health = %q, want ok", c.Name, c.Health)
		}
	}

	// Dispatch: exec for cap.two reaches only backend B.
	doExec := func(capName string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/exec/"+capName, strings.NewReader(`{"task_id":"t1","params":{},"timeout_s":10}`))
		req.Header.Set("Authorization", "Bearer tok")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("exec %s = %d: %s", capName, rec.Code, rec.Body.String())
		}
	}
	doExec("cap.two")
	if hitsB != 1 || hitsA != 0 {
		t.Errorf("dispatch: hitsA=%d hitsB=%d, want 0/1", hitsA, hitsB)
	}
	doExec("cap.one")
	if hitsA != 1 || hitsB != 1 {
		t.Errorf("dispatch: hitsA=%d hitsB=%d, want 1/1", hitsA, hitsB)
	}

	// Unknown capabilities are a 404 and reach no backend.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/exec/cap.unknown", strings.NewReader(`{"task_id":"t1","params":{},"timeout_s":10}`))
	req.Header.Set("Authorization", "Bearer tok")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown cap = %d, want 404", rec.Code)
	}
	if hitsA != 1 || hitsB != 1 {
		t.Errorf("unknown cap must not hit any backend (hitsA=%d hitsB=%d)", hitsA, hitsB)
	}
}
