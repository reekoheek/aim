package llamacpp

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

const (
	defaultPort           = 8082
	defaultHost           = "127.0.0.1"
	startTimeout          = 30 * time.Second
	defaultStopTimeout    = 5 * time.Second
	defaultInferenceDelay = 1 * time.Second
	defaultIdleTimeout    = 5 * time.Minute

	// CacheSlot is the slot that session-cached requests are pinned to.
	CacheSlot = 0
)

// slotClient talks to the slot save/restore endpoints. The timeout keeps a
// hung server from holding the slot lock forever.
var slotClient = &http.Client{Timeout: time.Minute}

// Server manages a llama.cpp server process.
type Server struct {
	bin            string
	port           int
	host           string
	flags          map[string]any
	stopTimeout    time.Duration
	idleTimeout    time.Duration
	selector       string
	slotSavePath   string
	slotExpiration time.Duration
	slotMaxFiles   int
	inferenceDelay time.Duration
	slot           chan struct{} // held by one request at a time while slot caching is on
	slotSession    string        // session currently loaded in CacheSlot, if known
	slotUnsaved    bool
	cmd            *exec.Cmd
	done           chan struct{} // closed once the process has exited
	logger         *logger.Logger
	mu             sync.Mutex
	started        bool
	startErr       error

	idleMu    sync.Mutex
	idleTimer *time.Timer
	active    int
}

// NewServer creates a new llama.cpp server manager.
func NewServer(selector string, entry *config.LlamaCppEntry, server config.ServerConfig, l *logger.Logger) *Server {
	return &Server{
		bin:            entry.Bin,
		port:           entry.Port,
		host:           defaultHost,
		flags:          entry.Flags,
		stopTimeout:    cmp.Or(entry.StopTimeout, defaultStopTimeout),
		inferenceDelay: cmp.Or(server.InferenceDelay, defaultInferenceDelay),
		idleTimeout:    idleTimeoutDuration(server.IdleTimeout),
		selector:       selector,
		slotSavePath:   server.SaveSlotPath,
		slotExpiration: server.SlotExpiration,
		slotMaxFiles:   server.SlotMaxFiles,
		slot:           make(chan struct{}, 1),
		logger:         l,
	}
}

// Start starts the llama.cpp server process and waits for it to be ready.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return s.startErr
	}

	// Find llama-server binary
	binPath, err := exec.LookPath(s.bin)
	if err != nil {
		s.startErr = fmt.Errorf("llama.cpp binary %q not found: %w", s.bin, err)
		s.logger.LogError("llama.cpp binary not found", s.startErr)
		return s.startErr
	}

	args := s.args()

	// llama-server does not create the slot save dir itself.
	if s.slotSavePath != "" {
		dir, err := filepath.Abs(s.slotSavePath)
		if err == nil {
			err = os.MkdirAll(dir, 0o755)
		}
		if err != nil {
			s.startErr = fmt.Errorf("slot save path %q: %w", s.slotSavePath, err)
			s.logger.LogError("invalid slot save path", s.startErr)
			return s.startErr
		}
		s.slotSavePath = dir
		args = append(args, "--slot-save-path", dir)
	}

	// Log full command
	s.logger.Info("starting llama.cpp server",
		slog.String("cmd", fmt.Sprintf("%s %s", binPath, strings.Join(args, " "))),
	)

	// A leftover llama-server on our port would answer the health check
	// below while our own process fails to bind.
	addr := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.startErr = fmt.Errorf("port %d is already in use (leftover llama-server?): %w", s.port, err)
		s.logger.LogError("llama.cpp server port in use", s.startErr)
		return s.startErr
	}
	ln.Close()

	s.cmd = exec.CommandContext(context.Background(), binPath, args...)
	s.cmd.Stdout = os.Stdout
	stderr, err := s.cmd.StderrPipe()
	if err != nil {
		s.startErr = fmt.Errorf("stderr pipe: %w", err)
		s.logger.LogError("stderr pipe", s.startErr)
		return s.startErr
	}
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "using config file") {
				continue
			}
			s.logger.Info(s.selector, slog.String("msg", line))
		}
	}()

	if err := s.cmd.Start(); err != nil {
		s.startErr = fmt.Errorf("start llama.cpp server: %w", err)
		return s.startErr
	}

	// Reap the process so it never lingers as a zombie, and let Stop and
	// Exited observe its exit. Wait closes the stderr pipe, so drain it first.
	done := make(chan struct{})
	s.done = done
	go func() {
		<-logDone
		err := s.cmd.Wait()
		s.logger.Info("llama.cpp server exited", slog.String("model", s.selector), slog.Any("status", err))
		close(done)
	}()

	if err := s.waitReady(done); err != nil {
		s.startErr = err
		s.cmd.Process.Kill()
		<-done
		s.logger.LogError("llama.cpp server failed to start", err)
		return err
	}

	s.started = true
	s.logger.Info("llama.cpp server ready", slog.String("addr", addr))

	if s.inferenceDelay > 0 {
		s.logger.Info("waiting for inference delay", slog.Duration("delay", s.inferenceDelay))
		<-time.After(s.inferenceDelay)
	}

	if s.startIdleTimer() {
		s.logger.Info("llama.cpp server idle timeout", slog.Duration("timeout", s.idleTimeout))
	}

	return nil
}

