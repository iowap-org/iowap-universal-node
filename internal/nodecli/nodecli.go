// Package nodecli composes and parses the existing iowap-cli commands the
// daemon runs inside the universal-node container (board DEC: onboarding is
// pure composition of existing CLI building blocks — no new node API):
//
//	iowap-cli --json relay discover   (mDNS lookup, prints the relay URL)
//	iowap-cli --json node register    (registers the node on a relay)
//
// The --json flag is the CLI's GLOBAL flag (it must precede the subcommand),
// so argv places it before "relay"/"node". Discovery prints the URL as the
// first line and exits 1 when nothing was found; register prints a JSON
// object {"ok": true, ...} and exits non-zero on failure.
package nodecli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CLI binary name inside the node container.
const CLI = "iowap-cli"

// Timeouts for the exec calls (register's HTTP timeout defaults to 15 s).
const (
	DiscoverTimeout = 8 * time.Second
	RegisterTimeout = 25 * time.Second
)

// Result is the outcome of one container exec.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Ok reports whether the command exited with status 0.
func (r Result) Ok() bool { return r.ExitCode == 0 }

// DiscoverArgv builds the argv for the relay discovery exec.
func DiscoverArgv() []string {
	return []string{CLI, "--json", "relay", "discover", "--timeout", "3"}
}

// RegisterArgv builds the argv for `node register <server> --name <name>`.
func RegisterArgv(server, name string) []string {
	return []string{CLI, "--json", "node", "register", server, "--name", name}
}

// ParseDiscoverURL extracts the relay URL from discovery stdout.
// Tolerant by design: extra log lines on stdout, trailing service hints
// and surrounding whitespace are ignored. Returns "" when no URL is found
// (the caller then leaves the form field empty for manual entry).
func ParseDiscoverURL(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}

// RegisterOutcome is the parsed result of one register attempt.
type RegisterOutcome struct {
	OK      bool
	NodeID  string
	Message string // human-readable success or failure detail
}

// ParseRegister parses the register output. Success is the JSON
// {"ok": true, "node_id": ...} line (with --json); failures print a
// diagnostic on stderr and exit non-zero.
func ParseRegister(res Result) RegisterOutcome {
	if res.Ok() {
		var payload struct {
			OK     bool   `json:"ok"`
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal([]byte(findJSONObject(res.Stdout)), &payload); err == nil && payload.OK {
			return RegisterOutcome{
				OK:      true,
				NodeID:  payload.NodeID,
				Message: fmt.Sprintf("node %q registered", payload.NodeID),
			}
		}
		// Exit 0 without parseable success JSON — treat as failure with
		// the raw output so the user sees what happened.
		return RegisterOutcome{OK: false, Message: "unexpected register output: " + firstLine(res.Stdout)}
	}
	msg := strings.TrimSpace(res.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(res.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("register failed with exit code %d", res.ExitCode)
	}
	return RegisterOutcome{OK: false, Message: firstLine(msg)}
}

// findJSONObject returns the first substring of s that starts with '{' and
// ends with the matching last '}' line — enough for single-line CLI JSON.
func findJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return s
	}
	end := strings.LastIndex(s, "}")
	if end < start {
		return s[start:]
	}
	return s[start : end+1]
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}
