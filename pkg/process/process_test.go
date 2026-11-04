package process

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reekoheek/aim/internal/logger"
)

const (
	fakeProcessEnv            = "AIM_FAKE_PROCESS"
	fakeProcessModeServe      = "serve"
	fakeProcessModeHang       = "hang"
	fakeProcessModeExit       = "exit"
	fakeProcessOrphanInterval = 100 * time.Millisecond
	fakeHealthPathFlag        = "--fake-health"
	fakeHostFlag              = "--fake-host"
	fakePortFlag              = "--fake-port"
	startWithin               = 5 * time.Second
)

var testLogger = logger.New("ERROR")

func TestMain(m *testing.M) {
	switch os.Getenv(fakeProcessEnv) {
	case fakeProcessModeServe:
		go exitWhenOrphaned(os.Getppid())
		runFakeServer(os.Args[1:])
	case fakeProcessModeHang:
		exitWhenOrphaned(os.Getppid())
	case fakeProcessModeExit:
		os.Exit(1)
	default:
		os.Exit(m.Run())
	}
}

func exitWhenOrphaned(parent int) {
	for os.Getppid() == parent {
		time.Sleep(fakeProcessOrphanInterval)
	}
	os.Exit(0)
}

func runFakeServer(args []string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+argValue(args, fakeHealthPathFlag), func(w http.ResponseWriter, r *http.Request) {})
	http.ListenAndServe(net.JoinHostPort(argValue(args, fakeHostFlag), argValue(args, fakePortFlag)), mux)
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

func fakeSpec(t *testing.T, healthPath string, servedHealthPath string) Spec {
	t.Helper()
	port := freePort(t)
	return Spec{
		Name:        "fake server",
		Selector:    "m",
		Bin:         os.Args[0],
		Port:        port,
		Args:        []string{fakeHostFlag, DefaultHost, fakePortFlag, strconv.Itoa(port), fakeHealthPathFlag, servedHealthPath},
		HealthPath:  healthPath,
		StopTimeout: 2 * time.Second,
	}
}

func TestNewDefaults(t *testing.T) {
	tests := []struct {
		name            string
		spec            Spec
		wantHost        string
		wantStopTimeout time.Duration
	}{
		{name: "defaults", spec: Spec{}, wantHost: DefaultHost, wantStopTimeout: defaultStopTimeout},
		{name: "configured", spec: Spec{Host: "0.0.0.0", StopTimeout: 30 * time.Second}, wantHost: "0.0.0.0", wantStopTimeout: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := New(tt.spec, testLogger)
			if got.spec.Host != tt.wantHost {
				t.Errorf("host %q, want %q", got.spec.Host, tt.wantHost)
			}
			if got.spec.StopTimeout != tt.wantStopTimeout {
				t.Errorf("stop timeout %v, want %v", got.spec.StopTimeout, tt.wantStopTimeout)
			}
		})
	}
}

func TestStart(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		healthPath   string
		servedHealth string
		startTimeout time.Duration
		missingBin   bool
		wantErr      string
	}{
		{name: "ready on health path", mode: fakeProcessModeServe, healthPath: "/health", servedHealth: "/health", startTimeout: startWithin},
		{name: "ready on another health path", mode: fakeProcessModeServe, healthPath: "/v1/models", servedHealth: "/v1/models", startTimeout: startWithin},
		{name: "unlimited start timeout still becomes ready", mode: fakeProcessModeServe, healthPath: "/health", servedHealth: "/health", startTimeout: unlimitedStartAfter},
		{name: "health path never answers ok", mode: fakeProcessModeServe, healthPath: "/health", servedHealth: "/other", startTimeout: time.Second, wantErr: "did not become ready within 1s"},
		{name: "process exits during startup", mode: fakeProcessModeExit, healthPath: "/health", startTimeout: startWithin, wantErr: "server exited during startup"},
		{name: "binary not found", mode: fakeProcessModeServe, startTimeout: startWithin, missingBin: true, wantErr: "fake server binary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(fakeProcessEnv, tt.mode)
			spec := fakeSpec(t, tt.healthPath, tt.servedHealth)
			spec.StartTimeout = tt.startTimeout
			if tt.missingBin {
				spec.Bin = filepath.Join(t.TempDir(), "missing")
			}
			p := New(spec, testLogger)
			defer p.Stop(context.Background())

			err := p.Start(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got error %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if p.Exited() {
				t.Error("process exited after a successful start")
			}
		})
	}
}

func TestStartAbortsWhenContextCanceled(t *testing.T) {
	tests := []struct {
		name         string
		startTimeout time.Duration
	}{
		{name: "with start timeout", startTimeout: startWithin},
		{name: "without start timeout", startTimeout: unlimitedStartAfter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(fakeProcessEnv, fakeProcessModeHang)
			spec := fakeSpec(t, "/health", "/health")
			spec.StartTimeout = tt.startTimeout
			p := New(spec, testLogger)
			ctx, cancel := context.WithCancel(context.Background())

			started := make(chan error, 1)
			go func() { started <- p.Start(ctx) }()
			cancel()

			select {
			case err := <-started:
				if err == nil {
					t.Fatal("Start succeeded although its context was canceled")
				}
			case <-time.After(startWithin):
				t.Fatal("canceled startup did not return")
			}
			if !p.Exited() {
				t.Error("process still running after canceled startup")
			}
		})
	}
}

func TestStartRejectsPortInUse(t *testing.T) {
	t.Setenv(fakeProcessEnv, fakeProcessModeServe)
	spec := fakeSpec(t, "/health", "/health")
	ln, err := net.Listen("tcp", net.JoinHostPort(DefaultHost, strconv.Itoa(spec.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	err = New(spec, testLogger).Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already in use (leftover fake server?)") {
		t.Errorf("got error %v, want port in use", err)
	}
}

func TestStopEndsProcess(t *testing.T) {
	t.Setenv(fakeProcessEnv, fakeProcessModeServe)
	p := New(fakeSpec(t, "/health", "/health"), testLogger)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	p.Stop(context.Background())

	if !p.Exited() {
		t.Error("process still running after Stop")
	}
	select {
	case <-p.Done():
	default:
		t.Error("Done not closed after Stop")
	}
}

func TestIsMuted(t *testing.T) {
	tests := []struct {
		name  string
		muted []string
		line  string
		want  bool
	}{
		{name: "nothing muted", muted: nil, line: "using config file x", want: false},
		{name: "line contains muted text", muted: []string{"using config file"}, line: "info: using config file /etc/x", want: true},
		{name: "line without muted text", muted: []string{"using config file"}, line: "model loaded", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(Spec{MutedLogLines: tt.muted}, testLogger)
			if got := p.isMuted(tt.line); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

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
			got := FlagArgs(tt.flags)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
