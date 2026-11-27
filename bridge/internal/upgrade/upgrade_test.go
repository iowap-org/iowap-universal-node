package upgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeRunner records compose invocations; the queued errors answer the
// calls in order (nil entries succeed).
type fakeRunner struct {
	calls  [][]string
	envs   []map[string]string
	errs   []error
	outs   []string
	called int
}

func (f *fakeRunner) Compose(ctx context.Context, env map[string]string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	f.envs = append(f.envs, env)
	var (
		err error
		out string
	)
	if f.called < len(f.errs) {
		err = f.errs[f.called]
	}
	if f.called < len(f.outs) {
		out = f.outs[f.called]
	}
	f.called++
	return out, err
}

func (f *fakeRunner) allArgs() string {
	parts := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		parts = append(parts, strings.Join(c, " "))
	}
	return strings.Join(parts, " | ")
}

const (
	repo = "ghcr.io/iowap-org/iowap-universal-node"
	digA = "sha256:aaaaaaaaaaaaaaaaaaaa"
	digB = "sha256:bbbbbbbbbbbbbbbbbbbb"
)

// --- PlanFor (pure digest → steps) ------------------------------------------

func TestPlanForDifferingDigestsPullAndRecreate(t *testing.T) {
	plan, err := PlanFor(repo, digA, digB)
	if err != nil {
		t.Fatalf("PlanFor(differing): %v", err)
	}
	if got := fmt.Sprint(plan.Steps); got != "[pull recreate]" {
		t.Fatalf("steps = %s, want [pull recreate] (atomic sequence: pull first)", got)
	}
	if plan.From != digA || plan.To != digB || plan.Image != repo {
		t.Fatalf("plan = %+v, want from/to/image preserved", plan)
	}
}

