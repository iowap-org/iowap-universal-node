package tray

import (
	"context"
	"net/http"
)

// App is the tray's configuration, assembled by main.go. Platform-neutral
// so the non-Windows stub compiles against the same shape.
type App struct {
	// Ctrl performs container operations (dockerx.Client).
	Ctrl ContainerController
	// DaemonVersion is the running daemon version (server.Version).
	DaemonVersion string
	// ImageRef is the universal-node image the compose profile runs.
	ImageRef string
	// RepoDigestsOf resolves the local image digests (docker inspect).
	RepoDigestsOf func(ctx context.Context, imageRef string) ([]string, error)
	// Shutdown ends the daemon after Quit was chosen. Never nil.
	Shutdown func()
	// HTTPClient is used for dashboard/update probes (nil → default).
	HTTPClient *http.Client
	// ComposeProfile is the vendored universal-node compose profile
	// (bootstrap.Profile()): the exact bytes the daemon executed at
	// bootstrap, re-executed by the upgrade action (board T-049) —
	// never a second, drifting copy.
	ComposeProfile []byte
	// UpgradeEnv resolves the compose interpolation env for the
	// recreate (board T-049): current container tokens (tokenres: env
	// override → container inspect → generate, identical to the
	// bootstrap resolution) + the workspace dir. Nil disables the
	// upgrade action. The env carries secrets — it is compose-only and
	// is never logged.
	UpgradeEnv func(ctx context.Context) (map[string]string, error)
}
