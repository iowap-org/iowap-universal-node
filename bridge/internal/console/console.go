// Package console decides whether the daemon shows a console window
// (v0.3.1, Ronny feedback #2) and, on Windows, wires it up.
//
// Decision (option a): release builds use the windowsgui subsystem
// (release.yml, -H=windowsgui) so a double-click start NEVER shows a
// console — not even a flash, because the OS never creates one. When
// --debug is requested, Show allocates a console via AllocConsole and
// redirects the std handles to it. The alternative (console subsystem +
// ShowWindow(SW_HIDE)) was rejected: the OS creates and immediately hides
// the console, which still flashes a window on the taskbar/screen for a
// frame on some Windows versions.
//
// Debug is requested by the --debug flag (command line) or the
// IOWAP_BRIDGE_DEBUG=1 environment variable.
package console

import "strings"

// debugEnvVar is the environment variable accepted (truthy values 1/true/yes/on).
const debugEnvVar = "IOWAP_BRIDGE_DEBUG"

// Debug reports whether debug mode was requested: the literal --debug
// flag in args, or IOWAP_BRIDGE_DEBUG with a truthy value (1/true/yes/on,
// case-insensitive). Injected getenv + args keep the decision testable.
func Debug(args []string, getenv func(string) string) bool {
	for _, a := range args {
		if a == "--debug" {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(getenv(debugEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
