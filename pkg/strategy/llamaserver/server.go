package llamaserver

import (
	"bufio"
	"bytes"
	"cmp"
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
	healthPollInterval    = 500 * time.Millisecond
	defaultStopTimeout    = 5 * time.Second
	defaultInferenceDelay = 1 * time.Second
	defaultIdleTimeout    = 5 * time.Minute

	CacheSlot = 0
)

var slotClient = &http.Client{Timeout: time.Minute}

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
	slot           chan struct{}
	slotSession    string
	slotUnsaved    bool
	cmd            *exec.Cmd
	done           chan struct{}
	logger         *logger.Logger
	mu             sync.Mutex
	started        bool
	startErr       error

	idleMu         sync.Mutex
	idleTimer      *time.Timer
	idleGeneration uint64
	active         int
	stopping       bool
}

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

func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return s.startErr
	}

	binPath, err := exec.LookPath(s.bin)
	if err != nil {
		s.startErr = fmt.Errorf("llama.cpp binary %q not found: %w", s.bin, err)
		s.logger.LogError("llama.cpp binary not found", s.startErr)
		return s.startErr
	}

	args := s.args()

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

	s.logger.Info("starting llama.cpp server",
		slog.String("cmd", fmt.Sprintf("%s %s", binPath, strings.Join(args, " "))),
	)

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

	done := make(chan struct{})
	s.done = done
	go func() {
		<-logDone
		err := s.cmd.Wait()
		s.logger.Info("llama.cpp server exited", slog.String("model", s.selector), slog.Any("status", err))
		close(done)
	}()

	if err := s.waitReady(ctx, done); err != nil {
		return s.abortStart(err, done)
	}

	s.logger.Info("llama.cpp server ready", slog.String("addr", addr))

	if err := s.waitInferenceDelay(ctx, done); err != nil {
		return s.abortStart(err, done)
	}

	s.started = true

	if s.startIdleTimer() {
		s.logger.Info("llama.cpp server idle timeout", slog.Duration("timeout", s.idleTimeout))
	}

	return nil
}

func (s *Server) abortStart(err error, done <-chan struct{}) error {
	s.startErr = err
	s.cmd.Process.Kill()
	<-done
	s.logger.LogError("llama.cpp server failed to start", err)
	return err
}

func (s *Server) waitInferenceDelay(ctx context.Context, done <-chan struct{}) error {
	if s.inferenceDelay <= 0 {
		return nil
	}
	s.logger.Info("waiting for inference delay", slog.Duration("delay", s.inferenceDelay))
	select {
	case <-time.After(s.inferenceDelay):
		return nil
	case <-done:
		return fmt.Errorf("server exited during startup")
	case <-ctx.Done():
		return fmt.Errorf("server startup canceled: %w", ctx.Err())
	}
}

func (s *Server) waitReady(ctx context.Context, done <-chan struct{}) error {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.After(startTimeout)
	for {
		resp, err := s.healthCheck(ctx, client)
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
		case <-ctx.Done():
			return fmt.Errorf("server startup canceled: %w", ctx.Err())
		case <-time.After(healthPollInterval):
		}
	}
}

func (s *Server) healthCheck(ctx context.Context, client *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL()+"/health", nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func (s *Server) Stop(ctx context.Context) {
	s.idleMu.Lock()
	s.stopping = true
	s.disarmIdleTimer()
	s.idleMu.Unlock()

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

func (s *Server) EnterRequest() bool {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()

	if s.stopping {
		return false
	}
	s.disarmIdleTimer()
	s.active++
	return true
}

func (s *Server) ExitRequest() {
	s.idleMu.Lock()
	defer s.idleMu.Unlock()

	s.active--
	if s.active > 0 || s.idleTimeout <= 0 || s.stopping {
		return
	}
	s.armIdleTimer()
}

func (s *Server) startIdleTimer() bool {
	if s.idleTimeout <= 0 {
		return false
	}
	s.idleMu.Lock()
	s.armIdleTimer()
	s.idleMu.Unlock()
	return true
}

func (s *Server) armIdleTimer() {
	s.disarmIdleTimer()
	generation := s.idleGeneration
	s.idleTimer = time.AfterFunc(s.idleTimeout, func() {
		s.stopIfIdle(generation)
	})
}

func (s *Server) disarmIdleTimer() {
	s.idleGeneration++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Server) stopIfIdle(generation uint64) {
	s.idleMu.Lock()
	if generation != s.idleGeneration || s.active > 0 || s.stopping {
		s.idleMu.Unlock()
		return
	}
	s.stopping = true
	s.idleTimer = nil
	s.idleMu.Unlock()

	s.logger.Info("stopping llama.cpp server after idle", slog.Duration("timeout", s.idleTimeout))
	s.Stop(context.Background())
}

func idleTimeoutDuration(seconds *int) time.Duration {
	if seconds == nil {
		return defaultIdleTimeout
	}
	return time.Duration(*seconds) * time.Second
}

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

func (s *Server) URL() string {
	return fmt.Sprintf("http://%s:%d", s.host, s.port)
}

func (s *Server) Selector() string {
	return s.selector
}

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

func (s *Server) SlotCacheEnabled() bool {
	return s.slotSavePath != ""
}

func (s *Server) AcquireSlot(ctx context.Context, sessionID string) (release func(), err error) {
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

	return func() {
		defer func() { <-s.slot }()

		if sessionID != "" {
			s.slotSession = sessionID
			s.slotUnsaved = true
		}

		s.logger.Info("slot released",
			slog.Int("slot", CacheSlot),
			slog.String("session", sessionID),
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

func (s *Server) slotFile(sessionID string) string {
	return sanitizeFileName(s.selector) + slotFileSeparator + sessionDigest(sessionID) + slotFileExtension
}

const (
	slotFileSeparator   = "--"
	sessionDigestLength = 16
)

func sessionDigest(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:])[:sessionDigestLength]
}

var unsafeFileNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeFileName(id string) string {
	return unsafeFileNameChars.ReplaceAllString(id, "--")
}

func (s *Server) slotAction(action string, file string) error {
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
