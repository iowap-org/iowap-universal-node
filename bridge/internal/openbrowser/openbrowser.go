// Package openbrowser opens a URL in the default browser — the last step
// of the bootstrap (board T-018, Ronny): after a successful bootstrap the
// daemon hands off to the node-side onboarding UI. Best-effort by design:
// callers treat any error as "print the URL instead", never fatal.
package openbrowser

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Runner abstracts the command launch (injected for tests).
type Runner func(name string, args ...string) error

// realRun starts a detached process without waiting for it.
func realRun(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// Command returns the OS-specific browser-open command for the given GOOS:
// Windows uses rundll32 url.dll,FileProtocolHandler, macOS `open`, anything
// else xdg-open (Linux/BSD).
func Command(goos, url string) (string, []string) {
	switch goos {
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		return "open", []string{url}
	default:
		return "xdg-open", []string{url}
	}
}

// OpenWith opens url on the given GOOS via run.
func OpenWith(run Runner, goos, url string) error {
	if url == "" {
		return fmt.Errorf("openbrowser: empty url")
	}
	name, args := Command(goos, url)
	if err := run(name, args...); err != nil {
		return fmt.Errorf("openbrowser: %s: %w", name, err)
	}
	return nil
}

// Open opens url in the default browser on the current OS. Best-effort:
// the error is informational — the caller prints the URL instead and
// continues (never crashes over a missing browser).
func Open(url string) error {
	return OpenWith(realRun, runtime.GOOS, url)
}
