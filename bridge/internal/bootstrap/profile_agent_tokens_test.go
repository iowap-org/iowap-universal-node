package bootstrap

// T-035: the vendored compose profile must carry the agent-token volume.
//
// The daemon creates the node container FROM this embedded profile — a
// profile without `iowap-agent-tokens:/data/agents` would silently ship
// nodes that keep their agent tokens on the relay-dir fallback even though
// the docker-repo profile already mounts the volume. The re-vendor
// discipline (T-016b) is enforced here: these literals are the CI-grep
// half of the contract (vendored file vs iowap-docker@9188f70).

import (
	"strings"
	"testing"
)

func TestVendoredProfileMountsAgentTokenVolume(t *testing.T) {
	profile := string(composeFile)
	if !strings.Contains(profile, "iowap-agent-tokens:/data/agents") {
		t.Fatal("vendored universal-node profile lost the T-035 agent-token volume mount (iowap-agent-tokens:/data/agents) — re-vendor from iowap-docker")
	}
	if !strings.Contains(profile, "Vendored from iowap-docker@") {
		t.Fatal("vendored profile carries no provenance comment (iowap-docker@<ref>) — the re-vendor comment is part of the contract")
	}
}

func TestVendoredProfileDeclaresTheAgentTokenVolume(t *testing.T) {
	profile := string(composeFile)
	cut := strings.LastIndex(profile, "\nvolumes:")
	if cut < 0 {
		t.Fatal("vendored profile has no volumes: declaration section")
	}
	declared := profile[cut:]
	if !strings.Contains(declared, "iowap-agent-tokens:") {
		t.Fatalf("profile consumes iowap-agent-tokens without declaring it under volumes: — got %q", declared)
	}
	// the established volumes stay untouched
	if !strings.Contains(declared, "universal-node-state:") {
		t.Fatal("vendored profile lost universal-node-state in the volumes: declaration")
	}
}
