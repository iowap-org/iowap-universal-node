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

func TestHashBearerIsSha256Hex(t *testing.T) {
	h := HashBearer("iw_mcp_test")
	if len(h) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(h))
	}
	if h != HashBearer("iw_mcp_test") {
		t.Fatal("hash must be deterministic")
	}
	if h == HashBearer("iw_mcp_test2") {
		t.Fatal("different bearers must hash differently")
	}
}
