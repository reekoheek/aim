package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

const fakeMonitorCloseTimeout = 10 * time.Second

var testLogger = logger.New("ERROR")

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newFakeLlamaConfig(t *testing.T, idleTimeoutSeconds int, models ...string) *config.Config {
	t.Helper()
	t.Setenv(fakeLlamaServerEnv, fakeLlamaServerModeServe)
	cfg := &config.Config{
		Models: map[string]config.ModelEntry{},
		Server: config.ServerConfig{
			SaveSlotPath:   t.TempDir(),
			InferenceDelay: time.Millisecond,
			IdleTimeout:    &idleTimeoutSeconds,
		},
	}
	for _, model := range models {
		cfg.Models[model] = config.ModelEntry{
			Type: "llama-server",
			LlamaCpp: &config.LlamaCppEntry{
				Bin:         os.Args[0],
				Port:        freePort(t),
				StopTimeout: 2 * time.Second,
			},
		}
	}
	return cfg
}

func currentServer(m *Monitor) *Server {
	m.lock <- struct{}{}
	defer m.unlock()
	return m.server
}

func acquire(ctx context.Context, m *Monitor, cfg *config.Config, model string) (*Server, error) {
	return m.Acquire(ctx, cfg, testLogger, model)
}

func newTestMonitor(t *testing.T) *Monitor {
	t.Helper()
	m := NewMonitor()
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			m.Close(ctx)
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(fakeMonitorCloseTimeout):
			t.Error("monitor close did not finish")
		}
	})
	return m
}

func receiveWithin[T any](t *testing.T, ch <-chan T, timeout time.Duration, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(timeout):
		t.Fatalf("%s did not finish within %v", what, timeout)
		var zero T
		return zero
	}
}

