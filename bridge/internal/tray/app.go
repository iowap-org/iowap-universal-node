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
}
