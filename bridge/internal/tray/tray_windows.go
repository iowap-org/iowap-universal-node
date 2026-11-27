//go:build windows

// Windows tray wiring (board T-023): fyne.io/systray (maintained fork of
// getlantern/systray, pure Go on Windows — no CGO). The daemon's HTTP
// server runs independently; tray errors must never crash the daemon —
// main.go guards Run with a recover, and every action here degrades to a
// status line on failure.
package tray

import (
	"context"
	"fmt"
	"log"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"

	"github.com/iowap-org/iowap-bridge-daemon/internal/openbrowser"
	"github.com/iowap-org/iowap-bridge-daemon/internal/upgrade"
)

// Run blocks in the systray event loop. Call from a goroutine.
func Run(app App) {
	icon, err := iconBytes()
	if err != nil {
		log.Printf("iowap-bridge: tray: icon unavailable (%v) — tray disabled", err)
		return
	}
	systray.Run(func() { onReady(app, icon) }, func() { onExit(app) })
}

func onReady(app App, icon []byte) {
	systray.SetIcon(icon)
	systray.SetTooltip("iowap-bridge")
	systray.SetTitle("iowap-bridge")

	// Status widget: disabled header lines, refreshed by a ticker (and
	// manually before each relevant action) — a left-click opens the menu
	// and shows the latest snapshot.
	statusItems := make([]*systray.MenuItem, 0, 5)
	for range 5 {
		item := systray.AddMenuItem("…", "iowap-bridge status")
		item.Disable()
		statusItems = append(statusItems, item)
	}
	systray.AddSeparator()

	mDashboard := systray.AddMenuItem("Open Dashboard", "Open the node dashboard in your browser")
	mRestart := systray.AddMenuItem("Restart docker container", "Restart the universal-node container (tokens preserved)")
	mUpdate := systray.AddMenuItem("Check for updates", "Check the container image and this exe for updates")
	mUpgrade := systray.AddMenuItem("Node upgrade…", "Pull the latest universal-node image and recreate the container (asks first; registration, tokens and workspace survive)")
	// Board T-049: the action exists from this exe on, enabled only when
	// the digest diff is confirmed (check at tray start + on every
	// manual "Check for updates" + after a finished upgrade — no remote
	// polling in the refresh ticker).
	mUpgrade.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit the daemon (asks whether to stop the node container too)")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	refresh := func() { refreshStatus(ctx, app, statusItems) }
	refresh()
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()

	// Click handlers: systray v1.12 exposes ClickedCh channels — one
	// consuming goroutine per item (errors inside are logged, never
	// propagated to the daemon).
	go func() {
		for range mDashboard.ClickedCh {
			_ = openbrowser.Open(DashboardURL)
		}
	}()
	go func() {
		for range mRestart.ClickedCh {
			clickRestart(ctx, app, statusItems)
		}
	}()
	go func() {
		for range mUpdate.ClickedCh {
			clickUpdate(ctx, app, statusItems, mUpgrade)
		}
	}()
	go func() {
		for range mUpgrade.ClickedCh {
			clickUpgrade(ctx, app, statusItems, mUpgrade)
		}
	}()
	go func() {
		for range mQuit.ClickedCh {
			clickQuit(app)
		}
	}()

	// One registry check at tray start decides the upgrade menu state:
	// pending diff → the item enables; every other state keeps it
	// disabled (fail-closed: an unknown state never enables the action).
	go func() {
		pending, err := checkUpgradePending(ctx, app)
		switch {
		case err != nil:
			log.Printf("iowap-bridge: tray: upgrade menu stays disabled (%v)", err)
		case pending:
			mUpgrade.Enable()
		}
	}()
}

// refreshStatus updates the disabled header lines with a fresh snapshot.
func refreshStatus(ctx context.Context, app App, items []*systray.MenuItem) {
	var digest string
	if app.RepoDigestsOf != nil {
		if digests, err := app.RepoDigestsOf(ctx, app.ImageRef); err == nil {
			digest = shortenDigest(localImageDigest(digestRepo(app.ImageRef), digests))
		}
	}
	lines := StatusLines(ctx, app.Ctrl, ContainerName, DashboardURL, app.DaemonVersion, digest)
	for i, item := range items {
		if i < len(lines) {
			item.SetTitle(lines[i])
		} else {
			item.SetTitle("")
		}
	}
	if len(lines) > 0 {
		systray.SetTooltip(joinLines(lines))
	}
}

