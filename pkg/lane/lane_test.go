package lane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
)

const (
	testLane     = "default"
	waitTimeout  = 5 * time.Second
	pollInterval = 10 * time.Millisecond
	quietPeriod  = 300 * time.Millisecond
)

var errFakeStart = errors.New("fake start failure")

type fakeProcess struct {
	model         string
	failStart     bool
	waitForCancel bool
	mu            sync.Mutex
	started       bool
	stopped       bool
	crashed       bool
}

func (p *fakeProcess) Start(ctx context.Context) error {
	if p.waitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if p.failStart {
		return errFakeStart
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}

func (p *fakeProcess) Stop(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
}

func (p *fakeProcess) Exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped || p.crashed
}

func (p *fakeProcess) crash() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.crashed = true
}

func newTestSupervisor(t *testing.T, idleTimeout time.Duration) *Supervisor {
	t.Helper()
	s := NewSupervisor(idleTimeout, logger.New("ERROR"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		defer cancel()
		s.Close(ctx)
	})
	return s
}

func buildFake(model string) func() (*fakeProcess, error) {
	return func() (*fakeProcess, error) {
		return &fakeProcess{model: model}, nil
	}
}

func runFake(t *testing.T, s *Supervisor, laneName string, model string) *Lease[*fakeProcess] {
	t.Helper()
	lease, err := Run(context.Background(), s, laneName, model, buildFake(model))
	if err != nil {
		t.Fatalf("Run(%q, %q): %v", laneName, model, err)
	}
	return lease
}

func runFakeAsync(s *Supervisor, laneName string, model string) <-chan error {
	ran := make(chan error, 1)
	go func() {
		lease, err := Run(context.Background(), s, laneName, model, buildFake(model))
		if err == nil {
			lease.Release()
		}
		ran <- err
	}()
	return ran
}

func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(waitTimeout):
		t.Fatalf("%s did not finish within %v", what, waitTimeout)
		var zero T
		return zero
	}
}

func expectPending[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case value := <-ch:
		t.Fatalf("%s finished early with %v", what, value)
	case <-time.After(quietPeriod):
	}
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.After(waitTimeout)
	for !condition() {
		select {
		case <-deadline:
			t.Fatalf("%s did not happen within %v", what, waitTimeout)
		case <-time.After(pollInterval):
		}
	}
}

func waitForTurnHeld(t *testing.T, s *Supervisor, laneName string) {
	t.Helper()
	waitUntil(t, "lane turn held", func() bool {
		lane := s.Lane(laneName)
		return lane != nil && len(lane.turn) == 1
	})
}

func TestRunOnSameLane(t *testing.T) {
	tests := []struct {
		name     string
		models   []string
		wantSame bool
	}{
		{name: "same model reuses the running process", models: []string{"a", "a"}, wantSame: true},
		{name: "different model swaps the running process", models: []string{"a", "b"}, wantSame: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSupervisor(t, 0)

			first := runFake(t, s, testLane, tt.models[0])
			first.Release()
			second := runFake(t, s, testLane, tt.models[1])
			second.Release()

			if got := first.Process == second.Process; got != tt.wantSame {
				t.Errorf("same process %v, want %v", got, tt.wantSame)
			}
			if got := first.Process.Exited(); got == tt.wantSame {
				t.Errorf("first process exited %v, want %v", got, !tt.wantSame)
			}
			if got := s.Lane(testLane).Process(); got != second.Process {
				t.Errorf("lane holds %v, want %v", got, second.Process)
			}
		})
	}
}

func TestRunRestartsExitedProcess(t *testing.T) {
	s := newTestSupervisor(t, 0)
	first := runFake(t, s, testLane, "a")
	first.Release()
	first.Process.crash()

	second := runFake(t, s, testLane, "a")
	defer second.Release()

	if second.Process == first.Process || !second.Process.started {
		t.Error("Run did not start a new process after the old one exited")
	}
}

