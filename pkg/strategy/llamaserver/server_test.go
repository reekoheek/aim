package llamaserver

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

func TestFlagArgs(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]any
		want  []string
	}{
		{
			name:  "no flags",
			flags: nil,
			want:  nil,
		},
		{
			name:  "short flag with int",
			flags: map[string]any{"-ngl": 999},
			want:  []string{"-ngl", "999"},
		},
		{
			name:  "long flag with float",
			flags: map[string]any{"--temp": 0.6},
			want:  []string{"--temp", "0.6"},
		},
		{
			name:  "zero values are still passed",
			flags: map[string]any{"--cache-ram": 0, "--min-p": 0.0},
			want:  []string{"--cache-ram", "0", "--min-p", "0"},
		},
		{
			name:  "negative value",
			flags: map[string]any{"--top-k": -1},
			want:  []string{"--top-k", "-1"},
		},
		{
			name:  "on stays a value",
			flags: map[string]any{"-fa": "on"},
			want:  []string{"-fa", "on"},
		},
		{
			name:  "value with spaces stays one argument",
			flags: map[string]any{"--reasoning-budget-message": "... budget exceeded. Answer now."},
			want:  []string{"--reasoning-budget-message", "... budget exceeded. Answer now."},
		},
		{
			name:  "true becomes bare flag",
			flags: map[string]any{"--jinja": true},
			want:  []string{"--jinja"},
		},
		{
			name:  "false is left out",
			flags: map[string]any{"--offline": false},
			want:  nil,
		},
		{
			name: "sorted by flag name",
			flags: map[string]any{
				"--temp":  0.6,
				"--jinja": true,
				"-hf":     "org/repo",
				"-c":      4096,
			},
			want: []string{"--jinja", "--temp", "0.6", "-c", "4096", "-hf", "org/repo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flagArgs(tt.flags)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServerArgs(t *testing.T) {
	tests := []struct {
		name  string
		entry *config.LlamaCppEntry
		want  []string
	}{
		{
			name:  "managed flags only",
			entry: &config.LlamaCppEntry{Port: 12345, Slots: 1},
			want:  []string{"--offline", "--no-cors-credentials", "--cors-origins", "localhost", "--no-ui", "-np", "1", "--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name: "managed flags come after model flags",
			entry: &config.LlamaCppEntry{
				Port:  23456,
				Slots: 1,
				Flags: map[string]any{"-ngl": 999},
			},
			want: []string{
				"--offline",
				"--no-cors-credentials",
				"--cors-origins", "localhost",
				"--no-ui",
				"-ngl", "999",
				"-np", "1",
				"--host", "127.0.0.1",
				"--port", "23456",
				"--log-colors", "off",
			},
		},
		{
			name:  "slot count sets parallel slots",
			entry: &config.LlamaCppEntry{Port: 12345, Slots: 3},
			want:  []string{"--offline", "--no-cors-credentials", "--cors-origins", "localhost", "--no-ui", "-np", "3", "--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name:  "slot cache turned off keeps one parallel slot",
			entry: &config.LlamaCppEntry{Port: 12345, Slots: 0},
			want:  []string{"--offline", "--no-cors-credentials", "--cors-origins", "localhost", "--no-ui", "-np", "1", "--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name:  "context per slot sizes the whole context",
			entry: &config.LlamaCppEntry{Port: 12345, Slots: 2, Ctx: 64000},
			want:  []string{"--offline", "--no-cors-credentials", "--cors-origins", "localhost", "--no-ui", "-np", "2", "-c", "128000", "--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name:  "context with slot cache turned off covers the one slot",
			entry: &config.LlamaCppEntry{Port: 12345, Slots: 0, Ctx: 4096},
			want:  []string{"--offline", "--no-cors-credentials", "--cors-origins", "localhost", "--no-ui", "-np", "1", "-c", "4096", "--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", tt.entry, config.ServerConfig{}, nil, nil).args()
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServerArgsCannotBeConfiguredAsFlags(t *testing.T) {
	entry := withSlotSavePath(&config.LlamaCppEntry{Port: 12345, Slots: 1, Ctx: 4096}, "/slots")
	for _, arg := range NewServer("m", entry, config.ServerConfig{}, nil, nil).args() {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		t.Run(arg, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".aim.yaml")
			yaml := fmt.Sprintf("models:\n  m:\n    type: llama-server\n    flags:\n      %s: true\n", arg)
			if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(path); err == nil {
				t.Errorf("config accepts %s although aim sets it", arg)
			}
		})
	}
}

func TestNewServerStopTimeout(t *testing.T) {
	tests := []struct {
		name        string
		stopTimeout time.Duration
		want        time.Duration
	}{
		{name: "default", stopTimeout: 0, want: defaultStopTimeout},
		{name: "configured", stopTimeout: 30 * time.Second, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", &config.LlamaCppEntry{StopTimeout: tt.stopTimeout}, config.ServerConfig{}, nil, nil).stopTimeout
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("AIM_TEST_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func startHelperProcess(t *testing.T, srv *Server) {
	t.Helper()
	srv.cmd = exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	srv.cmd.Env = append(os.Environ(), "AIM_TEST_HELPER_PROCESS=1")
	if err := srv.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	srv.done = done
	go func() {
		srv.cmd.Wait()
		close(done)
	}()
}

func stopOnIdle(srv *Server) {
	srv.Stop()
}

func newIdleTestServer(t *testing.T, idleTimeout time.Duration) *Server {
	t.Helper()
	return newIdleTestServerNotifying(t, idleTimeout, stopOnIdle)
}

func newIdleTestServerNotifying(t *testing.T, idleTimeout time.Duration, onIdle func(*Server)) *Server {
	t.Helper()
	srv := NewServer("m", &config.LlamaCppEntry{StopTimeout: time.Second}, config.ServerConfig{}, logger.New("ERROR"), onIdle)
	srv.idleTimeout = idleTimeout
	startHelperProcess(t, srv)
	t.Cleanup(func() { srv.cmd.Process.Kill() })
	return srv
}

func expectRunningFor(t *testing.T, srv *Server, duration time.Duration, what string) {
	t.Helper()
	select {
	case <-time.After(duration):
	case <-srv.done:
		t.Fatalf("server stopped %s", what)
	}
}

func TestIdleTimeoutArmsAndFires(t *testing.T) {
	srv := newIdleTestServer(t, 200*time.Millisecond)

	if !srv.startIdleTimer() {
		t.Fatal("startIdleTimer reported no timer armed")
	}

	waitUntil(t, 2*time.Second, "idle stop", srv.Exited)
}

func TestIdleTimeoutDisabledWhenZero(t *testing.T) {
	srv := newIdleTestServer(t, 0)

	if srv.startIdleTimer() {
		t.Fatal("startIdleTimer armed with idle timeout 0")
	}
	srv.EnterRequest()
	srv.ExitRequest()
	expectRunningFor(t, srv, 300*time.Millisecond, "with idle timeout disabled")
}

func TestEnterRequestCancelsIdleTimer(t *testing.T) {
	srv := newIdleTestServer(t, 200*time.Millisecond)
	srv.startIdleTimer()

	srv.EnterRequest()
	expectRunningFor(t, srv, 400*time.Millisecond, "while a request was in flight")

	srv.ExitRequest()
	waitUntil(t, 2*time.Second, "idle stop after the request exited", srv.Exited)
}

func TestExitRequestWaitsUntilAllRequestsDone(t *testing.T) {
	srv := newIdleTestServer(t, 200*time.Millisecond)
	srv.startIdleTimer()

	srv.EnterRequest()
	srv.EnterRequest()
	srv.ExitRequest()
	expectRunningFor(t, srv, 400*time.Millisecond, "while a request was still in flight")

	srv.ExitRequest()
	waitUntil(t, 2*time.Second, "idle stop after the last request exited", srv.Exited)
}

func TestStopIfIdle(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(srv *Server) uint64
		wantStop bool
	}{
		{
			name: "current timer stops an idle server",
			prepare: func(srv *Server) uint64 {
				srv.startIdleTimer()
				return srv.idleGeneration
			},
			wantStop: true,
		},
		{
			name: "timer replaced by a later request is ignored",
			prepare: func(srv *Server) uint64 {
				srv.startIdleTimer()
				stale := srv.idleGeneration
				srv.EnterRequest()
				srv.ExitRequest()
				return stale
			},
			wantStop: false,
		},
		{
			name: "timer disarmed by an in-flight request is ignored",
			prepare: func(srv *Server) uint64 {
				srv.startIdleTimer()
				stale := srv.idleGeneration
				srv.EnterRequest()
				return stale
			},
			wantStop: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newIdleTestServer(t, time.Hour)
			srv.stopIfIdle(tt.prepare(srv))
			if got := srv.Exited(); got != tt.wantStop {
				t.Errorf("stopped %v, want %v", got, tt.wantStop)
			}
		})
	}
}

func TestStopDisarmsIdleTimerAndRejectsRequests(t *testing.T) {
	srv := newIdleTestServer(t, time.Hour)
	srv.startIdleTimer()

	srv.Stop()

	if srv.idleTimer != nil {
		t.Error("idle timer still armed after Stop")
	}
	if srv.EnterRequest() {
		t.Error("EnterRequest accepted a request on a stopped server")
	}
}

func TestIdleTimeoutLeavesStopToOnIdle(t *testing.T) {
	idled := make(chan *Server, 1)
	srv := newIdleTestServerNotifying(t, 200*time.Millisecond, func(srv *Server) { idled <- srv })
	srv.startIdleTimer()

	if got := receiveWithin(t, idled, 2*time.Second, "idle notification"); got != srv {
		t.Errorf("onIdle received %p, want %p", got, srv)
	}
	expectRunningFor(t, srv, 300*time.Millisecond, "without onIdle stopping it")
	if srv.EnterRequest() {
		t.Error("EnterRequest accepted a request on a server reported idle")
	}
}

func TestEnterRequestRejectedOnceIdleStopBegins(t *testing.T) {
	srv := newIdleTestServer(t, time.Hour)
	srv.startIdleTimer()
	generation := srv.idleGeneration

	stopped := make(chan struct{})
	go func() {
		srv.stopIfIdle(generation)
		close(stopped)
	}()
	waitUntil(t, 2*time.Second, "idle stop to begin", func() bool {
		srv.idleMu.Lock()
		defer srv.idleMu.Unlock()
		return srv.stopping
	})

	if srv.EnterRequest() {
		t.Error("EnterRequest accepted a request on a server stopping after idle")
	}
	receiveWithin(t, stopped, 5*time.Second, "idle stop")
}

func TestStartAbortsWhenContextCanceled(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{name: "server never becomes ready", mode: fakeLlamaServerModeHang},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(fakeLlamaServerEnv, tt.mode)
			srv := NewServer("m", &config.LlamaCppEntry{Bin: os.Args[0], Port: freePort(t)}, config.ServerConfig{}, logger.New("ERROR"), nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			started := make(chan error, 1)
			go func() { started <- srv.Start(ctx) }()

			cancel()

			if err := receiveWithin(t, started, 5*time.Second, "canceled startup"); err == nil {
				t.Fatal("Start succeeded although its context was canceled")
			}
			if !srv.Exited() {
				t.Error("process still running after canceled startup")
			}
		})
	}
}

func TestIdleTimeoutDuration(t *testing.T) {
	seconds := func(value int) *int { return &value }
	tests := []struct {
		name    string
		seconds *int
		want    time.Duration
	}{
		{"unset uses default", nil, defaultIdleTimeout},
		{"zero disables", seconds(0), 0},
		{"configured", seconds(300), 300 * time.Second},
		{"one second", seconds(1), time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idleTimeoutDuration(tt.seconds); got != tt.want {
				t.Errorf("idleTimeoutDuration = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewServerIdleTimeoutZeroNeverStops(t *testing.T) {
	disabled := 0
	srv := NewServer("m", &config.LlamaCppEntry{StopTimeout: time.Second}, config.ServerConfig{IdleTimeout: &disabled}, logger.New("ERROR"), nil)
	startHelperProcess(t, srv)
	t.Cleanup(func() { srv.cmd.Process.Kill() })

	if srv.startIdleTimer() {
		t.Fatal("startIdleTimer armed with idle_timeout 0")
	}
	srv.EnterRequest()
	srv.ExitRequest()
	expectRunningFor(t, srv, 300*time.Millisecond, "with idle_timeout 0")
}

const (
	fakeLlamaServerEnv            = "AIM_FAKE_LLAMA_SERVER"
	fakeLlamaServerModeServe      = "serve"
	fakeLlamaServerModeHang       = "hang"
	fakeLlamaServerOrphanInterval = 100 * time.Millisecond
)

func TestMain(m *testing.M) {
	switch os.Getenv(fakeLlamaServerEnv) {
	case fakeLlamaServerModeServe:
		go exitWhenOrphaned(os.Getppid())
		runFakeLlamaServer(os.Args[1:])
	case fakeLlamaServerModeHang:
		exitWhenOrphaned(os.Getppid())
	default:
		os.Exit(m.Run())
	}
}

func exitWhenOrphaned(parent int) {
	for os.Getppid() == parent {
		time.Sleep(fakeLlamaServerOrphanInterval)
	}
	os.Exit(0)
}

func runFakeLlamaServer(args []string) {
	slotDir := argValue(args, "--slot-save-path")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		var body struct {
			FakeDelay  string `json:"fake_delay"`
			FakeStatus int    `json:"fake_status"`
			FakeEcho   bool   `json:"fake_echo"`
		}
		json.Unmarshal(received, &body)
		delay, _ := time.ParseDuration(body.FakeDelay)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cmp.Or(body.FakeStatus, http.StatusOK))
		if body.FakeEcho {
			w.Write(received)
			return
		}
		fmt.Fprint(w, `{"choices":[]}`)
	})
	mux.HandleFunc("POST /slots/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Filename string `json:"filename"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Query().Get("action") == "save" {
			os.WriteFile(filepath.Join(slotDir, body.Filename), []byte("kv"), 0o644)
		}
		fmt.Fprint(w, `{"n_saved": 1, "n_restored": 1}`)
	})
	http.ListenAndServe(net.JoinHostPort(argValue(args, "--host"), argValue(args, "--port")), mux)
}

func argValue(args []string, name string) string {
	index := slices.Index(args, name)
	if index < 0 || index+1 >= len(args) {
		return ""
	}
	return args[index+1]
}
