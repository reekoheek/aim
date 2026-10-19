package llamaserver

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

const (
	defaultPort        = 8082
	defaultHost        = "127.0.0.1"
	startTimeout       = 30 * time.Second
	healthPollInterval = 500 * time.Millisecond
	defaultStopTimeout = 5 * time.Second
	defaultIdleTimeout = 5 * time.Minute
	minParallelSlots   = 1
)

type Server struct {
	bin           string
	port          int
	host          string
	flags         map[string]any
	parallelSlots int
	slotContext   int
	stopTimeout   time.Duration
	idleTimeout   time.Duration
	selector      string
	onIdle        func(*Server)
	cmd           *exec.Cmd
	done          chan struct{}
	logger        *logger.Logger
	mu            sync.Mutex
	started       bool
	startErr      error

	idleMu         sync.Mutex
	idleTimer      *time.Timer
	idleGeneration uint64
	active         int
	stopping       bool
}

func NewServer(selector string, entry *config.LlamaCppEntry, server config.ServerConfig, l *logger.Logger, onIdle func(*Server)) *Server {
	return &Server{
		bin:           entry.Bin,
		port:          entry.Port,
		host:          defaultHost,
		flags:         entry.Flags,
		parallelSlots: max(entry.Slots, minParallelSlots),
		slotContext:   entry.Ctx,
		stopTimeout:   cmp.Or(entry.StopTimeout, defaultStopTimeout),
		idleTimeout:   idleTimeoutDuration(server.IdleTimeout),
		selector:      selector,
		onIdle:        onIdle,
		logger:        l,
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

func (s *Server) Stop() {
	s.idleMu.Lock()
	s.stopping = true
	s.disarmIdleTimer()
	s.idleMu.Unlock()

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
	s.onIdle(s)
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

func (s *Server) processDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.done
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
	args := append([]string{
		"--offline",
		"--no-cors-credentials",
		"--cors-origins", "localhost",
		"--no-ui",
	}, flagArgs(s.flags)...)
	args = append(args, s.slotArgs()...)
	return append(args,
		"--host", s.host,
		"--port", strconv.Itoa(s.port),
		"--log-colors", "off",
	)
}

func (s *Server) slotArgs() []string {
	args := []string{"-np", strconv.Itoa(s.parallelSlots)}
	if s.slotContext > 0 {
		args = append(args, "-c", strconv.Itoa(s.slotContext*s.parallelSlots))
	}
	return args
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
