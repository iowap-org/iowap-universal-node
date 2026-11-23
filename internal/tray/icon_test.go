package tray

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedIconMatchesCanonicalAsset guards against drift between the
// tray's embedded copy (internal/tray/icon.ico) and the canonical asset
// (assets/iowap.ico — also the source of the .syso exe resource). The
// embed directive cannot reference the parent dir, hence the copy + guard.
func TestEmbeddedIconMatchesCanonicalAsset(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "assets", "iowap.ico"))
	if err != nil {
		t.Fatalf("canonical asset unreadable: %v", err)
	}
	if len(trayIconICO) == 0 {
		t.Fatal("embedded icon is empty")
	}
	sum := func(b []byte) string {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	if sum(canonical) != sum(trayIconICO) {
		t.Fatalf("icon drift: assets/iowap.ico (%s) != internal/tray/icon.ico (%s) — re-copy the canonical asset",
			sum(canonical), sum(trayIconICO))
	}
	// Sanity: a real ICO starts with the ICONDIR magic and holds the
	// expected multi-size image count (16/24/32/48/64/128/256 = 7).
	if len(trayIconICO) < 6 || trayIconICO[0] != 0 || trayIconICO[1] != 0 || trayIconICO[2] != 1 || trayIconICO[3] != 0 {
		t.Fatal("embedded icon is not an ICO file")
	}
	if trayIconICO[4] != 7 {
		t.Fatalf("embedded icon has %d images, want 7 (multi-size 16–256)", trayIconICO[4])
	}
}
