package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
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
			want:  []string{"--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name: "managed flags come after model flags",
			entry: &config.LlamaCppEntry{
				Port:  23456,
				Flags: map[string]any{"-ngl": 999},
			},
			want: []string{
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

type slotRequest struct {
	session string
	keep    bool
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

func runSlotRequests(t *testing.T, srv *Server, requests []slotRequest) {
	t.Helper()
	for _, request := range requests {
		release, err := srv.AcquireSlot(context.Background(), request.session)
		if err != nil {
			t.Fatalf("AcquireSlot(%q): %v", request.session, err)
		}
		release(request.keep)
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
		requests    []slotRequest
		want        []string
	}{
		{
			name:     "same session reuses the slot without save or restore",
			requests: []slotRequest{{"a", true}, {"a", true}, {"a", true}},
			want:     nil,
		},
		{
			name:     "switching session saves the previous one",
			requests: []slotRequest{{"a", true}, {"b", true}},
			want:     []string{"save a"},
		},
		{
			name:     "switching back restores the saved session",
			requests: []slotRequest{{"a", true}, {"b", true}, {"a", true}},
			want:     []string{"save a", "save b", "restore a"},
		},
		{
			name:     "existing cache is restored on the first request",
			existing: []string{"a"},
			requests: []slotRequest{{"a", true}, {"a", true}},
			want:     []string{"restore a"},
		},
		{
			name:     "session not kept is not saved",
			requests: []slotRequest{{"a", false}, {"b", true}},
			want:     nil,
		},
		{
			name:        "session is saved after a failed restore",
			existing:    []string{"a"},
			failRestore: []string{"a"},
			requests:    []slotRequest{{"a", true}, {"b", true}},
			want:        []string{"restore a", "save a"},
		},
		{
			name:     "session-less request evicts the session",
			requests: []slotRequest{{"a", true}, {"", true}, {"a", true}},
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
		requests  []slotRequest
		want      []string
		wantFiles []string
	}{
		{
			name:      "least recently used session removed over the limit",
			maxFiles:  2,
			requests:  []slotRequest{{"a", true}, {"b", true}, {"c", true}, {"d", true}},
			want:      []string{"save a", "save b", "save c"},
			wantFiles: []string{"b", "c"},
		},
		{
			name:      "requested session counts as used before the limit is applied",
			maxFiles:  2,
			requests:  []slotRequest{{"a", true}, {"b", true}, {"c", true}, {"a", true}},
			want:      []string{"save a", "save b", "save c", "restore a"},
			wantFiles: []string{"a", "c"},
		},
		{
			name:      "requested session is restored before the limit is applied",
			maxFiles:  1,
			requests:  []slotRequest{{"a", true}, {"b", true}, {"a", true}},
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
		requests []slotRequest
		want     []string
	}{
		{
			name:     "unsaved session is saved",
			requests: []slotRequest{{"a", true}},
			want:     []string{"save a"},
		},
		{
			name:     "session not kept is not saved",
			requests: []slotRequest{{"a", false}},
			want:     nil,
		},
		{
			name:     "session saved on switch is not saved again",
			requests: []slotRequest{{"a", true}, {"b", false}},
			want:     []string{"save a"},
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

func TestSlotFileNormalizesUnsafeCharacters(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"simple", "simple.bin"},
		{"with slash", "with--slash.bin"},
		{"with backslash", "with--backslash.bin"},
		{"with spaces", "with--spaces.bin"},
		{"with colon", "with--colon.bin"},
		{"a.b.c", "a.b.c.bin"},
		{"with unicode ñ", "with--unicode--.bin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{}
			if got := srv.slotFile(tt.name); got != tt.want {
				t.Errorf("slotFile(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
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
