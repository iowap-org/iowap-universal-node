// Package dockerx is the docker/docker SDK-backed implementation of the
// engine interfaces the bootstrap package consumes. The client uses
// client.FromEnv — the default route to the local Docker Desktop engine
// (Windows named pipe, unix socket elsewhere).
package dockerx

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// Client talks to the local Docker Engine.
type Client struct {
	cli *client.Client
}

// New connects to the engine via the default route (client.FromEnv).
func New() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Ping verifies the engine is reachable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

// ContainerRunning reports whether the named container exists and is
// currently running.
func (c *Client) ContainerRunning(ctx context.Context, name string) (bool, error) {
	ct, exists, err := c.lookupContainer(ctx, name)
	if err != nil || !exists {
		return false, err
	}
	return ct.State == "running", nil
}

// InspectEnv returns the container's configured environment entries and
// whether the container exists. Missing container: (nil, false, nil).
// Used by internal/tokenres to reuse tokens from an existing universal-node
// container (the container IS the secret store, v0.1.1).
func (c *Client) InspectEnv(ctx context.Context, name string) ([]string, bool, error) {
	inspected, err := c.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspecting container %s: %w", name, err)
	}
	if inspected.Config == nil {
		return nil, true, nil
	}
	return inspected.Config.Env, true, nil
}

// ContainerExists reports whether a container with the given name exists
// in any state.
func (c *Client) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, exists, err := c.lookupContainer(ctx, name)
	return exists, err
}

// StartContainer starts an existing (stopped) container as-is — no recreate,
// so its env (bridge token, MCP bearer) stays untouched.
func (c *Client) StartContainer(ctx context.Context, name string) error {
	if err := c.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}
	return nil
}

// StopContainer stops the named container (tray "quit + stop container",
// board T-023).
func (c *Client) StopContainer(ctx context.Context, name string) error {
	if err := c.cli.ContainerStop(ctx, name, container.StopOptions{}); err != nil {
		return fmt.Errorf("stopping container %s: %w", name, err)
	}
	return nil
}

// ImageRepoDigests returns the pulled image's registry digests
// ("ghcr.io/org/img@sha256:…") for the tray's image update check
// (board T-023). Missing image: (nil, nil).
func (c *Client) ImageRepoDigests(ctx context.Context, imageRef string) ([]string, error) {
	inspected, _, err := c.cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspecting image %s: %w", imageRef, err)
	}
	return inspected.RepoDigests, nil
}

// lookupContainer finds the named container via the list API.
func (c *Client) lookupContainer(ctx context.Context, name string) (*types.Container, bool, error) {
	cts, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing containers: %w", err)
	}
	for i := range cts {
		for _, n := range cts[i].Names {
			if n == "/"+name || n == name {
				return &cts[i], true, nil
			}
		}
	}
	return nil, false, nil
}
