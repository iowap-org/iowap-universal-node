// Package daemonfile writes the reverse workspace channel (board T-026):
// after a successful bootstrap the daemon records its bridge endpoint as
// ``<workspace>/bridge/daemon.json`` so the node container can adopt it.
//
// Why: a container that the user re-created by hand (``docker compose up``
// without the token envs) carries no ``IOWAP_BRIDGE_TOKEN``, and the
// daemon starts existing containers as-is (DEC #6, never recreated) — the
// env-injection path can therefore not fix such a container. The shared
// workspace volume is the channel that survives every recreate (same
// pattern as the T-022 providers.yaml channel, in reverse): the daemon
// writes {url, token}, the node polls the file and writes the values into
// its node.yaml ``bridge:`` block (nodes/common/daemon_channel.py).
//
// The URL uses the established convention from the docs/README (T-005/
// T-009): the bridge API listens on 127.0.0.1:<port> on the host, and the
// container reaches the host via ``host.docker.internal`` — provided
// natively by Docker Desktop, mapped to the host gateway by the
// extra_hosts entry of the universal-node compose profile.
package daemonfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is the channel file inside <workspace>/bridge/.
const FileName = "daemon.json"

// Path returns <workspace>/bridge/daemon.json.
func Path(workspace string) string {
	return filepath.Join(workspace, "bridge", FileName)
}

// ManifestURL builds the bridge API URL as seen from inside the node
// container: host.docker.internal:<port> (loopback on the host side).
func ManifestURL(port int) string {
	return fmt.Sprintf("http://host.docker.internal:%d", port)
}

// Write atomically records {url, token} at <workspace>/bridge/daemon.json
// (temp file + rename in the same directory — a node poller can never see
// a torn file). Returns the written path; the token never appears in the
// return value, callers must not log it.
func Write(workspace, url, token string) (string, error) {
	dir := filepath.Join(workspace, "bridge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create bridge channel dir: %w", err)
	}
	payload, err := json.MarshalIndent(map[string]string{
		"url":   url,
		"token": token,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal daemon.json: %w", err)
	}
	path := Path(workspace)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write daemon.json: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename daemon.json: %w", err)
	}
	return path, nil
}
