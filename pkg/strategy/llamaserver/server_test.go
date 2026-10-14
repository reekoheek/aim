package llamaserver

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
			entry: &config.LlamaCppEntry{Port: 12345},
			want:  []string{"--offline", "--no-cors-credentials", "--cors-origins", "localhost", "--no-ui", "--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name: "managed flags come after model flags",
			entry: &config.LlamaCppEntry{
				Port:  23456,
				Flags: map[string]any{"-ngl": 999},
			},
			want: []string{
				"--offline",
				"--no-cors-credentials",
				"--cors-origins", "localhost",
				"--no-ui",
				"-ngl", "999",
				"--host", "127.0.0.1",
				"--port", "23456",
				"--log-colors", "off",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", tt.entry, config.ServerConfig{}, nil).args()
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
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
			got := NewServer("m", &config.LlamaCppEntry{StopTimeout: tt.stopTimeout}, config.ServerConfig{}, nil).stopTimeout
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewServerInferenceDelay(t *testing.T) {
	tests := []struct {
		name  string
		delay time.Duration
		want  time.Duration
	}{
		{name: "default", delay: 0, want: defaultInferenceDelay},
		{name: "configured", delay: 5 * time.Second, want: 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", &config.LlamaCppEntry{}, config.ServerConfig{InferenceDelay: tt.delay}, nil).inferenceDelay
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeSlotServer struct {
	mu          sync.Mutex
	dir         string
	failRestore map[string]bool
	actions     []string
}

func (f *fakeSlotServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := r.URL.Query().Get("action")

	f.mu.Lock()
	f.actions = append(f.actions, action+" "+body.Filename)
	f.mu.Unlock()

	switch {
	case action == "restore" && f.failRestore[body.Filename]:
		http.Error(w, "restore failed", http.StatusInternalServerError)
	case action == "save":
		path := filepath.Join(f.dir, body.Filename)
		if err := os.WriteFile(path, []byte("kv"), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		savedAt := time.Now()
		if err := os.Chtimes(path, savedAt, savedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"n_saved": 1}`)
	default:
		fmt.Fprint(w, `{"n_restored": 1}`)
	}
}

func (f *fakeSlotServer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.actions)
}

func newSlotTestServer(t *testing.T, existing []string, failRestore []string, stopTimeout time.Duration, maxFiles int) (*Server, *fakeSlotServer) {
	t.Helper()
	dir := t.TempDir()
	fake := &fakeSlotServer{dir: dir, failRestore: map[string]bool{}}
	httpServer := httptest.NewServer(fake)
	t.Cleanup(httpServer.Close)

	entry := &config.LlamaCppEntry{
		Port:        httpServer.Listener.Addr().(*net.TCPAddr).Port,
		StopTimeout: stopTimeout,
	}
	server := config.ServerConfig{SaveSlotPath: dir, SlotExpiration: time.Hour, SlotMaxFiles: maxFiles}
	srv := NewServer("m", entry, server, logger.New("ERROR"))
	for _, session := range existing {
		if err := os.WriteFile(filepath.Join(dir, srv.slotFile(session)), []byte("kv"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, session := range failRestore {
		fake.failRestore[srv.slotFile(session)] = true
	}
	return srv, fake
}

func runSlotRequests(t *testing.T, srv *Server, sessions []string) {
	t.Helper()
	for _, session := range sessions {
		release, err := srv.AcquireSlot(context.Background(), session)
		if err != nil {
			t.Fatalf("AcquireSlot(%q): %v", session, err)
		}
		release()
	}
}

func expectedActions(srv *Server, actions []string) []string {
	var expected []string
	for _, action := range actions {
		name, session, _ := strings.Cut(action, " ")
		expected = append(expected, name+" "+srv.slotFile(session))
	}
	return expected
}

func TestAcquireSlotSavesAndRestoresOnlyOnSessionSwitch(t *testing.T) {
	tests := []struct {
		name        string
		existing    []string
		failRestore []string
		requests    []string
		want        []string
	}{
		{
			name:     "same session reuses the slot without save or restore",
			requests: []string{"a", "a", "a"},
			want:     nil,
		},
		{
			name:     "switching session saves the previous one",
			requests: []string{"a", "b"},
			want:     []string{"save a"},
		},
		{
			name:     "switching back restores the saved session",
			requests: []string{"a", "b", "a"},
			want:     []string{"save a", "save b", "restore a"},
		},
		{
			name:     "existing cache is restored on the first request",
			existing: []string{"a"},
			requests: []string{"a", "a"},
			want:     []string{"restore a"},
		},
		{
			name:        "session is saved after a failed restore",
			existing:    []string{"a"},
			failRestore: []string{"a"},
			requests:    []string{"a", "b"},
			want:        []string{"restore a", "save a"},
		},
		{
			name:     "session-less request evicts the session",
			requests: []string{"a", "", "a"},
			want:     []string{"save a", "restore a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, fake := newSlotTestServer(t, tt.existing, tt.failRestore, 0, 0)
			runSlotRequests(t, srv, tt.requests)
			if got, want := fake.recorded(), expectedActions(srv, tt.want); !slices.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestAcquireSlotKeepsRecentlyUsedSlotFiles(t *testing.T) {
	tests := []struct {
		name      string
		maxFiles  int
		requests  []string
		want      []string
		wantFiles []string
	}{
		{
			name:      "least recently used session removed over the limit",
			maxFiles:  2,
			requests:  []string{"a", "b", "c", "d"},
			want:      []string{"save a", "save b", "save c"},
			wantFiles: []string{"b", "c"},
		},
		{
			name:      "requested session counts as used before the limit is applied",
			maxFiles:  2,
			requests:  []string{"a", "b", "c", "a"},
			want:      []string{"save a", "save b", "save c", "restore a"},
			wantFiles: []string{"a", "c"},
		},
		{
			name:      "requested session is restored before the limit is applied",
			maxFiles:  1,
			requests:  []string{"a", "b", "a"},
			want:      []string{"save a", "save b", "restore a"},
			wantFiles: []string{"b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, fake := newSlotTestServer(t, nil, nil, 0, tt.maxFiles)
			runSlotRequests(t, srv, tt.requests)
			if got, want := fake.recorded(), expectedActions(srv, tt.want); !slices.Equal(got, want) {
				t.Errorf("actions %q, want %q", got, want)
			}
			var wantFiles []string
			for _, session := range tt.wantFiles {
				wantFiles = append(wantFiles, srv.slotFile(session))
			}
			slices.Sort(wantFiles)
			if got := remainingNames(t, srv.slotSavePath); !slices.Equal(got, wantFiles) {
				t.Errorf("files %q, want %q", got, wantFiles)
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

func TestStopSavesUnsavedSlot(t *testing.T) {
	tests := []struct {
		name     string
		requests []string
		want     []string
	}{
		{
			name:     "unsaved session is saved",
			requests: []string{"a"},
			want:     []string{"save a"},
		},
		{
			name:     "session-less request leaves nothing to save",
			requests: []string{""},
			want:     nil,
		},
		{
			name:     "session saved on switch is not saved again",
			requests: []string{"a", "b"},
			want:     []string{"save a", "save b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, fake := newSlotTestServer(t, nil, nil, time.Second, 0)
			startHelperProcess(t, srv)
			runSlotRequests(t, srv, tt.requests)
			srv.Stop(context.Background())
			if got, want := fake.recorded(), expectedActions(srv, tt.want); !slices.Equal(got, want) {
				t.Errorf("got %q, want %q", got, want)
			}
			if !srv.Exited() {
				t.Error("process still running after Stop")
			}
		})
	}
}

func TestSlotFileNamesModelAndSession(t *testing.T) {
	tests := []struct {
		selector string
		session  string
		want     string
	}{
		{"qwen3.6-35b", "simple", "qwen3.6-35b--a7a39b72f29718e6.bin"},
		{"qwen3.6-35b", "a/b", "qwen3.6-35b--c14cddc033f64b9d.bin"},
		{"qwen3.6-35b", "a:b", "qwen3.6-35b--6783a31eabf68ccc.bin"},
		{"org/model", "simple", "org--model--a7a39b72f29718e6.bin"},
	}

	for _, tt := range tests {
		t.Run(tt.selector+" "+tt.session, func(t *testing.T) {
			srv := &Server{selector: tt.selector}
			if got := srv.slotFile(tt.session); got != tt.want {
				t.Errorf("slotFile(%q) = %q, want %q", tt.session, got, tt.want)
			}
		})
	}
}

func TestSlotFileSeparatesModelsAndSessions(t *testing.T) {
	type slotOwner struct {
		selector string
		session  string
	}
	tests := []struct {
		name  string
		first slotOwner
		other slotOwner
	}{
		{"different models", slotOwner{"qwen3.6-35b", "session"}, slotOwner{"lfm2.5-24b", "session"}},
		{"same architecture models", slotOwner{"qwen3.6-35b", "session"}, slotOwner{"cyber-tiel", "session"}},
		{"selector and session split differently", slotOwner{"org/model", "simple"}, slotOwner{"org", "model/simple"}},
		{"sessions differing only in unsafe characters", slotOwner{"m", "a/b"}, slotOwner{"m", "a:b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := (&Server{selector: tt.first.selector}).slotFile(tt.first.session)
			other := (&Server{selector: tt.other.selector}).slotFile(tt.other.session)
			if first == other {
				t.Errorf("%+v and %+v share slot file %q", tt.first, tt.other, first)
			}
		})
	}
}

func TestSlotFileStaysPathSafe(t *testing.T) {
	for _, session := range []string{"a/b", "a\\b", "../escape", "with unicode ñ", ""} {
		name := (&Server{selector: "org/model"}).slotFile(session)
		if strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, slotFileExtension) {
			t.Errorf("slotFile(%q) = %q is not a plain %s file name", session, name, slotFileExtension)
		}
	}
}

func TestSanitizeFileNameStaysPathSafe(t *testing.T) {
	for _, id := range []string{"a/b", "a\\b", "../escape", "a b/c", "hijau:12"} {
		name := sanitizeFileName(id)
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("sanitizeFileName(%q) = %q still contains path separators", id, name)
		}
	}
}

func newIdleTestServer(t *testing.T, idleTimeout time.Duration) *Server {
	t.Helper()
	srv := NewServer("m", &config.LlamaCppEntry{StopTimeout: time.Second}, config.ServerConfig{}, logger.New("ERROR"))
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

	srv.Stop(context.Background())

	if srv.idleTimer != nil {
		t.Error("idle timer still armed after Stop")
	}
	if srv.EnterRequest() {
		t.Error("EnterRequest accepted a request on a stopped server")
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
		name           string
		mode           string
		inferenceDelay time.Duration
		cancelOnReady  bool
	}{
		{name: "server never becomes ready", mode: fakeLlamaServerModeHang},
		{name: "server ready but waiting for the inference delay", mode: fakeLlamaServerModeServe, inferenceDelay: time.Minute, cancelOnReady: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(fakeLlamaServerEnv, tt.mode)
			srv := NewServer("m", &config.LlamaCppEntry{Bin: os.Args[0], Port: freePort(t)}, config.ServerConfig{InferenceDelay: tt.inferenceDelay}, logger.New("ERROR"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			started := make(chan error, 1)
			go func() { started <- srv.Start(ctx) }()

			if tt.cancelOnReady {
				waitForInferenceDelay(t, srv.URL()+"/health")
			}
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
	srv := NewServer("m", &config.LlamaCppEntry{StopTimeout: time.Second}, config.ServerConfig{IdleTimeout: &disabled}, logger.New("ERROR"))
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

func waitForInferenceDelay(t *testing.T, healthURL string) {
	t.Helper()
	waitUntil(t, handleTimeout, "server ready", func() bool {
		return respondsOK(healthURL)
	})
	<-time.After(2 * healthPollInterval)
}

func respondsOK(url string) bool {
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func argValue(args []string, name string) string {
	index := slices.Index(args, name)
	if index < 0 || index+1 >= len(args) {
		return ""
	}
	return args[index+1]
}
