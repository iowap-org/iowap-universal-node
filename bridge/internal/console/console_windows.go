//go:build windows

package console

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Show makes the console window appear exactly when debug is requested
// (windowsgui release build: AllocConsole + std handle redirect). Without
// debug it is a no-op — the windowsgui subsystem never had a console, so
// there is nothing to hide and nothing flashes.
//
// On a console-subsystem dev build (plain `go build` without
// -H=windowsgui) the process already has a console; AllocConsole then
// fails harmlessly and the handles are still re-pointed, so --debug works
// identically for local builds.
func Show(debug bool) {
	if !debug {
		return
	}
	// Detach from any inherited console first (no-op when none), then
	// allocate a fresh one. Errors are non-fatal: the daemon still logs
	// to whatever handles exist.
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	free := kernel32.NewProc("FreeConsole")
	alloc := kernel32.NewProc("AllocConsole")
	free.Call() //nolint:errcheck // no inherited console in a windowsgui process
	if r, _, err := alloc.Call(); r == 0 {
		// Failure includes "already attached" (console-subsystem dev
		// build) — fine there; report anything else but keep going.
		fmt.Fprintln(os.Stderr, "iowap-bridge: debug console:", err)
	}
	redirect("CONIN$", "stdin")
	redirect("CONOUT$", "stdout")
	redirect("CONOUT$", "stderr")
}

// redirect re-points one std handle at the console device.
func redirect(device, name string) {
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(device),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "iowap-bridge: debug console: cannot open", device, err)
		return
	}
	f := os.NewFile(uintptr(h), name)
	switch name {
	case "stdin":
		os.Stdin = f
	case "stdout":
		os.Stdout = f
	case "stderr":
		os.Stderr = f
	}
}
