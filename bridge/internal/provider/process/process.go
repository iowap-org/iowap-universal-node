// Package process implements the config-driven host-command provider
// (board T-019): a local command becomes a capability — params are
// substituted into the argv templates and/or piped to stdin as JSON,
// output files matching artifacts_glob are returned as artifacts.
//
// Security (deliberate, board T-019): this is the "invoke scripts" layer —
// the daemon operator explicitly vouches for every configured command.
package process

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provider/subst"
)

const (
	// stderrTail is how much stderr is echoed in a failure message.
	stderrTail = 300
	// stdoutLimit bounds the stdout echoed in the result.
	stdoutLimit = 4096
	// maxHealthFailures before the capability reports down.
	maxHealthFailures = 3
)

// Config describes one `type: process` provider from providers.yaml.
type Config struct {
	Name    string
	Summary string
	LongRun bool
	Schema  json.RawMessage

	// Command is the argv template; every element supports {{param}} /
	// {{task_id}} substitution.
	Command []string
	// StdinJSON pipes the raw params JSON to the process stdin
	// (default true; set to false for commands that don't read stdin).
	StdinJSON *bool
	// ArtifactsGlob is a glob pattern relative to the workspace dir; every
	// match becomes an artifact (workspace-relative path).
	ArtifactsGlob string
}

// Provider spawns a host command for every exec task.
type Provider struct {
	cfg Config

	mu       sync.Mutex
	failures int
}

// New builds the provider from its config entry.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg}
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

// --- Health --------------------------------------------------------------------

// Health reports ok while the command is resolvable on the host.
func (p *Provider) Health() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures < maxHealthFailures && p.commandResolvable() {
		return provider.HealthOK
	}
	return provider.HealthDown
}

// ProbeOnce checks command resolvability and returns the resulting health.
func (p *Provider) ProbeOnce(ctx context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commandResolvable() {
		p.failures = 0
		return provider.HealthOK
	}
	p.failures++
	return p.healthLocked()
}

func (p *Provider) healthLocked() string {
	if p.failures >= maxHealthFailures || !p.commandResolvable() {
		return provider.HealthDown
	}
	return provider.HealthOK
}

// commandResolvable reports whether the configured command looks
// executable on this host. It feeds the (advisory) health field only —
// it never gates a manifest entry (T-048: health is metadata).
//
// Resolution root: this check runs with the DAEMON's process cwd — a
// relative head like `scripts/tool.py` is os.Stat'ed against the
// directory the bridge daemon was started from, NOT against the
// per-exec workspace dir (Exec only chdir's the CHILD via cmd.Dir). A
// head without any path separator resolves through the daemon's PATH
// via exec.LookPath. So a manifest entry for a relative script probes
// ok (health "ok", advisory) exactly when the file exists relative to
// the daemon cwd — if it is actually meant to live in the workspace,
// the entry still publishes and a missing script surfaces as an honest
// exec error at run time, not as a missing capability.
func (p *Provider) commandResolvable() bool {
	if len(p.cfg.Command) == 0 {
		return false
	}
	head := p.cfg.Command[0]
	if strings.ContainsRune(head, '/') || strings.ContainsRune(head, os.PathSeparator) {
		_, err := os.Stat(head)
		return err == nil
	}
	_, err := exec.LookPath(head)
	return err == nil
}

// StartHealthLoop implements provider.HealthProber.
func (p *Provider) StartHealthLoop(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if got := p.ProbeOnce(ctx); got != provider.HealthOK {
					fmt.Printf("process %s: command not resolvable (%s)\n", p.cfg.Name, p.Health())
				}
			}
		}
	}()
}

// --- Exec (SPEC §5) --------------------------------------------------------------

// Exec runs the configured command with the substituted argv, pipes the
// params JSON to stdin (unless disabled), collects the exit status and
// globs the workspace for artifacts.
func (p *Provider) Exec(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, workspaceDir string) (map[string]any, []provider.Artifact, *provider.ExecError) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vars, err := subst.Vars(taskID, params)
	if err != nil {
		return nil, nil, provider.Terminal(provider.CodeInvalidParams, err.Error())
	}

	argv := make([]string, 0, len(p.cfg.Command))
	for i, elem := range p.cfg.Command {
		expanded, err := subst.Expand(elem, vars)
		if err != nil {
			return nil, nil, provider.Terminal(provider.CodeInvalidParams, fmt.Sprintf("command[%d]: %v", i, err))
		}
		argv = append(argv, expanded)
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workspaceDir
	if p.cfg.StdinJSON == nil || *p.cfg.StdinJSON {
		cmd.Stdin = bytes.NewReader(params)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, provider.Retryable(provider.CodeTimeout, fmt.Sprintf("process %q did not finish within the task timeout", argv[0]))
		}
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, nil, provider.Retryable(provider.CodeProviderUnavail, fmt.Sprintf("starting %q: %v", argv[0], err))
		}
		return nil, nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("process %q exited %v: %s", argv[0], exitCode(err), tail(stderr.String())))
	}

	artifacts, execErr := p.collectArtifacts(workspaceDir)
	if execErr != nil {
		return nil, nil, execErr
	}

	result := map[string]any{"exit_code": 0}
	if s := strings.TrimSpace(stdout.String()); s != "" {
		result["stdout"] = truncate(s)
	}
	return result, artifacts, nil
}

func (p *Provider) collectArtifacts(workspaceDir string) ([]provider.Artifact, *provider.ExecError) {
	if p.cfg.ArtifactsGlob == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(workspaceDir, filepath.FromSlash(p.cfg.ArtifactsGlob)))
	if err != nil {
		return nil, provider.Terminal(provider.CodeCapabilityFailed, fmt.Sprintf("artifacts_glob %q: %v", p.cfg.ArtifactsGlob, err))
	}
	sort.Strings(matches)
	var artifacts []provider.Artifact
	for _, m := range matches {
		rel, err := filepath.Rel(workspaceDir, m)
		if err != nil {
			continue
		}
		artifacts = append(artifacts, provider.Artifact{
			Name: filepath.Base(rel),
			Path: filepath.ToSlash(rel),
		})
	}
	return artifacts, nil
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > stderrTail {
		return s[len(s)-stderrTail:]
	}
	return s
}

func truncate(s string) string {
	if len(s) > stdoutLimit {
		return s[:stdoutLimit]
	}
	return s
}