func TestRunRejectsCanceledRequest(t *testing.T) {
	s := newTestSupervisor(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Run(ctx, s, testLane, "a", buildFake("a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want %v", err, context.Canceled)
	}
	if s.Lane(testLane).Process() != nil {
		t.Error("Run started a process for a canceled request")
	}
}

func TestRunFailures(t *testing.T) {
	errBuild := errors.New("fake build failure")
	tests := []struct {
		name         string
		build        func() (*fakeProcess, error)
		wantErr      error
		wantOldAlive bool
	}{
		{
			name:         "build failure keeps the running process",
			build:        func() (*fakeProcess, error) { return nil, errBuild },
			wantErr:      errBuild,
			wantOldAlive: true,
		},
		{
			name:         "start failure leaves the lane empty",
			build:        func() (*fakeProcess, error) { return &fakeProcess{failStart: true}, nil },
			wantErr:      ErrStartFailed,
			wantOldAlive: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSupervisor(t, 0)
			old := runFake(t, s, testLane, "a")
			old.Release()

			if _, err := Run(context.Background(), s, testLane, "b", tt.build); !errors.Is(err, tt.wantErr) {
				t.Fatalf("error %v, want %v", err, tt.wantErr)
			}
			if alive := !old.Process.Exited(); alive != tt.wantOldAlive {
				t.Errorf("old process alive %v, want %v", alive, tt.wantOldAlive)
			}
			wantResident := Process(nil)
			if tt.wantOldAlive {
				wantResident = old.Process
			}
			if got := s.Lane(testLane).Process(); got != wantResident {
				t.Errorf("lane holds %v, want %v", got, wantResident)
			}
		})
	}
}

func TestSwapWaitsForInFlightLease(t *testing.T) {
	s := newTestSupervisor(t, 0)
	old := runFake(t, s, testLane, "a")

	swapped := runFakeAsync(s, testLane, "b")
	waitForTurnHeld(t, s, testLane)
	expectPending(t, swapped, "swap while a lease is in flight")
	if old.Process.Exited() {
		t.Fatal("old process stopped while a lease was in flight")
	}

	old.Release()

	if err := receiveWithin(t, swapped, "swap after the lease"); err != nil {
		t.Fatal(err)
	}
	if !old.Process.Exited() {
		t.Error("old process still running after the swap")
	}
}

