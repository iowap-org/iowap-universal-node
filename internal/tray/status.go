// Status widget data (board T-023): the tray's default menu header shows
// the container state, the node's registration state and the versions.
package tray

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ContainerName mirrors bootstrap.ContainerName (kept identical; the tray
// must not import bootstrap to avoid an import cycle through heavier
// packages).
const ContainerName = "iowap-universal-node"

// DashboardURL is the node-side onboarding/dashboard UI (T-017/T-021).
const DashboardURL = "http://127.0.0.1:7783"

// State is the node's registration state as shown in the tray.
type State int

const (
	StateUnknown State = iota
	StateRegistered
	StateUnregistered
)

func (s State) String() string {
	switch s {
	case StateRegistered:
		return "registered"
	case StateUnregistered:
		return "not registered"
	default:
		return "unknown"
	}
}

// RegistrationState probes the node dashboard (mapped UI, 7783): the
// blank onboarding page renders a config form, the configured page does
// not. That content marker is the pragmatic registration signal — no
// container exec indirection. Any failure → StateUnknown (never crash).
func RegistrationState(ctx context.Context, hc *http.Client, dashboardURL string) State {
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dashboardURL, nil)
	if err != nil {
		return StateUnknown
	}
	resp, err := hc.Do(req)
	if err != nil {
		return StateUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return StateUnknown
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return StateUnknown
	}
	if strings.Contains(string(body), "<form") {
		return StateUnregistered
	}
	return StateRegistered
}

// StatusLines renders the tray status widget's lines (one per menu
// header row). Never panics: every component failure degrades to an
// informative line.
func StatusLines(ctx context.Context, ctrl ContainerController, containerName, dashboardURL, daemonVersion, imageDigest string) []string {
	lines := []string{}

	// Container status.
	switch running, err := ctrl.Running(ctx, containerName); {
	case err != nil:
		lines = append(lines, "Container: unknown ("+err.Error()+")")
	case running:
		lines = append(lines, "Container: running")
	default:
		lines = append(lines, "Container: stopped")
	}

	// Node registration via the dashboard page.
	switch RegistrationState(ctx, nil, dashboardURL) {
	case StateRegistered:
		lines = append(lines, "Node: registered")
	case StateUnregistered:
		lines = append(lines, "Node: not registered (open the dashboard to set up)")
	default:
		lines = append(lines, "Node: status unknown (dashboard unreachable?)")
	}

	// Versions.
	lines = append(lines, fmt.Sprintf("Daemon: %s", daemonVersion))
	if imageDigest != "" {
		lines = append(lines, fmt.Sprintf("Image: %s", imageDigest))
	}
	return lines
}
