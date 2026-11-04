package process

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
)

const (
	DefaultHost         = "127.0.0.1"
	healthPollInterval  = 500 * time.Millisecond
	healthCheckTimeout  = 5 * time.Second
	defaultStopTimeout  = 5 * time.Second
	unlimitedStartAfter = 0
)

type Spec struct {
	Name          string
	Selector      string
	Bin           string
	Host          string
	Port          int
	Args          []string
	HealthPath    string
	StartTimeout  time.Duration
	StopTimeout   time.Duration
	MutedLogLines []string
}

type Process struct {
	spec     Spec
	cmd      *exec.Cmd
	done     chan struct{}
	logger   *logger.Logger
	mu       sync.Mutex
	started  bool
	startErr error
}

func New(spec Spec, l *logger.Logger) *Process {
	spec.Host = cmp.Or(spec.Host, DefaultHost)
	spec.StopTimeout = cmp.Or(spec.StopTimeout, defaultStopTimeout)
	return &Process{spec: spec, logger: l}
}

func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return p.startErr
	}

	binPath, err := exec.LookPath(p.spec.Bin)
	if err != nil {
		p.startErr = fmt.Errorf("%s binary %q not found: %w", p.spec.Name, p.spec.Bin, err)
		p.logger.LogError(p.spec.Name+" binary not found", p.startErr)
		return p.startErr
	}

	p.logger.Info("starting "+p.spec.Name,
		slog.String("cmd", fmt.Sprintf("%s %s", binPath, strings.Join(p.spec.Args, " "))),
	)

	addr := net.JoinHostPort(p.spec.Host, strconv.Itoa(p.spec.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.startErr = fmt.Errorf("port %d is already in use (leftover %s?): %w", p.spec.Port, p.spec.Name, err)
		p.logger.LogError(p.spec.Name+" port in use", p.startErr)
		return p.startErr
	}
	ln.Close()

	p.cmd = exec.CommandContext(context.Background(), binPath, p.spec.Args...)
	p.cmd.Stdout = os.Stdout
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		p.startErr = fmt.Errorf("stderr pipe: %w", err)
		p.logger.LogError("stderr pipe", p.startErr)
		return p.startErr
	}
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if p.isMuted(line) {
				continue
			}
			p.logger.Info(p.spec.Selector, slog.String("msg", line))
		}
	}()

	if err := p.cmd.Start(); err != nil {
		p.startErr = fmt.Errorf("start %s: %w", p.spec.Name, err)
		return p.startErr
	}

	done := make(chan struct{})
	p.done = done
	go func() {
		<-logDone
		err := p.cmd.Wait()
		p.logger.Info(p.spec.Name+" exited", slog.String("model", p.spec.Selector), slog.Any("status", err))
		close(done)
	}()

	if err := p.waitReady(ctx, done); err != nil {
		return p.abortStart(err, done)
	}

	p.logger.Info(p.spec.Name+" ready", slog.String("addr", addr))

	p.started = true
	return nil
}

func (p *Process) isMuted(line string) bool {
	return slices.ContainsFunc(p.spec.MutedLogLines, func(muted string) bool {
		return strings.Contains(line, muted)
	})
}

func (p *Process) abortStart(err error, done <-chan struct{}) error {
	p.startErr = err
	p.cmd.Process.Kill()
	<-done
	p.logger.LogError(p.spec.Name+" failed to start", err)
	return err
}

func (p *Process) waitReady(ctx context.Context, done <-chan struct{}) error {
	client := &http.Client{Timeout: healthCheckTimeout}
	var deadline <-chan time.Time
	if p.spec.StartTimeout != unlimitedStartAfter {
		deadline = time.After(p.spec.StartTimeout)
	}
	for {
		resp, err := p.healthCheck(ctx, client)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-done:
			return fmt.Errorf("server exited during startup")
		case <-deadline:
			return fmt.Errorf("server did not become ready within %v", p.spec.StartTimeout)
		case <-ctx.Done():
			return fmt.Errorf("server startup canceled: %w", ctx.Err())
		case <-time.After(healthPollInterval):
		}
	}
}

func (p *Process) healthCheck(ctx context.Context, client *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL()+p.spec.HealthPath, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func (p *Process) Stop(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.done == nil {
		return
	}
	select {
	case <-p.done:
	default:
		p.logger.Info("stopping "+p.spec.Name, slog.Int("pid", p.cmd.Process.Pid))
		p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
		case <-time.After(p.spec.StopTimeout):
			p.logger.Info(p.spec.Name+" did not stop in time, killing", slog.Int("pid", p.cmd.Process.Pid))
			p.cmd.Process.Kill()
			<-p.done
		}
	}
	p.started = false
}

func (p *Process) Exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.done == nil {
		return false
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *Process) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.done
}

func (p *Process) URL() string {
	return fmt.Sprintf("http://%s:%d", p.spec.Host, p.spec.Port)
}

func FlagArgs(flags map[string]any) []string {
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
