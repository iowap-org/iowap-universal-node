// Package registry loads the config-driven provider registry (board
// T-019): providers.yaml in the daemon config dir turns host applications
// and scripts into capabilities without a daemon rebuild. The built-in
// ComfyUI provider is one configurable type among three (comfyui, http,
// process) — the server's manifest assembly and exec dispatch stay
// generic over the provider.Provider interface.
//
// Loading semantics:
//   - missing or empty file → empty registry, NO error (the daemon serves
//     an empty manifest rather than crashing);
//   - invalid YAML, unknown types, missing required fields → error (the
//     daemon refuses to start rather than silently dropping capabilities).
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/comfyui"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/httpprovider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/process"

	"gopkg.in/yaml.v3"
)

// fileConfig is the providers.yaml schema.
type fileConfig struct {
	Providers []entry `yaml:"providers"`
}

type entry struct {
	// Common (name is required except for the default comfy.generate).
	Name        string         `yaml:"name"`
	Type        string         `yaml:"type"`
	Summary     string         `yaml:"summary"`
	LongRun     bool           `yaml:"long_run"`
	InputSchema map[string]any `yaml:"input_schema"`
	// Optional per-provider workspace: a subdirectory (workspace-relative,
	// sanitized) this provider's Exec runs in.
	Workspace string `yaml:"workspace"`

	// Scheduling hints (board T-030): max_parallel is the number of
	// concurrent exec slots the node may use for this capability, cost
	// the point cost per task. Both are tolerant of missing/0/negative/
	// non-numeric values and default to 1 (see flexInt + normalize).
	MaxParallel flexInt `yaml:"max_parallel"`
	Cost        flexInt `yaml:"cost"`

	// type: comfyui
	URL      string `yaml:"url"`
	StartCmd string `yaml:"start_cmd"`

	// type: http
	HealthURL string    `yaml:"health_url"`
	Exec      *execConf `yaml:"exec"`

	// type: process
	Command       []string `yaml:"command"`
	StdinJSON     *bool    `yaml:"stdin_json"`
	ArtifactsGlob string   `yaml:"artifacts_glob"`
}

type execConf struct {
	Method        string            `yaml:"method"`
	URLTemplate   string            `yaml:"url_template"`
	Headers       map[string]string `yaml:"headers"`
	BodyTemplate  string            `yaml:"body_template"`
	ResultPath    string            `yaml:"result_path"`
	ArtifactPaths []string          `yaml:"artifact_paths"`
}

// flexInt decodes an optional YAML integer without turning garbage into a
// config error: the Node dashboard (board T-030) writes max_parallel/cost,
// and a stray non-numeric value must degrade to the default (1) instead of
// refusing daemon startup. Any decode failure leaves the zero value, which
// normalize() maps to 1.
type flexInt int

func (f *flexInt) UnmarshalYAML(value *yaml.Node) error {
	var n int
	if err := value.Decode(&n); err != nil {
		*f = 0
		return nil // tolerated: normalize() applies the default
	}
	*f = flexInt(n)
	return nil
}

// normalizeInt maps missing/zero/negative values to the default of 1.
func normalizeInt(f flexInt) int {
	if f < 1 {
		return 1
	}
	return int(f)
}

// instantiate builds the provider for ONE resolved capability name.
// Shared by Load (file registry) and Instantiate (spec-exec, board T-044)
// so both paths run the exact same type switch, wrapping and workspace
// scoping — spec-exec must not become a second, divergent runner.
func instantiate(name string, e entry) (provider.Provider, error) {
	workspace := ""
	if e.Workspace != "" {
		ws, ok := provider.SanitizeRelPath(e.Workspace)
		if !ok {
			return nil, fmt.Errorf("workspace %q must be a safe workspace-relative path", e.Workspace)
		}
		workspace = ws
	}

	var inner provider.Provider
	switch e.Type {
	case "comfyui":
		if e.URL == "" {
			return nil, fmt.Errorf("type comfyui requires url")
		}
		inner = comfyui.New(e.URL, e.StartCmd)
	case "http":
		if e.Exec == nil {
			return nil, fmt.Errorf("type http requires an exec mapping")
		}
		if e.Exec.URLTemplate == "" {
			return nil, fmt.Errorf("type http requires exec.url_template")
		}
		inner = httpprovider.New(httpprovider.Config{
			Name:          name,
			Summary:       e.Summary,
			LongRun:       e.LongRun,
			Schema:        schemaJSON(e),
			HealthURL:     e.HealthURL,
			Method:        e.Exec.Method,
			URLTemplate:   e.Exec.URLTemplate,
			Headers:       e.Exec.Headers,
			BodyTemplate:  e.Exec.BodyTemplate,
			ResultPath:    e.Exec.ResultPath,
			ArtifactPaths: e.Exec.ArtifactPaths,
		})
	case "process":
		if len(e.Command) == 0 {
			return nil, fmt.Errorf("type process requires command")
		}
		inner = process.New(process.Config{
			Name:          name,
			Summary:       e.Summary,
			LongRun:       e.LongRun,
			Schema:        schemaJSON(e),
			Command:       e.Command,
			StdinJSON:     e.StdinJSON,
			ArtifactsGlob: e.ArtifactsGlob,
		})
	default:
		return nil, fmt.Errorf("unknown provider type %q (want comfyui, http or process)", e.Type)
	}

	return &configured{
		inner:       inner,
		name:        name,
		summary:     e.Summary,
		schema:      schemaJSON(e),
		longRun:     e.LongRun,
		workspace:   workspace,
		maxParallel: normalizeInt(e.MaxParallel),
		cost:        normalizeInt(e.Cost),
	}, nil
}

