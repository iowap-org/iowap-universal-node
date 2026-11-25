package daemonfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestURLUsesHostDockerInternal(t *testing.T) {
	got := ManifestURL(7780)
	if got != "http://host.docker.internal:7780" {
		t.Errorf("ManifestURL(7780) = %q, want http://host.docker.internal:7780", got)
	}
}

func TestWriteCreatesChannelWithExactContent(t *testing.T) {
	ws := t.TempDir()
	path, err := Write(ws, "http://host.docker.internal:7780", "iw_bridgesecret")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if path != filepath.Join(ws, "bridge", "daemon.json") {
		t.Fatalf("path = %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, raw)
	}
	if doc["url"] != "http://host.docker.internal:7780" || doc["token"] != "iw_bridgesecret" {
		t.Errorf("content = %v", doc)
	}
	// No temp file left behind (atomic rename).
	entries, _ := os.ReadDir(filepath.Join(ws, "bridge"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteOverwritesAtomically(t *testing.T) {
	ws := t.TempDir()
	if _, err := Write(ws, "http://old:1", "t1"); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := Write(ws, "http://new:2", "t2"); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	raw, _ := os.ReadFile(Path(ws))
	var doc map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("invalid json after overwrite: %v", err)
	}
	if doc["url"] != "http://new:2" || doc["token"] != "t2" {
		t.Errorf("content after overwrite = %v", doc)
	}
}

func TestWriteCreatesMissingBridgeDir(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "deep", "workspace")
	if _, err := Write(ws, "http://host.docker.internal:7780", "tok"); err != nil {
		t.Fatalf("Write into fresh workspace: %v", err)
	}
	if _, err := os.Stat(Path(ws)); err != nil {
		t.Fatalf("daemon.json missing: %v", err)
	}
}