func TestRunGivesUpWaitingForTurnWhenRequestCanceled(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "request for the model being replaced", model: "a"},
		{name: "request for the model being swapped in", model: "b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSupervisor(t, 0)
			old := runFake(t, s, testLane, "a")
			swapped := runFakeAsync(s, testLane, "b")
			waitForTurnHeld(t, s, testLane)

			ctx, cancel := context.WithCancel(context.Background())
			waiting := make(chan error, 1)
			go func() {
				lease, err := Run(ctx, s, testLane, tt.model, buildFake(tt.model))
				if err == nil {
					lease.Release()
				}
				waiting <- err
			}()
			cancel()

			if err := receiveWithin(t, waiting, "canceled request waiting for the swap"); !errors.Is(err, context.Canceled) {
				t.Errorf("error %v, want %v", err, context.Canceled)
			}

			old.Release()
			if err := receiveWithin(t, swapped, "swap after the lease"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIdleTimeout(t *testing.T) {
	tests := []struct {
		name        string
		idleTimeout time.Duration
		leases      int
		released    int
		wantStop    bool
	}{
		{name: "stops after the last lease is released", idleTimeout: 100 * time.Millisecond, leases: 1, released: 1, wantStop: true},
		{name: "zero never stops", idleTimeout: 0, leases: 1, released: 1, wantStop: false},
		{name: "lease in flight keeps the process", idleTimeout: 100 * time.Millisecond, leases: 1, released: 0, wantStop: false},
		{name: "waits until every lease is released", idleTimeout: 100 * time.Millisecond, leases: 2, released: 1, wantStop: false},
		{name: "stops after several leases are all released", idleTimeout: 100 * time.Millisecond, leases: 2, released: 2, wantStop: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSupervisor(t, tt.idleTimeout)
			leases := make([]*Lease[*fakeProcess], tt.leases)
			for i := range leases {
				leases[i] = runFake(t, s, testLane, "a")
			}
			for _, lease := range leases[:tt.released] {
				lease.Release()
			}
			process := leases[0].Process

			if tt.wantStop {
				waitUntil(t, "idle stop", process.Exited)
				if s.Lane(testLane).Process() != nil {
					t.Error("lane still holds the process stopped after idle")
				}
				return
			}
			time.Sleep(tt.idleTimeout + quietPeriod)
			if process.Exited() {
				t.Error("process stopped although it should stay running")
			}
			for _, lease := range leases[tt.released:] {
				lease.Release()
			}
		})
	}
}

func TestIdleTimeoutRestartedByLaterLease(t *testing.T) {
	const idleTimeout = 300 * time.Millisecond
	s := newTestSupervisor(t, idleTimeout)
	first := runFake(t, s, testLane, "a")
	first.Release()

	time.Sleep(idleTimeout / 2)
	second := runFake(t, s, testLane, "a")
	time.Sleep(idleTimeout)
	if second.Process.Exited() {
		t.Fatal("idle timer armed before the later lease stopped the process")
	}

	second.Release()
	waitUntil(t, "idle stop after the later lease", second.Process.Exited)
}

func TestStopIfIdleIgnoresStaleTimer(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(t *testing.T, s *Supervisor) uint64
		wantStop bool
	}{
		{
			name: "current timer stops an idle process",
			prepare: func(t *testing.T, s *Supervisor) uint64 {
				return s.Lane(testLane).resident.idleGeneration
			},
			wantStop: true,
		},
		{
			name: "timer replaced by a later lease is ignored",
			prepare: func(t *testing.T, s *Supervisor) uint64 {
				stale := s.Lane(testLane).resident.idleGeneration
				runFake(t, s, testLane, "a").Release()
				return stale
			},
			wantStop: false,
		},
		{
			name: "timer disarmed by an in-flight lease is ignored",
			prepare: func(t *testing.T, s *Supervisor) uint64 {
				stale := s.Lane(testLane).resident.idleGeneration
				t.Cleanup(runFake(t, s, testLane, "a").Release)
				return stale
			},
			wantStop: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSupervisor(t, time.Hour)
			lease := runFake(t, s, testLane, "a")
			lease.Release()
			lane := s.Lane(testLane)
			r := lane.resident

			lane.stopIfIdle(r, tt.prepare(t, s))

			if got := lease.Process.Exited(); got != tt.wantStop {
				t.Errorf("stopped %v, want %v", got, tt.wantStop)
			}
		})
	}
}

func TestRunStartsNewProcessOnceIdleStopBegins(t *testing.T) {
	s := newTestSupervisor(t, time.Hour)
	old := runFake(t, s, testLane, "a")
	old.Release()
	lane := s.Lane(testLane)
	r := lane.resident
	lane.mu.Lock()
	generation := r.idleGeneration
	lane.mu.Unlock()

	lane.turn <- struct{}{}
	stopped := make(chan struct{})
	go func() {
		lane.stopIfIdle(r, generation)
		close(stopped)
	}()
	waitUntil(t, "idle stop to begin", func() bool {
		lane.mu.Lock()
		defer lane.mu.Unlock()
		return r.stopping
	})

	if lane.lease(r) {
		t.Error("lease accepted on a process stopping after idle")
	}
	lane.endTurn()
	receiveWithin(t, stopped, "idle stop")

	fresh := runFake(t, s, testLane, "a")
	defer fresh.Release()
	if fresh.Process == old.Process {
		t.Error("Run reused the process stopped after idle")
	}
}