// Instantiate builds ONE provider directly from a spec mapping — exec
// contract v1.1 "spec mode" (board T-044). The node resolves its registry
// entry, copies it into the exec request body under `spec`, and the daemon
// dispatches straight onto the type runner WITHOUT a registry lookup.
//
// The spec carries the SAME snake_case fields as the providers.yaml
// entry (type, command, stdin_json, exec.url_template, url, workspace, …).
// JSON is a YAML subset, so the mapping decodes through the regular
// entry schema: unknown keys are dropped (never invented), absent
// optional fields stay absent (no defaults at this layer). `name` is the
// routing label from the URL — a spec-level name is overridden, never
// merged. Mis-shaped specs (unknown type, missing required fields,
// unsafe workspace) produce an error the caller maps to 422.
func Instantiate(name string, spec map[string]any) (provider.Provider, error) {
	if !provider.ValidCapName(name) {
		return nil, fmt.Errorf("capability name %q does not match %s", name, provider.CapNamePattern)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encoding spec: %w", err)
	}
	var e entry
	if err := yaml.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("spec is not a valid provider entry: %w", err)
	}
	e.Name = name
	return instantiate(name, e)
}

// Load parses the providers file and instantiates one provider.Provider
// per entry. See the package comment for the missing/empty/invalid
// semantics.
func Load(path string) ([]provider.Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading providers file %s: %w", path, err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("parsing providers file %s: %w", path, err)
	}

	var provs []provider.Provider
	seen := make(map[string]bool, len(fc.Providers))
	for i, e := range fc.Providers {
		where := fmt.Sprintf("providers[%d]", i)
		name, err := capabilityName(e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s: capability %q registered twice", where, name)
		}
		seen[name] = true
		// workspace / type validation + provider build live in instantiate
		// (shared with the spec-exec path, board T-044).
		p, err := instantiate(name, e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		provs = append(provs, p)
	}
	return provs, nil
}

// capabilityName resolves the manifest capability name: explicit `name`,
// defaulting to comfy.generate for the comfyui type (pre-registry
// behavior). Must match the SPEC §3 pattern.
func capabilityName(e entry) (string, error) {
	name := e.Name
	if name == "" && e.Type == "comfyui" {
		name = "comfy.generate"
	}
	if name == "" {
		return "", fmt.Errorf("missing name")
	}
	if !provider.ValidCapName(name) {
		return "", fmt.Errorf("capability name %q does not match %s", name, provider.CapNamePattern)
	}
	return name, nil
}

func schemaJSON(e entry) json.RawMessage {
	if len(e.InputSchema) == 0 {
		return nil
	}
	raw, err := json.Marshal(e.InputSchema)
	if err != nil {
		return nil
	}
	return raw
}

// configured decorates an inner provider with the registry-resolved
// capability name, summary/schema/long_run overrides and workspace
// scoping, and forwards the HealthProber interface so the server's health
// loops keep working generically.
type configured struct {
	inner     provider.Provider
	name      string
	summary   string // "" = keep the inner summary
	schema    json.RawMessage
	longRun   bool // applied as an explicit override only when true
	workspace string
	// Scheduling hints (board T-030), already normalized by Load
	// (missing/0/negative/garbage → 1). Applied to every provider type
	// centrally, so comfyui/http/process entries all carry them.
	maxParallel int
	cost        int
}

func (c *configured) Capability() provider.Capability {
	capability := c.inner.Capability()
	capability.Name = c.name
	if c.summary != "" {
		capability.Summary = c.summary
	}
	if len(c.schema) > 0 {
		capability.InputSchema = c.schema
	}
	if c.longRun {
		capability.LongRun = true
	}
	capability.MaxParallel = c.maxParallel
	capability.Cost = c.cost
	return capability
}

func (c *configured) Exec(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, workspaceDir string) (map[string]any, []provider.Artifact, *provider.ExecError) {
	if c.workspace != "" {
		workspaceDir = filepath.Join(workspaceDir, filepath.FromSlash(c.workspace))
		// Scoped workspaces are self-creating: providers may run with
		// cwd inside them before writing anything.
		if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
			return nil, nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("creating provider workspace %q: %v", c.workspace, err))
		}
	}
	return c.inner.Exec(ctx, taskID, params, timeout, workspaceDir)
}

// Unwrap exposes the inner provider (registry tests type-assert through it).
func (c *configured) Unwrap() provider.Provider { return c.inner }

// prober returns the inner provider's HealthProber, or nil.
func (c *configured) prober() provider.HealthProber {
	hp, _ := c.inner.(provider.HealthProber)
	return hp
}

func (c *configured) ProbeOnce(ctx context.Context) string {
	if hp := c.prober(); hp != nil {
		return hp.ProbeOnce(ctx)
	}
	return provider.HealthOK
}

func (c *configured) StartHealthLoop(ctx context.Context, interval time.Duration) {
	if hp := c.prober(); hp != nil {
		hp.StartHealthLoop(ctx, interval)
	}
}

func (c *configured) Health() string {
	if hp := c.prober(); hp != nil {
		return hp.Health()
	}
	return provider.HealthOK
}
