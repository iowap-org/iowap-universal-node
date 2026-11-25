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
