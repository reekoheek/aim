package sdserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/strategy"
)

const (
	fakeSDServerEnv            = "AIM_FAKE_SD_SERVER"
	fakeSDServerModeServe      = "serve"
	fakeSDServerOrphanInterval = 100 * time.Millisecond
	fakeSupervisorCloseTimeout = 10 * time.Second
	fakeImagesResponse         = `{"data":[{"b64_json":"aW1n"}]}`
	echoContentTypeHeader      = "X-Fake-Content-Type"
)

var testLogger = logger.New("ERROR")

func TestMain(m *testing.M) {
	switch os.Getenv(fakeSDServerEnv) {
	case fakeSDServerModeServe:
		go exitWhenOrphaned(os.Getppid())
		runFakeSDServer(os.Args[1:])
	default:
		os.Exit(m.Run())
	}
}

func exitWhenOrphaned(parent int) {
	for os.Getppid() == parent {
		time.Sleep(fakeSDServerOrphanInterval)
	}
	os.Exit(0)
}

func runFakeSDServer(args []string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, func(w http.ResponseWriter, r *http.Request) {})
	fakeImages := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set(echoContentTypeHeader, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, fakeImagesResponse)
	}
	mux.HandleFunc("POST "+strategy.ImageGenerationsPath, fakeImages)
	mux.HandleFunc("POST "+strategy.ImageEditsPath, fakeImages)
	http.ListenAndServe(net.JoinHostPort(argValue(args, "--listen-ip"), argValue(args, "--listen-port")), mux)
}

func argValue(args []string, name string) string {
	index := slices.Index(args, name)
	if index < 0 || index+1 >= len(args) {
		return ""
	}
	return args[index+1]
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newFakeSDConfig(t *testing.T, models ...string) *config.Config {
	t.Helper()
	t.Setenv(fakeSDServerEnv, fakeSDServerModeServe)
	idleTimeoutSeconds := 0
	cfg := &config.Config{
		Models: map[string]config.ModelEntry{},
		Server: config.ServerConfig{IdleTimeout: &idleTimeoutSeconds},
	}
	for _, model := range models {
		cfg.Models[model] = config.ModelEntry{
			Type: config.ModelTypeSDServer,
			SDCpp: &config.SDCppEntry{
				Lane:         config.DefaultLane,
				Bin:          os.Args[0],
				Port:         freePort(t),
				StartTimeout: 5 * time.Second,
				StopTimeout:  2 * time.Second,
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

func TestServerArgs(t *testing.T) {
	tests := []struct {
		name  string
		entry *config.SDCppEntry
		want  []string
	}{
		{
			name:  "only listen address",
			entry: &config.SDCppEntry{Port: 12345},
			want:  []string{"--listen-ip", "127.0.0.1", "--listen-port", "12345"},
		},
		{
			name: "flags passed sorted before listen address",
			entry: &config.SDCppEntry{Port: 12345, Flags: map[string]any{
				"--vae":             "/models/ae.safetensors",
				"--diffusion-model": "/models/flux.gguf",
				"--offload-to-cpu":  true,
				"--color":           false,
				"--steps":           4,
			}},
			want: []string{
				"--diffusion-model", "/models/flux.gguf",
				"--offload-to-cpu",
				"--steps", "4",
				"--vae", "/models/ae.safetensors",
				"--listen-ip", "127.0.0.1",
				"--listen-port", "12345",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", tt.entry, testLogger).args()
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
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
			cfg := newFakeSDConfig(t, "a", "b")
			for model, laneName := range tt.lanes {
				cfg.Models[model].SDCpp.Lane = laneName
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
		})
	}
}

func TestRunServerRejectsOtherModelTypes(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelEntry{
		"chat": {Type: config.ModelTypeLlamaServer, LlamaCpp: &config.LlamaCppEntry{}},
	}}
	supervisor := newTestSupervisor(t, cfg)

	_, err := runServer(context.Background(), supervisor, cfg, testLogger, "chat")
	if err == nil {
		t.Fatal("expected error for a llama-server model")
	}
}
