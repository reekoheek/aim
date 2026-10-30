package lane

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunOnDifferentLanesKeepsBothRunning(t *testing.T) {
	s := newTestSupervisor(t, 0)

	gpu := runFake(t, s, "gpu", "a")
	gpu.Release()
	cpu := runFake(t, s, "cpu", "b")
	cpu.Release()

	if gpu.Process.Exited() || cpu.Process.Exited() {
		t.Errorf("gpu exited %v, cpu exited %v, want both running", gpu.Process.Exited(), cpu.Process.Exited())
	}
	if s.Lane("gpu").Process() != gpu.Process || s.Lane("cpu").Process() != cpu.Process {
		t.Error("lanes do not hold their own processes")
	}
}

func TestRunOnBusyLaneDoesNotBlockOtherLane(t *testing.T) {
	s := newTestSupervisor(t, 0)
	busy := runFake(t, s, "gpu", "a")
	defer busy.Release()

	swapped := runFakeAsync(s, "gpu", "b")
	waitForTurnHeld(t, s, "gpu")

	other := runFakeAsync(s, "cpu", "c")
	if err := receiveWithin(t, other, "run on another lane during a swap"); err != nil {
		t.Fatal(err)
	}
	expectPending(t, swapped, "swap while a lease is in flight")
	busy.Release()
	if err := receiveWithin(t, swapped, "swap after the lease"); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseReleaseIsIdempotent(t *testing.T) {
	s := newTestSupervisor(t, 0)
	first := runFake(t, s, testLane, "a")
	second := runFake(t, s, testLane, "a")

	first.Release()
	first.Release()

	if got := s.Lane(testLane).Leases(); got != 1 {
		t.Errorf("leases %d, want 1", got)
	}
	second.Release()
}

func TestCloseStopsEveryLane(t *testing.T) {
	s := newTestSupervisor(t, 0)
	gpu := runFake(t, s, "gpu", "a")
	gpu.Release()
	cpu := runFake(t, s, "cpu", "b")
	cpu.Release()

	s.Close(context.Background())

	if !gpu.Process.Exited() || !cpu.Process.Exited() {
		t.Errorf("gpu exited %v, cpu exited %v, want both stopped", gpu.Process.Exited(), cpu.Process.Exited())
	}
	if _, err := Run(context.Background(), s, "gpu", "a", buildFake("a")); !errors.Is(err, ErrClosed) {
		t.Errorf("Run after close: error %v, want %v", err, ErrClosed)
	}
}

func TestCloseInterruptsSwapWaitingForLease(t *testing.T) {
	s := newTestSupervisor(t, 0)
	old := runFake(t, s, testLane, "a")
	defer old.Release()

	swapped := runFakeAsync(s, testLane, "b")
	waitForTurnHeld(t, s, testLane)

	closed := make(chan struct{})
	go func() {
		s.Close(context.Background())
		close(closed)
	}()

	receiveWithin(t, closed, "close during a swap")
	if err := receiveWithin(t, swapped, "interrupted swap"); !errors.Is(err, ErrClosed) {
		t.Errorf("swap error %v, want %v", err, ErrClosed)
	}
	if !old.Process.Exited() {
		t.Error("old process still running after close")
	}
}

func TestCloseInterruptsStartBeforeProcessIsReady(t *testing.T) {
	s := newTestSupervisor(t, 0)
	started := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), s, testLane, "a", func() (*fakeProcess, error) {
			return &fakeProcess{waitForCancel: true}, nil
		})
		started <- err
	}()
	waitForTurnHeld(t, s, testLane)

	closed := make(chan struct{})
	go func() {
		s.Close(context.Background())
		close(closed)
	}()

	receiveWithin(t, closed, "close during startup")
	if err := receiveWithin(t, started, "interrupted start"); !errors.Is(err, ErrStartFailed) {
		t.Errorf("start error %v, want %v", err, ErrStartFailed)
	}
	if s.Lane(testLane).Process() != nil {
		t.Error("lane kept the process whose start was interrupted")
	}
}

func TestCloseWaitsForTurnPastDeadline(t *testing.T) {
	s := newTestSupervisor(t, 0)
	running := runFake(t, s, testLane, "a")
	running.Release()
	lane := s.Lane(testLane)

	lane.turn <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	closed := make(chan struct{})
	go func() {
		s.Close(ctx)
		close(closed)
	}()

	<-ctx.Done()
	expectPending(t, closed, "close while a swap holds the lane")
	lane.endTurn()

	receiveWithin(t, closed, "close after the swap")
	if !running.Process.Exited() {
		t.Error("process still running after close")
	}
}

func TestRunLeavesProcessForCloseOnceShutdownBegins(t *testing.T) {
	errMissing := errors.New("model missing from config")
	tests := []struct {
		name  string
		model string
		build func() (*fakeProcess, error)
	}{
		{name: "request for another model", model: "b", build: buildFake("b")},
		{name: "request for a model that cannot be built", model: "missing", build: func() (*fakeProcess, error) { return nil, errMissing }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSupervisor(t, 0)
			old := runFake(t, s, testLane, "a")
			old.Release()

			s.closeLifetime()
			if _, err := Run(context.Background(), s, testLane, tt.model, tt.build); !errors.Is(err, ErrClosed) {
				t.Errorf("Run during shutdown: error %v, want %v", err, ErrClosed)
			}
			if old.Process.Exited() {
				t.Fatal("Run during shutdown stopped the running process")
			}

			s.Close(context.Background())
			if !old.Process.Exited() {
				t.Error("process still running after close")
			}
		})
	}
}
