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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
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
	cts, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return false, fmt.Errorf("listing containers: %w", err)
	}
	for _, ct := range cts {
		for _, n := range ct.Names {
			if n == "/"+name || n == name {
				return ct.State == "running", nil
			}
		}
	}
	return false, nil
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
