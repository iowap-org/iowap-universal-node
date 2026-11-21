// Package provider defines the bridge-provider contract (SPEC §3/§5):
// every host capability is exposed by a Provider that reports a manifest
// entry and executes tasks.
package provider

import (
	"context"
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"
)

// Manifest health values (SPEC §3).
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthDown     = "down"
)

// Error codes (SPEC §5).
const (
	CodeCapabilityFailed = "capability.failed" // terminal
	CodeInvalidParams    = "invalid_params"    // terminal
	CodeProviderUnavail  = "provider.unavailable"
	CodeTimeout          = "timeout"
)

// CapNamePattern is the manifest capability name pattern (SPEC §3,
// names arrive WITHOUT the node-side "host." prefix).
var CapNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,63}$`)

// ValidCapName reports whether name matches the manifest pattern.
func ValidCapName(name string) bool {
	return CapNamePattern.MatchString(name)
}

// Artifact references a file the provider wrote under the workspace dir.
// Path is relative to the workspace root (the node reads it via the
// shared workspace volume, SPEC §5).
type Artifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ExecError is a task-level failure returned by Exec. The HTTP layer
// maps it onto a 200 "failed" response; Retryable drives the node's
// terminal-vs-requeue decision.
type ExecError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *ExecError) Error() string { return e.Code + ": " + e.Message }

// Terminal returns a terminal (non-retryable) exec error.
func Terminal(code, msg string) *ExecError {
	return &ExecError{Code: code, Message: msg}
}

// Retryable returns a retryable exec error.
func Retryable(code, msg string) *ExecError {
	return &ExecError{Code: code, Message: msg, Retryable: true}
}

// Capability is one manifest entry (SPEC §3). InputSchema is the raw
// JSON Schema passed through 1:1 — the node never re-validates.
type Capability struct {
	Name        string
	Summary     string
	InputSchema json.RawMessage
	LongRun     bool
	Health      string
}

// Provider is one host capability source.
type Provider interface {
	Capability() Capability
	// Exec runs one task. params is the raw "params" object from the
	// exec request. The provider writes any output files under
	// workspaceDir and returns their workspace-relative paths.
	Exec(ctx context.Context, taskID string, params json.RawMessage, timeout time.Duration, workspaceDir string) (map[string]any, []Artifact, *ExecError)
}

// HealthProber is optionally implemented by providers whose health is
// observed in the background (probe loop, optional process start).
type HealthProber interface {
	// ProbeOnce performs one health probe and returns the resulting
	// health value (provider.HealthOK / HealthDown).
	ProbeOnce(ctx context.Context) string
	// StartHealthLoop probes every interval until ctx is done.
	StartHealthLoop(ctx context.Context, interval time.Duration)
	// Health returns the current health value.
	Health() string
}

// SanitizeRelPath cleans a workspace-relative artifact path and rejects
// anything that could escape the workspace: absolute paths, ".."
// segments, and backslashes (Windows separators would resolve outside
// the intended tree). Returns the cleaned POSIX-style path.
func SanitizeRelPath(rel string) (string, bool) {
	if rel == "" || strings.ContainsRune(rel, '\\') {
		return "", false
	}
	if strings.ContainsRune(rel, ':') {
		return "", false // drive letters ("C:/...") and other device syntax
	}
	if path.IsAbs(rel) {
		return "", false
	}
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return cleaned, true
}
