package tray

import (
	"context"
	"errors"
	"testing"
)

// fakeController records operations in order.
type fakeController struct {
	ops      []string
	running  map[string]bool
	runningE error
	stopE    error
	startE   error
}

func (f *fakeController) Running(ctx context.Context, name string) (bool, error) {
	f.ops = append(f.ops, "running")
	if f.runningE != nil {
		return false, f.runningE
	}
	return f.running[name], nil
}

func (f *fakeController) Stop(ctx context.Context, name string) error {
	f.ops = append(f.ops, "stop:"+name)
	return f.stopE
}

func (f *fakeController) Start(ctx context.Context, name string) error {
	f.ops = append(f.ops, "start:"+name)
	return f.startE
}

func TestRestartContainerRunningStopsThenStarts(t *testing.T) {
	ctrl := &fakeController{running: map[string]bool{"node": true}}
	if err := RestartContainer(context.Background(), ctrl, "node"); err != nil {
		t.Fatalf("RestartContainer: %v", err)
	}
	want := []string{"running", "stop:node", "start:node"}
	if len(ctrl.ops) != len(want) {
		t.Fatalf("ops = %v, want %v", ctrl.ops, want)
	}
	for i := range want {
		if ctrl.ops[i] != want[i] {
			t.Fatalf("ops = %v, want %v (order matters: restart = stop + as-is start, consistent with bootstrap)", ctrl.ops, want)
		}
	}
}

func TestRestartContainerStoppedOnlyStarts(t *testing.T) {
	ctrl := &fakeController{running: map[string]bool{"node": false}}
	if err := RestartContainer(context.Background(), ctrl, "node"); err != nil {
		t.Fatalf("RestartContainer: %v", err)
	}
	want := []string{"running", "start:node"}
	if len(ctrl.ops) != len(want) {
		t.Fatalf("ops = %v, want %v", ctrl.ops, want)
	}
	for i := range want {
		if ctrl.ops[i] != want[i] {
			t.Fatalf("ops = %v, want %v", ctrl.ops, want)
		}
	}
}

func TestRestartContainerPropagatesErrors(t *testing.T) {
	ctrl := &fakeController{runningE: errors.New("engine down")}
	if err := RestartContainer(context.Background(), ctrl, "node"); err == nil {
		t.Fatal("engine error must propagate to the tray status line")
	}

	ctrl = &fakeController{running: map[string]bool{"node": true}, stopE: errors.New("stop failed")}
	if err := RestartContainer(context.Background(), ctrl, "node"); err == nil {
		t.Fatal("stop error must propagate (no start attempted on a failed stop)")
	}
	if ctrl.ops[len(ctrl.ops)-1] == "start:node" {
		t.Fatalf("start attempted after failed stop: %v", ctrl.ops)
	}
}

// --- Quit decision -----------------------------------------------------------

func TestQuitDecision(t *testing.T) {
	if got := DecideQuit(false); got != QuitDaemonOnly {
		t.Errorf("DecideQuit(false) = %v, want QuitDaemonOnly", got)
	}
	if got := DecideQuit(true); got != QuitWithContainerStop {
		t.Errorf("DecideQuit(true) = %v, want QuitWithContainerStop", got)
	}
}

func TestHandleQuitConfirmedStopsContainer(t *testing.T) {
	ctrl := &fakeController{running: map[string]bool{"node": true}}
	confirmed := false
	action := HandleQuit(func() bool { confirmed = true; return true }, ctrl, "node")
	if !confirmed {
		t.Fatal("confirmation dialog was not shown")
	}
	if action != QuitWithContainerStop {
		t.Fatalf("action = %v, want QuitWithContainerStop", action)
	}
	if len(ctrl.ops) != 1 || ctrl.ops[0] != "stop:node" {
		t.Fatalf("container stop missing: %v", ctrl.ops)
	}
}

func TestHandleQuitDeclinedKeepsContainer(t *testing.T) {
	ctrl := &fakeController{}
	action := HandleQuit(func() bool { return false }, ctrl, "node")
	if action != QuitDaemonOnly {
		t.Fatalf("action = %v, want QuitDaemonOnly", action)
	}
	if len(ctrl.ops) != 0 {
		t.Fatalf("container touched on decline: %v", ctrl.ops)
	}
}

func TestHandleQuitStopErrorStillQuitsDaemon(t *testing.T) {
	// The daemon must quit even when the container stop fails — the tray
	// reports the failure, but the user asked to exit.
	ctrl := &fakeController{stopE: errors.New("docker gone")}
	action := HandleQuit(func() bool { return true }, ctrl, "node")
	if action != QuitWithContainerStop {
		t.Fatalf("action = %v, want QuitWithContainerStop", action)
	}
}

// --- Upgrade confirm gate (board T-049) --------------------------------------

// Abbruch = keine Aktion: a declined dialog never reaches the upgrade —
// no compose call happens, the image and the running container stay
// untouched.
func TestHandleUpgradeCancelPerformsNoAction(t *testing.T) {
	ran := false
	run := func(ctx context.Context) error {
		ran = true
		return nil
	}
	actuated, err := HandleUpgrade(func() bool { return false }, run)
	if err != nil {
		t.Fatalf("decline must not surface an error: %v", err)
	}
	if actuated {
		t.Fatal("declined dialog must not actuate the upgrade")
	}
	if ran {
		t.Fatal("confirm gate declined — the upgrade must not have run")
	}
}

func TestHandleUpgradeConfirmedRunsUpgrader(t *testing.T) {
	calls := 0
	run := func(ctx context.Context) error { calls++; return nil }
	actuated, err := HandleUpgrade(func() bool { return true }, run)
	if err != nil {
		t.Fatalf("confirmed upgrade must surface no error: %v", err)
	}
	if !actuated {
		t.Fatal("confirmed dialog must actuate the upgrade")
	}
	if calls != 1 {
		t.Fatalf("upgrade run count = %d, want exactly 1", calls)
	}
}

// A failing upgrade surfaces its error for the tray status line.
func TestHandleUpgradeErrorSurfaces(t *testing.T) {
	_, err := HandleUpgrade(func() bool { return true }, func(ctx context.Context) error {
		return errors.New("docker compose pull failed")
	})
	if err == nil {
		t.Fatal("the upgrade error must surface")
	}
}

// A nil confirm (no dialog available) behaves like a decline — the same
// fail-closed semantics as HandleQuit.
func TestHandleUpgradeNilConfirmNoAction(t *testing.T) {
	ran := false
	actuated, err := HandleUpgrade(nil, func(ctx context.Context) error {
		ran = true
		return nil
	})
	if err != nil || actuated || ran {
		t.Fatalf("nil confirm must be a decline: actuated=%v ran=%v err=%v", actuated, ran, err)
	}
}
