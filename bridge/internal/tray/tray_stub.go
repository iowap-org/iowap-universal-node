//go:build !windows

// Non-Windows stub: the tray is a Windows-first feature (board T-023).
// CI runs the test suite on Linux — the stub keeps the daemon buildable
// and CGO-free there; the real wiring is tray_windows.go (verified by
// GOOS=windows cross-builds and manual testing).
package tray

import "log"

// Run is the platform-neutral no-op tray loop.
func Run(App) {
	log.Printf("iowap-bridge: tray icon not supported on this platform — skipping")
}

// confirmDialog has no portable implementation — daemon-only quit.
func confirmDialog(title, text string) bool {
	log.Printf("iowap-bridge: %s: %s (no dialog on this platform — answering no)", title, text)
	return false
}

// alertDialog has no portable implementation on non-Windows hosts — the
// daemon log stays the only upgrade-failure surface (the tray package
// itself is a stub here anyway).
func alertDialog(title, text string) {
	log.Printf("iowap-bridge: %s: %s", title, text)
}