// waitReady polls /health until the server is up, it exits, or startTimeout
// passes.
func (s *Server) waitReady(done <-chan struct{}) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.After(startTimeout)
	for {
		resp, err := client.Get(s.URL() + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-done:
			return fmt.Errorf("server exited during startup")
		case <-deadline:
			return fmt.Errorf("server did not become ready within %v", startTimeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Stop waits for the request holding CacheSlot, including its pending slot
// save, so neither is cut off, then stops the llama.cpp server process and
// waits for it to exit, killing it if it does not stop within stopTimeout.
// Once ctx is done it stops without waiting for the slot.
func (s *Server) Stop(ctx context.Context) {
	// The slot is kept, so requests still queued for it fail once the
	// process has exited.
	select {
	case s.slot <- struct{}{}:
		if s.done != nil && !s.Exited() {
			s.saveUnsavedSlot()
			s.pruneSlotFiles()
		}
	case <-s.done:
	case <-ctx.Done():
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done == nil {
		return
	}
	select {
	case <-s.done:
	default:
		s.logger.Info("stopping llama.cpp server", slog.Int("pid", s.cmd.Process.Pid))
		s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-s.done:
		case <-time.After(s.stopTimeout):
			s.logger.Info("llama.cpp server did not stop in time, killing", slog.Int("pid", s.cmd.Process.Pid))
			s.cmd.Process.Kill()
			<-s.done
		}
	}
	s.started = false
}

// EnterRequest cancels the idle timer while a request is in flight, so an
// active request never triggers an auto-stop.
func (s *Server) EnterRequest() {
	s.idleMu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.active++
	s.idleMu.Unlock()
}

// ExitRequest restarts the idle timer once no requests are in flight. When the
// timer fires it stops the server after idleTimeout.
func (s *Server) ExitRequest() {
	s.idleMu.Lock()
	s.active--
	if s.active > 0 || s.idleTimeout <= 0 {
		s.idleMu.Unlock()
		return
	}
	s.armIdleTimer()
	s.idleMu.Unlock()
}

// startIdleTimer arms the idle timer for a freshly started server and reports
// whether it was armed (idleTimeout > 0).
func (s *Server) startIdleTimer() bool {
	if s.idleTimeout <= 0 {
		return false
	}
	s.idleMu.Lock()
	s.armIdleTimer()
	s.idleMu.Unlock()
	return true
}

// armIdleTimer installs a timer that stops the server once it has been idle
// for idleTimeout. It re-arms if a request snuck in while the timer was
// pending, so an active request never triggers an auto-stop.
func (s *Server) armIdleTimer() {
	s.idleTimer = time.AfterFunc(s.idleTimeout, func() {
		s.idleMu.Lock()
		if s.active > 0 {
			s.armIdleTimer()
			s.idleMu.Unlock()
			return
		}
		s.idleTimer = nil
		s.idleMu.Unlock()
		s.logger.Info("stopping llama.cpp server after idle", slog.Duration("timeout", s.idleTimeout))
		s.Stop(context.Background())
	})
}

// idleTimeoutDuration converts an idle timeout in seconds to a duration,
// falling back to the default when unset.
func idleTimeoutDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultIdleTimeout
	}
	return time.Duration(seconds) * time.Second
}

// Exited reports whether a started server process has since exited.
func (s *Server) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// URL returns the server URL.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s:%d", s.host, s.port)
}

// Selector returns the model selector for this server.
func (s *Server) Selector() string {
	return s.selector
}

// GetFlags returns the server flags for logging.
func (s *Server) GetFlags() map[string]any {
	return s.flags
}

func (s *Server) args() []string {
	return append(append([]string{
		"--offline",
		"--no-cors-credentials",
		"--cors-origins", "localhost",
		"--no-ui",
	}, flagArgs(s.flags)...),
		"--host", s.host,
		"--port", strconv.Itoa(s.port),
		"--log-colors", "off",
	)
}

