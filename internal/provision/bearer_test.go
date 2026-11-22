package provision

import (
	"strings"
	"testing"
)

func TestGenerateBearerPrefixAndLength(t *testing.T) {
	b, err := GenerateBearer()
	if err != nil {
		t.Fatalf("GenerateBearer: %v", err)
	}
	if !strings.HasPrefix(b, BearerPrefix) {
		t.Fatalf("bearer %q lacks prefix %q", b, BearerPrefix)
	}
	// 32 random bytes → 43 unpadded base64url characters.
	if got := len(b) - len(BearerPrefix); got != 43 {
		t.Fatalf("bearer payload length = %d, want 43", got)
	}
}

func TestGenerateBearerUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 500; i++ {
		b, err := GenerateBearer()
		if err != nil {
			t.Fatalf("GenerateBearer: %v", err)
		}
		if _, dup := seen[b]; dup {
			t.Fatalf("duplicate bearer after %d generations: %q", i, b)
		}
		seen[b] = struct{}{}
	}
}

// GenerateBridgeToken uses the same crypto/rand logic (v0.1.1 token
// auto-provisioning): identifiable prefix, 256 bit entropy, unique.
func TestGenerateBridgeToken(t *testing.T) {
	b, err := GenerateBridgeToken()
	if err != nil {
		t.Fatalf("GenerateBridgeToken: %v", err)
	}
	if !strings.HasPrefix(b, BridgeTokenPrefix) {
		t.Fatalf("bridge token %q lacks prefix %q", b, BridgeTokenPrefix)
	}
	if got := len(b) - len(BridgeTokenPrefix); got != 43 {
		t.Fatalf("bridge token payload length = %d, want 43", got)
	}
	other, err := GenerateBridgeToken()
	if err != nil || other == b {
		t.Fatalf("bridge tokens must be unique: %q vs %q (%v)", b, other, err)
	}
	if strings.HasPrefix(b, BearerPrefix) {
		t.Fatal("bridge token must not collide with the MCP bearer prefix")
	}
}