func clickRestart(ctx context.Context, app App, items []*systray.MenuItem) {
	if err := RestartContainer(ctx, app.Ctrl, ContainerName); err != nil {
		setStatus(items, 1, "Restart failed: "+err.Error())
		return
	}
	setStatus(items, 1, "Container restarted "+time.Now().Format("15:04:05"))
	refreshStatus(ctx, app, items)
}

// clickUpdate runs both update checks and reports via the status line
// (v1 is informational only for the EXE — nothing is downloaded). The
// image check also flips the upgrade menu's enable state (board T-049).
func clickUpdate(ctx context.Context, app App, items []*systray.MenuItem, mUpgrade *systray.MenuItem) {
	setStatus(items, 1, "Update check…")

	report := func(line string) { setStatus(items, 1, line); log.Printf("iowap-bridge: tray: %s", line) }

	// (a) Container image digest comparison.
	imageLine := "Image: check failed"
	if app.RepoDigestsOf != nil {
		local, err := app.RepoDigestsOf(ctx, app.ImageRef)
		if err != nil {
			imageLine = "Image: check failed (" + err.Error() + ")"
		} else if remote, err := RemoteImageDigest(ctx, app.HTTPClient, app.ImageRef); err != nil {
			imageLine = "Image: check failed (" + err.Error() + ")"
		} else if upd, err := UpgradePending(digestRepo(app.ImageRef), local, remote); err != nil {
			imageLine = "Image: unknown state (" + err.Error() + ")"
			// Fail-closed: an unknown state never enables the action.
			mUpgrade.Disable()
		} else if upd {
			imageLine = "Image: UPDATE AVAILABLE"
			mUpgrade.Enable()
		} else {
			imageLine = "Image: up to date"
			mUpgrade.Disable()
		}
	}

	// (b) Daemon exe vs. GitHub latest release.
	exeLine := "exe: check failed"
	if tag, err := LatestReleaseTag(ctx, app.HTTPClient, GitHubRepo); err != nil {
		exeLine = "exe: check failed (" + err.Error() + ")"
	} else if upd, err := UpdateAvailable(app.DaemonVersion, tag); err != nil {
		exeLine = "exe: unknown state (" + err.Error() + ")"
	} else if upd {
		exeLine = "exe: UPDATE AVAILABLE (" + tag + ")"
	} else {
		exeLine = "exe: up to date"
	}

	report("Update: " + imageLine + " — " + exeLine)
}

// checkUpgradePending runs the fresh digest check behind the upgrade
// gate: the local repo digests vs. the registry's current digest.
func checkUpgradePending(ctx context.Context, app App) (bool, error) {
	if app.RepoDigestsOf == nil {
		return false, fmt.Errorf("no image inspector configured")
	}
	local, err := app.RepoDigestsOf(ctx, app.ImageRef)
	if err != nil {
		return false, err
	}
	remote, err := RemoteImageDigest(ctx, app.HTTPClient, app.ImageRef)
	if err != nil {
		return false, err
	}
	return UpgradePending(digestRepo(app.ImageRef), local, remote)
}

