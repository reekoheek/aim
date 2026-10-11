package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

const (
	defaultPort  = 8082
	defaultHost  = "127.0.0.1"
	startTimeout = 30 * time.Second
	stopTimeout  = 5 * time.Second

	// CacheSlot is the slot that session-cached requests are pinned to.
	CacheSlot = 0
)

// slotClient talks to the slot save/restore endpoints. The timeout keeps a
// hung server from holding the slot lock forever.
var slotClient = &http.Client{Timeout: time.Minute}

// Server manages a llama.cpp server process.
type Server struct {
	bin          string
	port         int
	host         string
	flags        config.LlamaCppFlags
	selector     string
	slotSavePath string
	slotMu       sync.Mutex // serializes requests while slot caching is on
	slotSession  string     // session currently loaded in CacheSlot, if known
	cmd          *exec.Cmd
	done         chan struct{} // closed once the process has exited
	logger       *logger.Logger
	mu           sync.Mutex
	started      bool
	startErr     error
}

// NewServer creates a new llama.cpp server manager.
func NewServer(selector string, entry *config.LlamaCppEntry, slotSavePath string, l *logger.Logger) *Server {
	return &Server{
		bin:          entry.Bin,
		port:         entry.Flags.Port,
		host:         defaultHost,
		flags:        entry.Flags,
		selector:     selector,
		slotSavePath: slotSavePath,
		logger:       l,
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

	// Build command arguments
	args := []string{
		"-hf", s.flags.HF,
		"--host", s.host,
		"--port", fmt.Sprintf("%d", s.flags.Port),
		"--ctx-size", fmt.Sprintf("%d", s.flags.C),
		"--flash-attn", s.flags.FA,
		"--gpu-layers", fmt.Sprintf("%d", s.flags.NGL),
		"--parallel", fmt.Sprintf("%d", s.flags.NP),
		"--cache-type-k", s.flags.CTK,
		"--cache-type-v", s.flags.CTV,
		"--log-colors", "off",
		"--temp", fmt.Sprintf("%f", s.flags.Temp),
		"--top-p", fmt.Sprintf("%f", s.flags.TopP),
		"--top-k", fmt.Sprintf("%d", s.flags.TopK),
		"--min-p", fmt.Sprintf("%f", s.flags.MinP),
		"--presence-penalty", fmt.Sprintf("%f", s.flags.PresencePenalty),
		"--repeat-penalty", fmt.Sprintf("%f", s.flags.RepeatPenalty),
	}

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

// Stop stops the llama.cpp server process and waits for it to exit, killing
// it if it does not stop within stopTimeout.
func (s *Server) Stop() {
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
		case <-time.After(stopTimeout):
			s.logger.Info("llama.cpp server did not stop in time, killing", slog.Int("pid", s.cmd.Process.Pid))
			s.cmd.Process.Kill()
			<-s.done
		}
	}
	s.started = false
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
func (s *Server) GetFlags() config.LlamaCppFlags {
	return s.flags
}

// SlotCacheEnabled reports whether session KV caches are persisted to disk.
func (s *Server) SlotCacheEnabled() bool {
	return s.slotSavePath != ""
}

// AcquireSlot locks CacheSlot for one request and, for a non-empty sessionID,
// restores the session's saved KV cache into it unless the slot already holds
// it. The returned release must be called exactly once; with save true it
// writes the slot back to the session's file before unlocking.
func (s *Server) AcquireSlot(sessionID string) (release func(save bool)) {
	start := time.Now()
	s.slotMu.Lock()
	acquired := time.Now()
	s.logger.Info("slot acquired",
		slog.Int("slot", CacheSlot),
		slog.String("session", sessionID),
		slog.Duration("waited", acquired.Sub(start)),
	)

	if sessionID != "" && s.slotSession != sessionID {
		s.restoreSlot(sessionID)
	}
	// Until the request completes the slot content is unknown.
	s.slotSession = ""

	return func(save bool) {
		defer s.slotMu.Unlock()

		saved := false
		if save && sessionID != "" {
			// The slot holds this session even if writing it to disk fails.
			s.slotSession = sessionID
			file := s.slotFile(sessionID)
			if err := s.slotAction("save", file); err != nil {
				s.logger.LogError("failed to save slot", err, slog.String("session", sessionID), slog.String("file", file))
			} else {
				saved = true
			}
		}

		s.logger.Info("slot released",
			slog.Int("slot", CacheSlot),
			slog.String("session", sessionID),
			slog.Bool("saved", saved),
			slog.Duration("held", time.Since(acquired)),
		)
	}
}

// restoreSlot loads the session's saved KV cache into CacheSlot, if any.
func (s *Server) restoreSlot(sessionID string) {
	file := s.slotFile(sessionID)
	if _, err := os.Stat(filepath.Join(s.slotSavePath, file)); err != nil {
		return
	}
	if err := s.slotAction("restore", file); err != nil {
		s.logger.LogError("failed to restore slot", err, slog.String("session", sessionID), slog.String("file", file))
	}
}

// slotFile names the session's cache file. llama-server only accepts a bare
// filename relative to --slot-save-path, and a cache is only valid for the
// model that produced it, so the name hashes both.
func (s *Server) slotFile(sessionID string) string {
	sum := sha256.Sum256([]byte(s.selector + "\x00" + sessionID))
	return hex.EncodeToString(sum[:16]) + ".bin"
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
