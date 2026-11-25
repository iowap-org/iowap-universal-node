//go:build !windows

package console

// Show is a no-op on non-Windows: there is no windowsgui subsystem and no
// console window to show or hide; logs go to stderr (and daemon.log, see
// internal/logging).
func Show(debug bool) {
	_ = debug
}
