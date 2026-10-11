package llamacpp

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
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
)

// Server manages a llama.cpp server process.
type Server struct {
	bin        string
	port       int
	host       string
	flags      config.LlamaCppFlags
	selector   string
	cmd        *exec.Cmd
	logger     *logger.Logger
	mu         sync.Mutex
	started    bool
	startErr   error
}

// NewServer creates a new llama.cpp server manager.
func NewServer(selector string, entry *config.LlamaCppEntry, l *logger.Logger) *Server {
	return &Server{
		bin:      entry.Bin,
		port:     entry.Flags.Port,
		host:     defaultHost,
		flags:    entry.Flags,
		selector: selector,
		logger:   l,
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

	// Log full command
	s.logger.Info("starting llama.cpp server",
		slog.String("cmd", fmt.Sprintf("%s %s", binPath, strings.Join(args, " "))),
	)

	s.cmd = exec.CommandContext(context.Background(), binPath, args...)
	s.cmd.Stdout = os.Stdout
	stderr, err := s.cmd.StderrPipe()
	if err != nil {
		s.startErr = fmt.Errorf("stderr pipe: %w", err)
		s.logger.LogError("stderr pipe", s.startErr)
		return s.startErr
	}
	go func() {
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

	// Wait for server to be ready
	ready := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		deadline := time.After(startTimeout)
		for {
			select {
			case <-deadline:
				ready <- fmt.Errorf("server did not become ready within %v", startTimeout)
				return
			default:
				resp, err := client.Get(fmt.Sprintf("http://%s:%d/health", s.host, s.port))
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == 200 {
						ready <- nil
						return
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
	}()

	if err := <-ready; err != nil {
		s.startErr = err
		s.cmd.Process.Kill()
		s.logger.LogError("llama.cpp server failed to start", err)
		return err
	}

	s.started = true
	s.logger.Info("llama.cpp server ready", slog.String("addr", fmt.Sprintf("%s:%d", s.host, s.port)))
	return nil
}

// Stop stops the llama.cpp server process.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil && s.cmd.Process != nil {
		s.logger.Info("stopping llama.cpp server", slog.String("pid", fmt.Sprintf("%d", s.cmd.Process.Pid)))
		s.cmd.Process.Signal(os.Interrupt)
		go func() {
			time.Sleep(5 * time.Second)
			s.cmd.Process.Kill()
		}()
	}
	s.started = false
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
