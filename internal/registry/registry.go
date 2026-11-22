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

		workspace := ""
		if e.Workspace != "" {
			ws, ok := provider.SanitizeRelPath(e.Workspace)
			if !ok {
				return nil, fmt.Errorf("%s: workspace %q must be a safe workspace-relative path", where, e.Workspace)
			}
			workspace = ws
		}

		var inner provider.Provider
		switch e.Type {
		case "comfyui":
			if e.URL == "" {
				return nil, fmt.Errorf("%s: type comfyui requires url", where)
			}
			inner = comfyui.New(e.URL, e.StartCmd)
		case "http":
			if e.Exec == nil {
				return nil, fmt.Errorf("%s: type http requires an exec mapping", where)
			}
			if e.Exec.URLTemplate == "" {
				return nil, fmt.Errorf("%s: type http requires exec.url_template", where)
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
				return nil, fmt.Errorf("%s: type process requires command", where)
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
			return nil, fmt.Errorf("%s: unknown provider type %q (want comfyui, http or process)", where, e.Type)
		}

		provs = append(provs, &configured{
			inner:     inner,
			name:      name,
			summary:   e.Summary,
			schema:    schemaJSON(e),
			longRun:   e.LongRun,
			workspace: workspace,
		})
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