func waitUntil(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for !condition() {
		select {
		case <-deadline:
			t.Fatalf("%s did not happen within %v", what, timeout)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func acquireAsync(m *Monitor, cfg *config.Config, model string) <-chan error {
	acquired := make(chan error, 1)
	go func() {
		srv, err := acquire(context.Background(), m, cfg, model)
		if err == nil {
			srv.ExitRequest()
		}
		acquired <- err
	}()
	return acquired
}

func waitForMonitorLockHeld(t *testing.T, m *Monitor) {
	t.Helper()
	waitUntil(t, 5*time.Second, "switch holding the monitor", func() bool {
		return len(m.lock) == 1
	})
}

func TestMonitorAcquire(t *testing.T) {
	tests := []struct {
		name     string
		models   []string
		wantSame bool
	}{
		{name: "same model reuses the running server", models: []string{"a", "a"}, wantSame: true},
		{name: "different model replaces the running server", models: []string{"a", "b"}, wantSame: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a", "b")
			m := newTestMonitor(t)

			first, err := acquire(context.Background(), m, cfg, tt.models[0])
			if err != nil {
				t.Fatal(err)
			}
			first.ExitRequest()
			second, err := acquire(context.Background(), m, cfg, tt.models[1])
			if err != nil {
				t.Fatal(err)
			}
			second.ExitRequest()

			if got := first == second; got != tt.wantSame {
				t.Errorf("same server %v, want %v", got, tt.wantSame)
			}
			if got := first.Exited(); got == tt.wantSame {
				t.Errorf("first server exited %v, want %v", got, !tt.wantSame)
			}
		})
	}
}

func TestMonitorAcquireRestartsServerStoppedAfterIdle(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 1, "a")
	m := newTestMonitor(t)

	first, err := acquire(context.Background(), m, cfg, "a")
	if err != nil {
		t.Fatal(err)
	}
	first.ExitRequest()
	waitUntil(t, 5*time.Second, "idle stop", first.Exited)

	second, err := acquire(context.Background(), m, cfg, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer second.ExitRequest()
	if second == first || second.Exited() {
		t.Error("Acquire did not start a new server after the idle stop")
	}
}

func TestMonitorAcquireRejectsCanceledRequest(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	m := newTestMonitor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := acquire(ctx, m, cfg, "a"); err == nil {
		t.Fatal("Acquire succeeded for a canceled request")
	}
	if currentServer(m) != nil {
		t.Error("Acquire started a server for a canceled request")
	}
}

func TestMonitorSwitchWaitsForInFlightRequest(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a", "b")
	m := newTestMonitor(t)
	old, err := acquire(context.Background(), m, cfg, "a")
	if err != nil {
		t.Fatal(err)
	}
	release, err := old.AcquireSlot(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}

	switched := acquireAsync(m, cfg, "b")
	waitForMonitorLockHeld(t, m)
	select {
	case err := <-switched:
		t.Fatalf("switch finished while a request was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if old.Exited() {
		t.Fatal("old server stopped while a request was in flight")
	}

	release()
	old.ExitRequest()

	if err := receiveWithin(t, switched, handleTimeout, "switch after the request"); err != nil {
		t.Fatal(err)
	}
	if !old.Exited() {
		t.Error("old server still running after the switch")
	}
}

func TestMonitorCloseInterruptsSwitchWaitingForRequest(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a", "b")
	m := newTestMonitor(t)
	old, err := acquire(context.Background(), m, cfg, "a")
	if err != nil {
		t.Fatal(err)
	}
	release, err := old.AcquireSlot(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	defer old.ExitRequest()
	defer release()

	switched := acquireAsync(m, cfg, "b")
	waitForMonitorLockHeld(t, m)

	closed := make(chan struct{})
	go func() {
		m.Close(context.Background())
		close(closed)
	}()

	receiveWithin(t, closed, fakeMonitorCloseTimeout, "close during a switch")
	if err := receiveWithin(t, switched, handleTimeout, "interrupted switch"); err != errMonitorClosed {
		t.Errorf("switch error %v, want %v", err, errMonitorClosed)
	}
	if !old.Exited() {
		t.Error("old server still running after close")
	}
	if _, err := acquire(context.Background(), m, cfg, "a"); err != errMonitorClosed {
		t.Errorf("Acquire after close: error %v, want %v", err, errMonitorClosed)
	}
}

func TestMonitorAcquireGivesUpWaitingForSwitchWhenRequestCanceled(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "request for the model being replaced", model: "a"},
		{name: "request for the model being switched to", model: "b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a", "b")
			m := newTestMonitor(t)
			old, err := acquire(context.Background(), m, cfg, "a")
			if err != nil {
				t.Fatal(err)
			}
			release, err := old.AcquireSlot(context.Background(), "session")
			if err != nil {
				t.Fatal(err)
			}

			switched := acquireAsync(m, cfg, "b")
			waitForMonitorLockHeld(t, m)

			ctx, cancel := context.WithCancel(context.Background())
			waiting := make(chan error, 1)
			go func() {
				srv, err := acquire(ctx, m, cfg, tt.model)
				if err == nil {
					srv.ExitRequest()
				}
				waiting <- err
			}()
			cancel()

			if err := receiveWithin(t, waiting, time.Second, "canceled request waiting for the switch"); err != context.Canceled {
				t.Errorf("error %v, want %v", err, context.Canceled)
			}

			release()
			old.ExitRequest()
			if err := receiveWithin(t, switched, handleTimeout, "switch after the request"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMonitorCloseStopsServerOnceSwitchOutlastsDeadline(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	m := newTestMonitor(t)
	srv, err := acquire(context.Background(), m, cfg, "a")
	if err != nil {
		t.Fatal(err)
	}
	srv.ExitRequest()

	m.lock <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	closed := make(chan struct{})
	go func() {
		m.Close(ctx)
		close(closed)
	}()

	<-ctx.Done()
	select {
	case <-closed:
		t.Fatal("close returned while a switch still held the monitor")
	case <-time.After(200 * time.Millisecond):
	}
	m.unlock()

	receiveWithin(t, closed, fakeMonitorCloseTimeout, "close after the switch")
	if !srv.Exited() {
		t.Error("server still running after close")
	}
}

func TestMonitorCloseInterruptsStartDuringInferenceDelay(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	cfg.Server.InferenceDelay = time.Minute
	m := newTestMonitor(t)
	healthURL := fmt.Sprintf("http://%s:%d/health", defaultHost, cfg.Models["a"].LlamaCpp.Port)

	started := acquireAsync(m, cfg, "a")
	waitForInferenceDelay(t, healthURL)

	closed := make(chan struct{})
	go func() {
		m.Close(context.Background())
		close(closed)
	}()

	receiveWithin(t, closed, fakeMonitorCloseTimeout, "close during the inference delay")
	if err := receiveWithin(t, started, handleTimeout, "interrupted start"); !errors.Is(err, errStartFailed) {
		t.Errorf("start error %v, want %v", err, errStartFailed)
	}
	if respondsOK(healthURL) {
		t.Error("server still running after close")
	}
}

func TestMonitorAcquireLeavesServerForCloseOnceShutdownBegins(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "request for another model", model: "b"},
		{name: "request for a model missing from config", model: "missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a", "b")
			m := newTestMonitor(t)
			old, err := acquire(context.Background(), m, cfg, "a")
			if err != nil {
				t.Fatal(err)
			}
			release, err := old.AcquireSlot(context.Background(), "session")
			if err != nil {
				t.Fatal(err)
			}
			release()
			old.ExitRequest()

			m.closeLifetime()
			if _, err := acquire(context.Background(), m, cfg, tt.model); err != errMonitorClosed {
				t.Errorf("Acquire during shutdown: error %v, want %v", err, errMonitorClosed)
			}
			if old.Exited() {
				t.Fatal("Acquire during shutdown stopped the running server")
			}

			ctx, cancel := context.WithTimeout(context.Background(), fakeMonitorCloseTimeout)
			defer cancel()
			m.Close(ctx)

			if !old.Exited() {
				t.Error("server still running after close")
			}
			if _, err := os.Stat(filepath.Join(old.slotSavePath, old.slotFile("session"))); err != nil {
				t.Errorf("slot not saved on close: %v", err)
			}
		})
	}
}