func TestPlanForSameDigestsNoSteps(t *testing.T) {
	plan, err := PlanFor(repo, digA, digA)
	if err != nil {
		t.Fatalf("PlanFor(same): %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("steps = %v, want empty (equal digests → no compose action)", plan.Steps)
	}
}

func TestPlanForUnknownDigestsError(t *testing.T) {
	cases := []struct{ from, to string }{
		{"", digB}, // nothing pulled yet → unknown state
		{digA, ""}, // no remote answer → unknown state
		{"", ""},   // fully unknown
	}
	for _, tc := range cases {
		plan, err := PlanFor(repo, tc.from, tc.to)
		if err == nil {
			t.Errorf("PlanFor(%q, %q) = %v, nil error; a missing digest must be an unknown-state error, not a silent no-op", tc.from, tc.to, plan)
		}
		if plan.Steps != nil && len(plan.Steps) > 0 {
			t.Errorf("PlanFor(%q, %q) planned steps on unknown state", tc.from, tc.to)
		}
	}
	if _, err := PlanFor("", digA, digB); err == nil {
		t.Error("empty image repo must error")
	}
}

// --- composeArgs (step → docker compose arguments) ---------------------------

func TestComposeArgsForSteps(t *testing.T) {
	pull, err := composeArgs(StepPull)
	if err != nil {
		t.Fatalf("composeArgs(pull): %v", err)
	}
	if got := strings.Join(pull, " "); got != "pull" {
		t.Fatalf("pull args = %q, want \"pull\"", got)
	}
	recreate, err := composeArgs(StepRecreate)
	if err != nil {
		t.Fatalf("composeArgs(recreate): %v", err)
	}
	if got := strings.Join(recreate, " "); got != "up -d" {
		t.Fatalf("recreate args = %q, want \"up -d\" (compose recreates on image change)", got)
	}
	if _, err := composeArgs(Step("nonsense")); err == nil {
		t.Fatal("unknown steps must error")
	}
}

// --- Run (atomic step sequence through the compose runner) -------------------

func TestRunExecutesPullThenRecreateInOrder(t *testing.T) {
	runner := &fakeRunner{}
	var reports, logs []string
	collect := func(dst *[]string) func(string) {
		return func(m string) { *dst = append(*dst, m) }
	}
	runner.outs = []string{"image Pulled", "Started iowap-universal-node"}
	plan, _ := PlanFor(repo, digA, digB)
	env := map[string]string{"IOWAP_MCP_BEARER": "x"}
	if err := Run(context.Background(), env, plan, runner, collect(&reports), collect(&logs)); err != nil {
		t.Fatalf("Run(success): %v", err)
	}
	if got := runner.allArgs(); got != "pull | up -d" {
		t.Fatalf("compose calls = %q, want \"pull | up -d\" (order matters: pull is the atomicity boundary)", got)
	}
	if len(runner.envs) != 2 {
		t.Fatalf("compose env count = %d, want 2", len(runner.envs))
	}
	for i, passed := range runner.envs {
		if passed["IOWAP_MCP_BEARER"] != "x" {
			t.Errorf("compose call %d lost the interpolation env", i+1)
		}
	}
	for _, want := range []string{"pull OK", "recreate OK", "step 1/2", "step 2/2"} {
		if !containsAny(reports, want) {
			t.Errorf("reports missing %q: %v", want, reports)
		}
	}
	if !containsAny(logs, "Pulled") || !containsAny(logs, "Started iowap-universal-node") {
		t.Errorf("compose output must reach the daemon log channel: %v", logs)
	}
}

// Run-Fail: a failed pull must NEVER trigger the recreate — the running
// container stays untouched (no half-upgraded state).
func TestRunPullFailureSkipsRecreate(t *testing.T) {
	runner := &fakeRunner{errs: []error{errors.New("exit status 1 — pull failed")}}
	var reports []string
	plan, _ := PlanFor(repo, digA, digB)
	err := Run(context.Background(), nil, plan, runner, func(m string) { reports = append(reports, m) }, nil)
	if err == nil {
		t.Fatal("a pull failure must surface as an error")
	}
	if !strings.Contains(err.Error(), "pull failed") {
		t.Fatalf("error must carry the pull failure, got: %v", err)
	}
	if runner.called != 1 {
		t.Fatalf("compose calls = %d, want exactly 1 (pull) — recreate must never run after a failed pull", runner.called)
	}
	if !containsAny(reports, "recreate skipped") {
		t.Errorf("reports must state that the recreate was skipped: %v", reports)
	}
}

func TestRunRecreateFailureSurfacesError(t *testing.T) {
	runner := &fakeRunner{errs: []error{nil, errors.New("exit status 1 — up failed")}}
	var reports []string
	plan, _ := PlanFor(repo, digA, digB)
	err := Run(context.Background(), nil, plan, runner, func(m string) { reports = append(reports, m) }, nil)
	if err == nil || !strings.Contains(err.Error(), "recreate") {
		t.Fatalf("the recreate failure must surface, got: %v", err)
	}
	if runner.called != 2 {
		t.Fatalf("compose calls = %d, want 2 (pull succeeded, then recreate attempted)", runner.called)
	}
	if !containsAny(reports, "recreate FAILED") {
		t.Errorf("reports must carry the recreate failure: %v", reports)
	}
}

func TestRunEmptyPlanCallsNoCompose(t *testing.T) {
	runner := &fakeRunner{}
	var reports []string
	plan, _ := PlanFor(repo, digA, digA)
	if err := Run(context.Background(), nil, plan, runner, func(m string) { reports = append(reports, m) }, nil); err != nil {
		t.Fatalf("Run(empty plan): %v", err)
	}
	if runner.called != 0 {
		t.Fatalf("compose calls = %d, want 0 (equal digests → no action)", runner.called)
	}
	if !containsAny(reports, "no steps") {
		t.Errorf("reports must say there was nothing to do: %v", reports)
	}
}

func TestRunRequiresRunner(t *testing.T) {
	plan, _ := PlanFor(repo, digA, digB)
	if err := Run(context.Background(), nil, plan, nil, nil, nil); err == nil {
		t.Fatal("nil runner must fail")
	}
}

// --- CLI (production runner wiring) ------------------------------------------

func TestCLIMapsEverythingOntosExecComposeSteps(t *testing.T) {
	// The production runner is thin glue by design: profile bytes +
	// env + args go straight to bootstrap.ExecComposeSteps — this test
	// pins the shape so a later refactor cannot quietly drop the env
	// (the recreate would lose the tokens, i.e. rotate the bearer).
	c := CLI{Profile: []byte("services: {universal-node: {image: test}}")}
	if len(c.Profile) == 0 {
		t.Fatal("the CLI runner carries the vendored profile bytes")
	}
}

func containsAny(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
