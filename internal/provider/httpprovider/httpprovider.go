// Package httpprovider implements the config-driven generic REST provider
// (board T-019): any HTTP endpoint becomes a capability via health_url
// probe and an exec mapping (method, url_template, headers, body_template,
// result_path, artifact_paths) with simple {{param}} substitution.
package httpprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/subst"
)

const (
	// probeTimeout bounds a single health probe / exec dial.
	probeTimeout = 3 * time.Second
	// maxHealthFailures before the capability reports down (matches the
	// ComfyUI provider semantics).
	maxHealthFailures = 3
)

// Config describes one `type: http` provider from providers.yaml.
type Config struct {
	Name    string
	Summary string
	LongRun bool
	Schema  json.RawMessage

	// HealthURL is probed (GET, 2xx = ok). Empty → the capability is
	// always considered healthy (no probe).
	HealthURL string

	Method        string            // default POST
	URLTemplate   string            // required; {{param}}/{{task_id}} substitution
	Headers       map[string]string // values support substitution
	BodyTemplate  string            // optional; JSON with {{param}} substitution
	ResultPath    string            // dot path into the response; empty → whole response object
	ArtifactPaths []string          // dot paths to strings (workspace-relative artifact paths)
}

// Provider calls a REST endpoint for every exec task.
type Provider struct {
	cfg    Config
	client *http.Client

	mu       sync.Mutex
	failures int
}

// New builds the provider from its config entry.
func New(cfg Config) *Provider {
	if cfg.Method == "" {
		cfg.Method = http.MethodPost
	}
	return &Provider{cfg: cfg, client: &http.Client{}}
}

// Capability implements provider.Provider (SPEC §3).
func (p *Provider) Capability() provider.Capability {
	return provider.Capability{
		Name:        p.cfg.Name,
		Summary:     p.cfg.Summary,
		InputSchema: p.cfg.Schema,
		LongRun:     p.cfg.LongRun,
		Health:      p.Health(),
	}
}

// --- Health ------------------------------------------------------------------

// Health reports ok until maxHealthFailures consecutive failed probes.
func (p *Provider) Health() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.HealthURL == "" {
		return provider.HealthOK
	}
	return p.healthLocked()
}

// ProbeOnce performs one health probe and returns the resulting health.
func (p *Provider) ProbeOnce(ctx context.Context) string {
	if p.cfg.HealthURL == "" {
		return provider.HealthOK
	}
	ok := p.probe(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if ok {
		p.failures = 0
		return provider.HealthOK
	}
	p.failures++
	return p.healthLocked()
}

func (p *Provider) healthLocked() string {
	if p.failures >= maxHealthFailures {
		return provider.HealthDown
	}
	return provider.HealthOK
}

// StartHealthLoop implements provider.HealthProber (15 s cadence by default
// from the server; no-op without a configured health_url).
func (p *Provider) StartHealthLoop(ctx context.Context, interval time.Duration) {
	if p.cfg.HealthURL == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if got := p.ProbeOnce(ctx); got != provider.HealthOK {
					fmt.Printf("httpprovider %s: health probe failing (%s)\n", p.cfg.Name, p.Health())
				}
			}
		}
	}()
}

func (p *Provider) probe(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, p.cfg.HealthURL, nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode <= 299
}

// --- Exec (SPEC §5) ----------------------------------------------------------

// Exec substitutes the params into the configured request, sends it and
// maps the response onto result/artifacts per result_path/artifact_paths.
func (p *Provider) Exec(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, workspaceDir string) (map[string]any, []provider.Artifact, *provider.ExecError) {
	_ = workspaceDir // artifact paths are workspace-relative strings from the response
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vars, err := subst.Vars(taskID, params)
	if err != nil {
		return nil, nil, provider.Terminal(provider.CodeInvalidParams, err.Error())
	}

	urlStr, execErr := expand("url_template", p.cfg.URLTemplate, vars)
	if execErr != nil {
		return nil, nil, execErr
	}
	body, execErr := expand("body_template", p.cfg.BodyTemplate, vars)
	if execErr != nil {
		return nil, nil, execErr
	}
	headers := make(map[string]string, len(p.cfg.Headers))
	for k, v := range p.cfg.Headers {
		expanded, execErr := expand("header "+k, v, vars)
		if execErr != nil {
			return nil, nil, execErr
		}
		headers[k] = expanded
	}

	req, err := http.NewRequestWithContext(ctx, p.cfg.Method, urlStr, stringPtr(body))
	if err != nil {
		return nil, nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("building request: %v", err))
	}
	if body != "" && headers["Content-Type"] == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, provider.Retryable(provider.CodeTimeout, "http provider request timed out")
		}
		return nil, nil, provider.Retryable(provider.CodeProviderUnavail, "http provider unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, nil, provider.Retryable(provider.CodeProviderUnavail, "reading http provider response: "+err.Error())
	}

	if resp.StatusCode == http.StatusBadRequest {
		return nil, nil, provider.Terminal(provider.CodeInvalidParams, "http provider rejected the params (validation): "+truncate(string(respBody)))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("http provider returned status %d: %s", resp.StatusCode, truncate(string(respBody))))
	}

	var parsed any
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return nil, nil, provider.Terminal(provider.CodeCapabilityFailed, "http provider response is not valid JSON")
		}
	}

	result, execErr := p.extractResult(parsed)
	if execErr != nil {
		return nil, nil, execErr
	}
	artifacts, execErr := p.extractArtifacts(parsed)
	if execErr != nil {
		return nil, nil, execErr
	}
	return result, artifacts, nil
}

func (p *Provider) extractResult(parsed any) (map[string]any, *provider.ExecError) {
	if p.cfg.ResultPath == "" {
		if obj, ok := parsed.(map[string]any); ok {
			return obj, nil
		}
		if parsed == nil {
			return map[string]any{}, nil
		}
		return nil, provider.Terminal(provider.CodeCapabilityFailed, "http provider response is not a JSON object and no result_path is configured")
	}
	v, ok := lookupPath(parsed, p.cfg.ResultPath)
	if !ok {
		return nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("result_path %q not found in http provider response", p.cfg.ResultPath))
	}
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case nil:
		return map[string]any{}, nil
	default:
		// Scalars/arrays are wrapped so the exec result stays an object.
		return map[string]any{"value": t}, nil
	}
}

func (p *Provider) extractArtifacts(parsed any) ([]provider.Artifact, *provider.ExecError) {
	if len(p.cfg.ArtifactPaths) == 0 {
		return nil, nil
	}
	var artifacts []provider.Artifact
	for _, ap := range p.cfg.ArtifactPaths {
		v, ok := lookupPath(parsed, ap)
		if !ok {
			return nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("artifact_paths entry %q not found in http provider response", ap))
		}
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("artifact_paths entry %q is not a string path", ap))
		}
		artifacts = append(artifacts, provider.Artifact{Name: path.Base(s), Path: s})
	}
	return artifacts, nil
}

// lookupPath resolves a "a.b.0" dot path against decoded JSON.
func lookupPath(v any, dotPath string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(dotPath, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			cur = node[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

func expand(what, tpl string, vars map[string]any) (string, *provider.ExecError) {
	if tpl == "" {
		return "", nil
	}
	out, err := subst.Expand(tpl, vars)
	if err != nil {
		return "", provider.Terminal(provider.CodeInvalidParams, what+": "+err.Error())
	}
	return out, nil
}

func stringPtr(s string) io.Reader {
	if s == "" {
		return nil
	}
	return strings.NewReader(s)
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