// clickUpgrade is the manual node upgrade (board T-049, T-023 quit
// pattern): fresh digest check → confirm dialog (registration, agent
// tokens and the workspace survive the recreate; running MCP sessions
// are interrupted) → atomic pull+recreate steps on the SAME compose
// execution path the bootstrap uses. A failed pull never reaches the
// recreate — the running container stays untouched.
func clickUpgrade(ctx context.Context, app App, items []*systray.MenuItem, mUpgrade *systray.MenuItem) {
	report := func(line string) { setStatus(items, 1, line); log.Printf("iowap-bridge: tray: %s", line) }

	if app.UpgradeEnv == nil || len(app.ComposeProfile) == 0 || app.RepoDigestsOf == nil {
		report("Node upgrade: not available in this daemon (no compose profile / token resolver)")
		return
	}

	report("Node upgrade: checking image state…")
	local, err := app.RepoDigestsOf(ctx, app.ImageRef)
	if err != nil {
		report("Node upgrade: image check failed (" + err.Error() + ")")
		return
	}
	remote, err := RemoteImageDigest(ctx, app.HTTPClient, app.ImageRef)
	if err != nil {
		report("Node upgrade: image check failed (" + err.Error() + ")")
		return
	}
	pending, err := UpgradePending(digestRepo(app.ImageRef), local, remote)
	switch {
	case err != nil:
		report("Node upgrade: unknown state (" + err.Error() + ")")
		return
	case !pending:
		mUpgrade.Disable()
		report("Node upgrade: image up to date — nothing to do")
		return
	}
	mUpgrade.Enable()

	ran, err := HandleUpgrade(
		func() bool {
			return confirmDialog(
				"Upgrade the node container?",
				"Pull the latest universal-node image and recreate the container?\n\n"+
					"Registration, agent tokens and the workspace survive the recreate.\n"+
					"Running MCP sessions will be interrupted.\n\nContinue?",
			)
		},
		func(ctx context.Context) error {
			env, err := app.UpgradeEnv(ctx)
			if err != nil {
				return fmt.Errorf("resolving container tokens for the recreate: %w", err)
			}
			plan, err := upgrade.PlanFor(digestRepo(app.ImageRef),
				localImageDigest(digestRepo(app.ImageRef), local), remote)
			if err != nil {
				return err
			}
			runner := upgrade.CLI{Profile: app.ComposeProfile}
			return upgrade.Run(
				ctx, env, plan, runner,
				func(msg string) { report(msg) },
				func(msg string) { log.Printf("iowap-bridge: upgrade: %s", msg) },
			)
		},
	)
	if !ran {
		// The confirm gate answered no: no compose call happened.
		report("Node upgrade cancelled — nothing changed")
		return
	}
	if err != nil {
		report("Node upgrade FAILED: " + err.Error())
		alertDialog("Upgrade failed",
			err.Error()+"\n\nA failed pull left the running container in place —"+
				" the daemon log has the details.")
		return
	}

	// Success: the fresh local digest is the upgrade proof; the menu
	// flips off unless the registry already moved on again.
	if fresh, err := app.RepoDigestsOf(ctx, app.ImageRef); err != nil {
		report("Node upgrade done (digest unknown: " + err.Error() + ")")
	} else {
		if pending, err := UpgradePending(digestRepo(app.ImageRef), fresh, remote); err == nil {
			if pending {
				mUpgrade.Enable()
			} else {
				mUpgrade.Disable()
			}
		}
		report("Node upgrade done — image now " + shortenDigest(localImageDigest(digestRepo(app.ImageRef), fresh)))
	}
	refreshStatus(ctx, app, items)
}

func clickQuit(app App) {
	action := HandleQuit(
		func() bool {
			return confirmDialog("Quit iowap-bridge", "Stop the node container too?")
		},
		app.Ctrl,
		ContainerName,
	)
	systray.Quit()
	if action == QuitWithContainerStop && app.Shutdown != nil {
		app.Shutdown()
	}
}

// onExit cleans up after the systray loop ended.
func onExit(app App) {
	if app.Shutdown != nil {
		app.Shutdown()
	}
}

func setStatus(items []*systray.MenuItem, idx int, line string) {
	if idx < len(items) {
		items[idx].SetTitle(line)
	}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += " | "
		}
		out += l
	}
	return out
}

// digestRepo returns the repo path of "host/repo:tag".
func digestRepo(imageRef string) string {
	for i := 0; i < len(imageRef); i++ {
		if imageRef[i] == ':' {
			return imageRef[:i]
		}
	}
	return imageRef
}

func shortenDigest(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// iconBytes returns the embedded tray icon.
func iconBytes() ([]byte, error) {
	if len(trayIconICO) == 0 {
		return nil, fmt.Errorf("embedded icon is empty")
	}
	return trayIconICO, nil
}

// confirmDialog shows the Windows MessageBox YES/NO dialog.
func confirmDialog(title, text string) bool {
	const (
		mbYesNo     = 0x04
		mbIconQuest = 0x20
		idYes       = 6
	)
	return windowsMessageBox(text, title, mbYesNo|mbIconQuest) == idYes
}

// alertDialog shows the Windows MessageBox OK dialog — the upgrade
// failure surface (board T-049): the per-step chatter lives in the tray
// status + daemon log, the clean failure message is the modal.
func alertDialog(title, text string) {
	const (
		mbOK      = 0x00
		mbIconErr = 0x10
	)
	windowsMessageBox(text, title, mbOK|mbIconErr)
}

func windowsMessageBox(text, title string, flags int) uintptr {
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	titlePtr, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return 0
	}
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return 0
	}
	ret, _, _ := proc.Call(0, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), uintptr(flags))
	return ret
}
