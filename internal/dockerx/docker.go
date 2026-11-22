// Package dockerx is the docker/docker SDK-backed implementation of the
// engine/exec interfaces the bootstrap and web UI packages consume. The
// client uses client.FromEnv — the default route to the local Docker
// Desktop engine (Windows named pipe, unix socket elsewhere).
package dockerx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/iowap-org/iowap-bridge-daemon/internal/nodecli"
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

// Exec runs a command inside the container and returns the demultiplexed
// stdout/stderr plus the exit code.
func (c *Client) Exec(ctx context.Context, name string, cmd []string, timeout time.Duration) (nodecli.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	created, err := c.cli.ContainerExecCreate(ctx, name, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nodecli.Result{}, fmt.Errorf("exec create in %s: %w", name, err)
	}

	resp, err := c.cli.ContainerExecAttach(ctx, created.ID, container.ExecStartOptions{})
	if err != nil {
		return nodecli.Result{}, fmt.Errorf("exec start in %s: %w", name, err)
	}
	defer resp.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, resp.Reader); err != nil && err != io.EOF {
		return nodecli.Result{}, fmt.Errorf("reading exec output: %w", err)
	}

	inspected, err := c.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return nodecli.Result{}, fmt.Errorf("exec inspect in %s: %w", name, err)
	}
	return nodecli.Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: inspected.ExitCode,
	}, nil
}
