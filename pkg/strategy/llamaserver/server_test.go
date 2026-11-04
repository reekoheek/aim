package llamaserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/strategy"
)

const fakeSupervisorCloseTimeout = 10 * time.Second

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
			SaveSlotPath: t.TempDir(),
			IdleTimeout:  &idleTimeoutSeconds,
		},
	}
	for _, model := range models {
		cfg.Models[model] = config.ModelEntry{
			Type: "llama-server",
			LlamaCpp: &config.LlamaCppEntry{
				Lane:        config.DefaultLane,
				Bin:         os.Args[0],
				Port:        freePort(t),
				StopTimeout: 2 * time.Second,
				Slots:       1,
			},
		}
	}
	return cfg
}

func newTestSupervisor(t *testing.T, cfg *config.Config) *lane.Supervisor {
	t.Helper()
	supervisor := lane.NewSupervisor(cfg.DefaultServer().IdleTimeoutDuration(), testLogger)
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			supervisor.Close(ctx)
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(fakeSupervisorCloseTimeout):
			t.Error("supervisor close did not finish")
		}
	})
	return supervisor
}

func currentServer(supervisor *lane.Supervisor) *Server {
	return serverOnLane(supervisor, config.DefaultLane)
}

func serverOnLane(supervisor *lane.Supervisor, laneName string) *Server {
	running := supervisor.Lane(laneName)
	if running == nil {
		return nil
	}
	srv, _ := running.Process().(*Server)
	return srv
}

func currentSlots(supervisor *lane.Supervisor) *slotCache {
	srv := currentServer(supervisor)
	if srv == nil {
		return nil
	}
	return srv.cachedSlots()
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
			got := NewServer("m", tt.entry, config.ServerConfig{}, nil).args()
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServerArgsCannotBeConfiguredAsFlags(t *testing.T) {
	entry := withSlotSavePath(&config.LlamaCppEntry{Port: 12345, Slots: 1, Ctx: 4096}, "/slots")
	for _, arg := range NewServer("m", entry, config.ServerConfig{}, nil).args() {
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

func TestPrepareServerSetsUpSlotsFromSlotCount(t *testing.T) {
	tests := []struct {
		name           string
		slots          int
		saveSlotPath   bool
		wantParallel   string
		wantSavePath   bool
		wantCacheSlots int
	}{
		{name: "one slot", slots: 1, saveSlotPath: true, wantParallel: "1", wantSavePath: true, wantCacheSlots: 1},
		{name: "several slots", slots: 3, saveSlotPath: true, wantParallel: "3", wantSavePath: true, wantCacheSlots: 3},
		{name: "zero slots turn the slot cache off", slots: 0, saveSlotPath: true, wantParallel: "1", wantSavePath: false, wantCacheSlots: 0},
		{name: "slots without a slot save path stay parallel", slots: 2, saveSlotPath: false, wantParallel: "2", wantSavePath: false, wantCacheSlots: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			entry := cfg.Models["a"].LlamaCpp
			entry.Slots = tt.slots
			server := cfg.DefaultServer()
			if !tt.saveSlotPath {
				server.SaveSlotPath = ""
			}

			srv, err := prepareServer("a", entry, server, testLogger)
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer srv.Stop(context.Background())

			args := srv.args()
			if got := argValue(args, "-np"); got != tt.wantParallel {
				t.Errorf("-np %q, want %q", got, tt.wantParallel)
			}
			if got := slices.Contains(args, slotSavePathFlag); got != tt.wantSavePath {
				t.Errorf("passes %s %v, want %v", slotSavePathFlag, got, tt.wantSavePath)
			}
			cacheSlots := 0
			if slots := srv.cachedSlots(); slots != nil {
				cacheSlots = len(slots.slots)
			}
			if cacheSlots != tt.wantCacheSlots {
				t.Errorf("slot cache with %d slots, want %d", cacheSlots, tt.wantCacheSlots)
			}
		})
	}
}

func TestPrepareServerRejectsUnusableSlotSavePath(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	server := cfg.DefaultServer()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	server.SaveSlotPath = filepath.Join(blocker, "slots")

	if _, err := prepareServer("a", cfg.Models["a"].LlamaCpp, server, testLogger); !errors.Is(err, lane.ErrStartFailed) {
		t.Errorf("error %v, want %v", err, lane.ErrStartFailed)
	}
}

func TestStopSavesUnsavedSlots(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	srv, err := prepareServer("a", cfg.Models["a"].LlamaCpp, cfg.DefaultServer(), testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	slots := srv.cachedSlots()
	_, release, err := slots.acquire(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	release()

	srv.Stop(context.Background())

	if !srv.Exited() {
		t.Error("server still running after Stop")
	}
	if _, err := os.Stat(filepath.Join(slots.dir, slots.file("session"))); err != nil {
		t.Errorf("slot not saved on stop: %v", err)
	}
}

func TestRunServerUsesModelLane(t *testing.T) {
	tests := []struct {
		name         string
		lanes        map[string]string
		wantFirstRun bool
	}{
		{name: "models on the same lane swap", lanes: map[string]string{"a": "gpu", "b": "gpu"}, wantFirstRun: false},
		{name: "models on different lanes run together", lanes: map[string]string{"a": "gpu", "b": "cpu"}, wantFirstRun: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a", "b")
			for model, laneName := range tt.lanes {
				cfg.Models[model].LlamaCpp.Lane = laneName
			}
			supervisor := newTestSupervisor(t, cfg)

			first, err := runServer(context.Background(), supervisor, cfg, testLogger, "a")
			if err != nil {
				t.Fatal(err)
			}
			first.Release()
			second, err := runServer(context.Background(), supervisor, cfg, testLogger, "b")
			if err != nil {
				t.Fatal(err)
			}
			second.Release()

			if running := !first.Process.Exited(); running != tt.wantFirstRun {
				t.Errorf("first server running %v, want %v", running, tt.wantFirstRun)
			}
			if got := serverOnLane(supervisor, tt.lanes["b"]); got != second.Process {
				t.Errorf("lane %q holds %v, want the second server", tt.lanes["b"], got)
			}
		})
	}
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
			srv := NewServer("m", &config.LlamaCppEntry{Bin: os.Args[0], Port: freePort(t)}, config.ServerConfig{}, testLogger)
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

const (
	fakeLlamaServerEnv            = "AIM_FAKE_LLAMA_SERVER"
	fakeLlamaServerModeServe      = "serve"
	fakeLlamaServerModeHang       = "hang"
	fakeLlamaServerOrphanInterval = 100 * time.Millisecond
	fakeChatCompletionResponse    = `{"choices":[]}`
	fakeSystemOneResponse         = `{"answers":{}}`
	fakeMessagesResponse          = `{"type":"message","content":[]}`
	fakeCountTokensResponse       = `{"input_tokens":3}`
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
	mux.HandleFunc("POST "+strategy.ChatCompletionsPath, fakeInference(fakeChatCompletionResponse))
	mux.HandleFunc("POST "+strategy.SystemOnePath, fakeInference(fakeSystemOneResponse))
	mux.HandleFunc("POST "+strategy.MessagesPath, fakeInference(fakeMessagesResponse))
	mux.HandleFunc("POST "+strategy.CountTokensPath, fakeInference(fakeCountTokensResponse))
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

func fakeInference(response string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		fmt.Fprint(w, response)
	}
}

func argValue(args []string, name string) string {
	index := slices.Index(args, name)
	if index < 0 || index+1 >= len(args) {
		return ""
	}
	return args[index+1]
}
