// Package tray implements the Windows tray icon for the bridge daemon
// (board T-023): a status widget (container state, node registration,
// versions), the menu actions (open dashboard, restart container, update
// check, quit with container-stop confirmation) and the wiring into the
// daemon's main loop.
//
// Testability: everything except the systray wiring itself lives in
// platform-independent files behind small interfaces
// (ContainerController), so the full suite runs on Linux CI. The
// systray-driven part is Windows-only (//go:build windows) and verified
// by GOOS=windows cross-builds + manual testing (board decision: no
// GUI self-tests).
package tray

import (
	"context"
	"errors"
	"fmt"
)

// ContainerController is the container operations surface the tray needs
// (implemented by internal/dockerx.Client; faked in tests).
type ContainerController interface {
	// Running reports whether the named container is currently running.
	Running(ctx context.Context, name string) (bool, error)
	// Stop stops the named container.
	Stop(ctx context.Context, name string) error
	// Start starts an existing container as-is (no recreate — consistent
	// with the bootstrap's existing-container path, board DEC #6: no
	// bearer rotation, saved agent configs survive).
	Start(ctx context.Context, name string) error
}

// RestartContainer restarts the universal-node container: stop, then
// start as-is. Consistent with the bootstrap (T-012): the container env —
// and with it the MCP bearer — stays untouched.
func RestartContainer(ctx context.Context, ctrl ContainerController, name string) error {
	if ctrl == nil {
		return errors.New("tray: no container controller")
	}
	running, err := ctrl.Running(ctx, name)
	if err != nil {
		return fmt.Errorf("checking %s: %w", name, err)
	}
	if running {
		if err := ctrl.Stop(ctx, name); err != nil {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
	}
	if err := ctrl.Start(ctx, name); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return nil
}

// QuitAction is what the user chose in the quit confirmation.
type QuitAction int

const (
	// QuitDaemonOnly exits the daemon, leaves the container running.
	QuitDaemonOnly QuitAction = iota
	// QuitWithContainerStop exits the daemon AND stops the node container.
	QuitWithContainerStop
)

// DecideQuit maps the confirmation answer onto the action. Pure function
// so the decision table is testable without a GUI.
func DecideQuit(stopContainer bool) QuitAction {
	if stopContainer {
		return QuitWithContainerStop
	}
	return QuitDaemonOnly
}

// HandleQuit runs the quit flow: ask for confirmation (the platform
// confirm dialog; the injected func keeps this testable), stop the
// container when the user chose "yes", and always return the action —
// the daemon quits even when the container stop fails (the failure is
// reported by the caller).
func HandleQuit(confirm func() bool, ctrl ContainerController, name string) QuitAction {
	if confirm == nil || !confirm() {
		return QuitDaemonOnly
	}
	if ctrl != nil {
		_ = ctrl.Stop(context.Background(), name)
	}
	return QuitWithContainerStop
}

// HandleUpgrade runs the node upgrade's confirm gate (the T-023 quit
// pattern, board T-049): the compose actions run ONLY after a confirmed
// dialog — cancelling performs NO action (no pull, no recreate, the
// running container is untouched). Confirm is injected like for quit so
// the gate is testable without a GUI; run is the caller's whole upgrade
// (digest plan + compose steps); its error is returned for the status
// line. ran reports whether the upgrade actually ran.
func HandleUpgrade(confirm func() bool, run func(ctx context.Context) error) (ran bool, err error) {
	if confirm == nil || !confirm() || run == nil {
		return false, nil
	}
	return true, run(context.Background())
}
