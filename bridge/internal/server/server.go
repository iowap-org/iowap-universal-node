// Package server implements the bridge HTTP contract (SPEC §2–§5):
// GET /v1/manifest and POST /v1/exec/{name} behind Bearer auth,
// bound to loopback only by the caller (config.BindAddr).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// Version is the daemon version reported in the manifest.
const Version = "0.3.5"

// ProviderName is the manifest provider name.
const ProviderName = "iowap-bridge"

// DefaultExecTimeout is used when the request omits timeout_s
// (SPEC §5 example budget).
const DefaultExecTimeout = 3600 * time.Second

// Server serves the bridge contract for the registered providers.
type Server struct {
	token        string
	workspaceDir string

	mu           sync.RWMutex
	providers    map[string]provider.Provider
	healthCancel context.CancelFunc // cancels the current generation's health loops
}

// New builds a server. Providers are indexed by their capability name;
// duplicates keep the first registration.
func New(token, workspaceDir string, providers ...provider.Provider) *Server {
	s := &Server{
		token:        token,
		workspaceDir: workspaceDir,
		providers:    make(map[string]provider.Provider, len(providers)),
	}
	s.setProviders(providers)
	return s
}

// setProviders indexes providers by capability name. Caller holds mu.
func (s *Server) setProviders(providers []provider.Provider) {
	s.providers = make(map[string]provider.Provider, len(providers))
	for _, p := range providers {
		capability := p.Capability()
		if !provider.ValidCapName(capability.Name) {
			log.Printf("provider %T: skipping invalid capability name %q", p, capability.Name)
			continue
		}
		if _, exists := s.providers[capability.Name]; exists {
			log.Printf("provider %T: capability %q already registered — keeping first", p, capability.Name)
			continue
		}
		s.providers[capability.Name] = p
	}
}

// ReplaceProviders atomically swaps the served registry (board T-022,
// workspace provider watch): the manifest and exec routing reflect the
// new set immediately, and the previous generation's health loops are
// stopped before the new ones start. Safe for concurrent requests.
func (s *Server) ReplaceProviders(ctx context.Context, healthInterval time.Duration, providers ...provider.Provider) {
	s.mu.Lock()
	if s.healthCancel != nil {
		s.healthCancel()
	}
	hctx, cancel := context.WithCancel(ctx)
	s.healthCancel = cancel
	s.setProviders(providers)
	current := make([]provider.Provider, 0, len(s.providers))
	for _, p := range s.providers {
		current = append(current, p)
	}
	s.mu.Unlock()

	for _, p := range current {
		if hp, ok := p.(provider.HealthProber); ok {
			hp.StartHealthLoop(hctx, healthInterval)
		}
	}
}

// StartHealthLoops is superseded by ReplaceProviders (board T-022): it
// starts health probing for the current registry under the CALLER's
// context (never canceled by later swaps). Kept for direct consumers that
// do not participate in hot-reload.
func (s *Server) StartHealthLoops(ctx context.Context, interval time.Duration) {
	s.mu.RLock()
	current := make([]provider.Provider, 0, len(s.providers))
	for _, p := range s.providers {
		current = append(current, p)
	}
	s.mu.RUnlock()
	for _, p := range current {
		if hp, ok := p.(provider.HealthProber); ok {
			hp.StartHealthLoop(ctx, interval)
		}
	}
}

// Handler returns the fully wired http.Handler (auth over ALL routes).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/manifest", s.handleManifest)
	mux.HandleFunc("POST /v1/exec/{name}", s.handleExec)
	return s.authMiddleware(mux)
}

// authMiddleware enforces the Bearer token on every request, with a
// constant-time comparison.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r.Header.Get("Authorization"))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"code": "unauthorized", "message": "missing or invalid bearer token"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token from a "Bearer <token>" header value.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return header[len(prefix):]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	providers := make([]provider.Provider, 0, len(s.providers))
	for _, p := range s.providers {
		providers = append(providers, p)
	}
	s.mu.RUnlock()

	capabilities := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		capability := p.Capability()
		if capability.Health == "" {
			capability.Health = provider.HealthOK
		}
		// Scheduling hints (board T-030): the registry normalizes these,
		// but providers registered directly (without the registry) must
		// not leak 0 into the manifest either — default to 1.
		if capability.MaxParallel < 1 {
			capability.MaxParallel = 1
		}
		if capability.Cost < 1 {
			capability.Cost = 1
		}
		schema := capability.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{},"required":[]}`)
		}
		capabilities = append(capabilities, map[string]any{
			"name":         capability.Name,
			"summary":      capability.Summary,
			"input_schema": json.RawMessage(schema),
			"long_run":     capability.LongRun,
			"health":       capability.Health,
			// Scheduling hints (board T-030): the registry normalizes
			// both to >= 1, so they are always present and valid.
			"max_parallel": capability.MaxParallel,
			"cost":         capability.Cost,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": 1,
		"provider": map[string]any{
			"name":    ProviderName,
			"host_os": runtime.GOOS,
			"version": Version,
		},
		"capabilities": capabilities,
	})
}

type execRequest struct {
	TaskID   string          `json:"task_id"`
	Params   json.RawMessage `json:"params"`
	TimeoutS *int            `json:"timeout_s"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func failedBody(code, message string, retryable bool) map[string]any {
	return map[string]any{
		"status": "failed",
		"error":  errorBody{Code: code, Message: message, Retryable: retryable},
	}
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.RLock()
	p, ok := s.providers[name]
	s.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]any{
				"code":    "capability.unknown",
				"message": fmt.Sprintf("no capability %q at this provider", name),
			},
		})
		return
	}

	var req execRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": provider.CodeInvalidParams, "message": "request body is not a valid exec request: " + err.Error()},
		})
		return
	}
	if strings.TrimSpace(req.TaskID) == "" || len(req.Params) == 0 || string(req.Params) == "null" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"code": provider.CodeInvalidParams, "message": "task_id and params are required"},
		})
		return
	}
	timeout := DefaultExecTimeout
	if req.TimeoutS != nil {
		if *req.TimeoutS <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"code": provider.CodeInvalidParams, "message": "timeout_s must be a positive integer"},
			})
			return
		}
		timeout = time.Duration(*req.TimeoutS) * time.Second
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	result, artifacts, execErr := p.Exec(ctx, req.TaskID, req.Params, timeout, s.workspaceDir)
	if execErr != nil {
		writeJSON(w, http.StatusOK, failedBody(execErr.Code, execErr.Message, execErr.Retryable))
		return
	}

	// Never echo artifact paths that escape the workspace — the node
	// would read and upload files from outside the shared volume.
	for _, a := range artifacts {
		if _, ok := provider.SanitizeRelPath(a.Path); !ok {
			writeJSON(w, http.StatusOK, failedBody(
				provider.CodeCapabilityFailed,
				fmt.Sprintf("provider returned an unsafe artifact path %q — rejected", a.Path),
				false,
			))
			return
		}
	}

	out := map[string]any{
		"status": "completed",
		"result": result,
	}
	if result == nil {
		out["result"] = map[string]any{}
	}
	if artifacts != nil {
		list := make([]map[string]any, 0, len(artifacts))
		for _, a := range artifacts {
			list = append(list, map[string]any{"name": a.Name, "path": a.Path})
		}
		out["artifacts"] = list
	}
	writeJSON(w, http.StatusOK, out)
}
