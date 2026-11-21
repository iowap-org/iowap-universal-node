// Package comfyui implements the first bridge provider: Stable Diffusion
// via a local ComfyUI installation (SPEC §1, board T-007).
package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

const (
	// DefaultPollInterval is the /history polling cadence (task T-007: 2 s).
	DefaultPollInterval = 2 * time.Second
	// startWaitTimeout bounds the "start ComfyUI and wait for health" step.
	startWaitTimeout = 90 * time.Second
	// probeTimeout bounds a single health probe / API request dial.
	probeTimeout = 3 * time.Second
	// maxHealthFailures before the capability reports down.
	maxHealthFailures = 3
)

// ComfyUI generates images through a local ComfyUI server.
type ComfyUI struct {
	baseURL string
	cmd     string // optional start command (IOWAP_COMFYUI_CMD)
	client  *http.Client

	pollInterval time.Duration
	startFn      func(ctx context.Context) error // seam for tests; nil = launch cmd

	mu         sync.Mutex
	failures   int
	process    *exec.Cmd
	processErr error
}

// New creates a provider talking to the ComfyUI at baseURL. When cmd is
// non-empty, a failed health probe starts ComfyUI via that command and
// waits for it to become healthy.
func New(baseURL, cmd string) *ComfyUI {
	return &ComfyUI{
		baseURL:      strings.TrimRight(baseURL, "/"),
		cmd:          cmd,
		client:       &http.Client{},
		pollInterval: DefaultPollInterval,
	}
}