func flagArgs(flags map[string]any) []string {
	names := make([]string, 0, len(flags))
	for name := range flags {
		names = append(names, name)
	}
	slices.Sort(names)

	var args []string
	for _, name := range names {
		switch value := flags[name].(type) {
		case bool:
			if value {
				args = append(args, name)
			}
		default:
			args = append(args, name, fmt.Sprint(value))
		}
	}
	return args
}

// SlotCacheEnabled reports whether session KV caches are persisted to disk.
func (s *Server) SlotCacheEnabled() bool {
	return s.slotSavePath != ""
}

func (s *Server) AcquireSlot(ctx context.Context, sessionID string) (release func(keep bool), err error) {
	start := time.Now()
	select {
	case s.slot <- struct{}{}:
	case <-s.done:
		return nil, fmt.Errorf("llama.cpp server has exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	acquired := time.Now()
	s.logger.Info("slot acquired",
		slog.Int("slot", CacheSlot),
		slog.String("session", sessionID),
		slog.Duration("waited", acquired.Sub(start)),
	)

	if s.slotSession != sessionID {
		s.markSlotUsed(sessionID)
		s.saveUnsavedSlot()
		s.slotSession = ""
		if sessionID != "" && s.restoreSlot(sessionID) {
			s.slotSession = sessionID
		}
		s.pruneSlotFiles()
	}

	return func(keep bool) {
		defer func() { <-s.slot }()

		if keep && sessionID != "" {
			s.slotSession = sessionID
			s.slotUnsaved = true
		}

		s.logger.Info("slot released",
			slog.Int("slot", CacheSlot),
			slog.String("session", sessionID),
			slog.Bool("kept", keep),
			slog.Duration("held", time.Since(acquired)),
		)
	}, nil
}

func (s *Server) saveUnsavedSlot() {
	if s.slotSession == "" || !s.slotUnsaved {
		return
	}
	file := s.slotFile(s.slotSession)
	s.slotUnsaved = false
	if err := s.slotAction("save", file); err != nil {
		s.logger.LogError("failed to save slot", err, slog.String("session", s.slotSession), slog.String("file", file))
	}
}

func (s *Server) pruneSlotFiles() {
	pruned, err := pruneSlots(s.slotSavePath, s.slotExpiration, s.slotMaxFiles, time.Now())
	logPrunedSlots(s.logger, s.slotSavePath, pruned, err)
}

func (s *Server) markSlotUsed(sessionID string) {
	if sessionID == "" {
		return
	}
	file := s.slotFile(sessionID)
	if err := touchSlotFile(filepath.Join(s.slotSavePath, file), time.Now()); err != nil {
		s.logger.LogError("failed to mark slot cache as used", err, slog.String("session", sessionID), slog.String("file", file))
	}
}

// restoreSlot loads the session's saved KV cache into CacheSlot, if any, and
// reports false if loading it failed.
func (s *Server) restoreSlot(sessionID string) bool {
	file := s.slotFile(sessionID)
	if _, err := os.Stat(filepath.Join(s.slotSavePath, file)); err != nil {
		return true
	}
	if err := s.slotAction("restore", file); err != nil {
		s.logger.LogError("failed to restore slot", err, slog.String("session", sessionID), slog.String("file", file))
		return false
	}
	return true
}

// slotFile names the session's cache file. llama-server only accepts a bare
// filename relative to --slot-save-path, so the session ID is normalized to a
// safe charset; characters that would break the path become "--". Each session
// always uses the same model, so the model is not part of the name.
func (s *Server) slotFile(sessionID string) string {
	return sanitizeFileName(sessionID) + ".bin"
}

var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFileName maps any character that is unsafe in a filename to "--".
func sanitizeFileName(id string) string {
	return unsafeFileNameChars.ReplaceAllString(id, "--")
}

// slotAction runs a save/restore/erase action on CacheSlot.
func (s *Server) slotAction(action, file string) error {
	start := time.Now()
	url := fmt.Sprintf("http://%s:%d/slots/%d?action=%s", s.host, s.port, CacheSlot, action)
	payload, err := json.Marshal(map[string]string{"filename": file})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	resp, err := slotClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%s slot %d: %w", action, CacheSlot, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s slot %d: status %d: %s", action, CacheSlot, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var res struct {
		NSaved    int `json:"n_saved"`
		NRestored int `json:"n_restored"`
	}
	_ = json.Unmarshal(body, &res)

	s.logger.Info("slot cache", slog.String("action", action),
		slog.Int("slot", CacheSlot),
		slog.String("file", file),
		slog.Int("tokens", res.NSaved+res.NRestored),
		slog.Duration("took", time.Since(start)),
	)
	return nil
}
