// Package upgrade implements the tray's MANUAL node-upgrade action
// (board T-049): the vendored universal-node compose profile is pulled
// (`docker compose pull`) and the container recreated (`docker compose
// up -d`) through the daemon's existing compose pathway — the same
// blueprint the bootstrap executes, no new tooling.
//
// The sequence is atomic at the pull boundary: a pull failure returns
// before any recreate is attempted, so the running container stays
// untouched (never a "half-upgraded" state). The compose env (tokens,
// workspace) is resolved by the caller — main wires internal/tokenres,
// which returns the CURRENT container values, so the recreate never
// rotates the MCP bearer silently. No auto-upgrade in v1: the only
// trigger is the tray menu item, enabled by the tray's digest check.
package upgrade

import (
	"context"
	"fmt"
	"strings"

	"github.com/iowap-org/iowap-bridge-daemon/internal/bootstrap"
)

// Step is one compose action of an upgrade plan.
type Step string

const (
	// StepPull brings the profile's image to the remote digest
	// (`docker compose pull`).
	StepPull Step = "pull"
	// StepRecreate replaces the running container with the freshly
	// pulled image (`docker compose up -d` — an image change makes
	// compose recreate the service; named volumes, the workspace bind
	// and node.yaml state survive; MCP sessions restart with it).
	StepRecreate Step = "recreate"
)

// Plan is the upgrade action set for one digest change.
type Plan struct {
	// Image is the repository path the digests belong to.
	Image string
	// From is the digest currently pulled (local RepoDigest).
	From string
	// To is the digest the upgrade moves to (remote latest digest).
	To string
	// Steps is the ordered action set; empty means nothing to do.
	Steps []Step
}

// PlanFor is the pure upgrade decision: two matched digests → steps.
// Equal digests → an empty plan (nothing to do). A missing digest is an
// unknown state — an error, never a silent no-op (the tray shows
// "cannot check" instead of a false "up to date").
func PlanFor(imageRepo, from, to string) (Plan, error) {
	if imageRepo == "" {
		return Plan{}, fmt.Errorf("upgrade: empty image repository")
	}
	if from == "" {
		return Plan{}, fmt.Errorf("upgrade: no local digest known for %s", imageRepo)
	}
	if to == "" {
		return Plan{}, fmt.Errorf("upgrade: no remote digest for %s", imageRepo)
	}
	if from == to {
		return Plan{Image: imageRepo, From: from, To: to}, nil
	}
	return Plan{
		Image: imageRepo,
		From:  from,
		To:    to,
		Steps: []Step{StepPull, StepRecreate},
	}, nil
}

// composeArgs maps a step onto its `docker compose …` arguments.
func composeArgs(step Step) ([]string, error) {
	switch step {
	case StepPull:
		return []string{"pull"}, nil
	case StepRecreate:
		return []string{"up", "-d"}, nil // recreate on image change
	}
	return nil, fmt.Errorf("upgrade: unknown step %q", step)
}

// ComposeRunner executes one compose action for the profile with the
// caller's interpolation env. Production: CLI — the daemon's existing
// `docker compose -f <tmpfile> <args…>` pathway (bootstrap).
type ComposeRunner interface {
	Compose(ctx context.Context, env map[string]string, args ...string) (string, error)
}

// Reporter receives one line per plan event (step start, OK, failure).
// The tray shares this channel for its status line; secrets must never
// go through it (the env is compose-only).
type Reporter func(msg string)

// Logger receives the raw compose output — tool chatter stays in the
// daemon log, out of the tray.
type Logger func(msg string)

// Run executes the plan's steps in order, atomically: the sequence is
// pull → recreate and the first failure ends the run — a failed pull
// never reaches the recreate, the running container stays as-is. Each
// step's result is reported; the compose CLI output goes to the logger.
func Run(ctx context.Context, env map[string]string, plan Plan, runner ComposeRunner, report Reporter, log Logger) error {
	if runner == nil {
		return fmt.Errorf("upgrade: no compose runner")
	}
	if report == nil {
		report = func(string) {}
	}
	if log == nil {
		log = func(string) {}
	}
	if len(plan.Steps) == 0 {
		report("Node upgrade: image already current — no steps")
		return nil
	}
	for i, step := range plan.Steps {
		args, err := composeArgs(step)
		if err != nil {
			return err
		}
		report(fmt.Sprintf("Node upgrade: step %d/%d — docker compose %s (image %s → %s)",
			i+1, len(plan.Steps), step, short(plan.From), short(plan.To)))
		out, err := runner.Compose(ctx, env, args...)
		if strings.TrimSpace(out) != "" {
			log(fmt.Sprintf("docker compose %s output: %s", step, strings.TrimSpace(out)))
		}
		if err != nil {
			if step == StepPull {
				report(fmt.Sprintf("Node upgrade: pull FAILED — recreate skipped, the container stays in place: %v", err))
			} else {
				report(fmt.Sprintf("Node upgrade: recreate FAILED: %v", err))
			}
			return fmt.Errorf("docker compose %s failed: %w", step, err)
		}
		report(fmt.Sprintf("Node upgrade: %s OK", step))
	}
	return nil
}

// CLI is the production ComposeRunner: the vendored universal-node
// profile re-executed on the daemon's existing compose pathway
// (bootstrap.ExecComposeSteps — `docker compose -f <tmpfile> <args…>`).
// Profile carries the same bytes the bootstrap runs (bootstrap.Profile()).
type CLI struct {
	Profile []byte
}

// Compose runs `docker compose -f <profile> <args…>` with the env for
// the profile's ${...:-} interpolation.
func (c CLI) Compose(ctx context.Context, env map[string]string, args ...string) (string, error) {
	return bootstrap.ExecComposeSteps(ctx, c.Profile, env, args...)
}

func short(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}