// Capability implements provider.Provider (SPEC §3).
func (c *ComfyUI) Capability() provider.Capability {
	return provider.Capability{
		Name:    "comfy.generate",
		Summary: "Generate images with the local ComfyUI installation (Stable Diffusion)",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "workflow": {
      "type": "object",
      "description": "ComfyUI API-format workflow (the JSON saved via 'Save (API Format)')"
    }
  },
  "required": ["workflow"]
}`),
		LongRun: true,
		Health:  c.Health(),
	}
}

// --- Health (probe loop, start-on-demand) -----------------------------------

// Health returns the current capability health: down after
// maxHealthFailures consecutive failed probes, ok otherwise.
func (c *ComfyUI) Health() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures >= maxHealthFailures {
		return provider.HealthDown
	}
	return provider.HealthOK
}

// ProbeOnce performs one health probe (GET / with a /system_stats
// fallback, 200 = ok). On failure it increments the failure streak and —
// if a start command is configured — launches ComfyUI and waits for it.
// Returns the resulting health value.
func (c *ComfyUI) ProbeOnce(ctx context.Context) string {
	if c.probeEndpoint(ctx, "/") || c.probeEndpoint(ctx, "/system_stats") {
		c.mu.Lock()
		c.failures = 0
		c.mu.Unlock()
		return provider.HealthOK
	}

	needStart := false
	c.mu.Lock()
	c.failures++
	if c.cmd != "" && c.process == nil {
		needStart = true
	}
	c.mu.Unlock()

	if needStart {
		if err := c.startAndWait(ctx); err != nil {
			log.Printf("comfyui: start-on-demand failed: %v", err)
		}
	}
	return c.Health()
}

func (c *ComfyUI) probeEndpoint(ctx context.Context, path string) bool {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// startAndWait launches the configured command and polls health until it
// succeeds or startWaitTimeout elapses.
func (c *ComfyUI) startAndWait(ctx context.Context) error {
	c.mu.Lock()
	if c.process != nil {
		c.mu.Unlock()
		return nil // already starting/running
	}
	var err error
	if c.startFn != nil {
		c.process = &exec.Cmd{} // marker: a start is in flight
		c.mu.Unlock()
		err = c.startFn(ctx)
	} else {
		cmd := exec.Command("cmd", "/C", c.cmd)
		if !isWindows() {
			cmd = exec.Command("sh", "-c", c.cmd)
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err = cmd.Start(); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("launch %q: %w", c.cmd, err)
		}
		c.process = cmd
		c.mu.Unlock()
		go func() {
			_ = cmd.Wait()
			c.mu.Lock()
			c.process = nil // exited — allow a restart on the next failure
			c.mu.Unlock()
		}()
	}
	if err != nil {
		c.mu.Lock()
		c.process = nil
		c.mu.Unlock()
		return err
	}

	deadline := time.Now().Add(startWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if c.probeEndpoint(context.Background(), "/") {
			log.Printf("comfyui: started via %q — healthy", c.cmd)
			return nil
		}
	}
	return fmt.Errorf("comfyui did not become healthy within %v", startWaitTimeout)
}

func isWindows() bool { return os.PathSeparator == '\\' }

// StartHealthLoop implements provider.HealthProber.
func (c *ComfyUI) StartHealthLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if got := c.ProbeOnce(ctx); got != provider.HealthOK {
					log.Printf("comfyui: health probe failing (%s)", c.Health())
				}
			}
		}
	}()
}

// --- Exec (SPEC §5) ---------------------------------------------------------

// Exec submits the API-format workflow to /prompt, polls /history until
// the run completes, downloads the outputs via /view and writes them
// under workspaceDir.
func (c *ComfyUI) Exec(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, workspaceDir string) (map[string]any, []provider.Artifact, *provider.ExecError) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var parsed map[string]any
	if err := json.Unmarshal(params, &parsed); err != nil || parsed == nil {
		return nil, nil, provider.Terminal(provider.CodeInvalidParams, "params must be a JSON object")
	}
	workflow, ok := parsed["workflow"].(map[string]any)
	if !ok {
		return nil, nil, provider.Terminal(provider.CodeInvalidParams, `params must contain a "workflow" object (ComfyUI API-format workflow, "Save (API Format)")`)
	}

	promptID, execErr := c.submitPrompt(ctx, workflow)
	if execErr != nil {
		return nil, nil, execErr
	}

	outputs, execErr := c.awaitHistory(ctx, promptID)
	if execErr != nil {
		return nil, nil, execErr
	}

	images := make([]map[string]any, 0, 4)
	artifacts := make([]provider.Artifact, 0, 4)
	for _, nodeOutputs := range outputs {
		for _, img := range flattenImages(nodeOutputs) {
			rel, data, execErr := c.fetchOutput(ctx, taskID, img)
			if execErr != nil {
				return nil, nil, execErr
			}
			name, _ := img["filename"].(string)
			if err := writeWorkspaceFile(workspaceDir, rel, data); err != nil {
				return nil, nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("writing artifact: %v", err))
			}
			images = append(images, map[string]any{"name": name, "path": rel})
			artifacts = append(artifacts, provider.Artifact{Name: name, Path: rel})
		}
	}

	result := map[string]any{
		"prompt_id": promptID,
		"images":    images,
	}
	return result, artifacts, nil
}

// submitPrompt POSTs the workflow as {"prompt": ..., "client_id": ...}
// to /prompt and returns the prompt_id.
func (c *ComfyUI) submitPrompt(ctx context.Context, workflow map[string]any) (string, *provider.ExecError) {
	body, _ := json.Marshal(map[string]any{
		"prompt":    workflow,
		"client_id": "iowap-bridge",
	})
	status, respBody, err := c.request(ctx, http.MethodPost, "/prompt", body)
	if err != nil {
		if ctx.Err() != nil {
			return "", provider.Retryable(provider.CodeTimeout, "comfyui prompt submission timed out")
		}
		return "", provider.Retryable(provider.CodeProviderUnavail, "comfyui unreachable: "+err.Error())
	}
	if status == http.StatusBadRequest {
		return "", provider.Terminal(provider.CodeInvalidParams, "comfyui rejected the workflow (validation): "+truncate(string(respBody), 300))
	}
	if status < 200 || status > 299 {
		return "", provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("comfyui /prompt returned http %d: %s", status, truncate(string(respBody), 300)))
	}
	var parsed struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.PromptID == "" {
		return "", provider.Terminal(provider.CodeCapabilityFailed, "comfyui /prompt returned no prompt_id: "+truncate(string(respBody), 300))
	}
	return parsed.PromptID, nil
}

// awaitHistory polls /history/{promptID} every pollInterval until the
// entry is completed (SPEC/task: 2 s cadence, ctx timeout governs).
func (c *ComfyUI) awaitHistory(ctx context.Context, promptID string) (map[string]any, *provider.ExecError) {
	for {
		status, respBody, err := c.request(ctx, http.MethodGet, "/history/"+url.PathEscape(promptID), nil)
		if err == nil && status == http.StatusOK {
			var history map[string]struct {
				Status struct {
					StatusStr string `json:"status_str"`
					Completed bool   `json:"completed"`
				} `json:"status"`
				Outputs map[string]any `json:"outputs"`
			}
			if json.Unmarshal(respBody, &history) == nil {
				if entry, ok := history[promptID]; ok && entry.Status.Completed {
					if entry.Status.StatusStr != "success" {
						return nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("comfyui execution failed (status %q) for prompt %s", entry.Status.StatusStr, promptID))
					}
					return entry.Outputs, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, provider.Retryable(provider.CodeTimeout, fmt.Sprintf("comfyui run %s did not complete within the task timeout", promptID))
		case <-time.After(c.pollInterval):
		}
	}
}

// fetchOutput downloads one output image via /view and returns its
// workspace-relative path and bytes. Unsafe names are rejected.
func (c *ComfyUI) fetchOutput(ctx context.Context, taskID string, img map[string]any) (string, []byte, *provider.ExecError) {
	filename, _ := img["filename"].(string)
	subfolder, _ := img["subfolder"].(string)
	filetype, _ := img["type"].(string)
	if filetype == "" {
		filetype = "output"
	}

	rel, ok := buildRelPath(taskID, subfolder, filename)
	if !ok {
		return "", nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("comfyui reported an unsafe output name (filename=%q subfolder=%q) — rejected", filename, subfolder))
	}
	q := url.Values{}
	q.Set("filename", filename)
	q.Set("subfolder", subfolder)
	q.Set("type", filetype)
	status, data, err := c.request(ctx, http.MethodGet, "/view?"+q.Encode(), nil)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, provider.Retryable(provider.CodeTimeout, "timed out downloading comfyui output")
		}
		return "", nil, provider.Retryable(provider.CodeProviderUnavail, "comfyui unreachable while fetching output: "+err.Error())
	}
	if status != http.StatusOK {
		return "", nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("comfyui /view returned http %d for %s", status, filename))
	}
	return rel, data, nil
}

// request performs one HTTP call against the ComfyUI base URL.
func (c *ComfyUI) request(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}
