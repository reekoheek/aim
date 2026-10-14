package llamaserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

var (
	errMonitorClosed  = errors.New("llama.cpp monitor is shutting down")
	errServerStopping = errors.New("llama.cpp server is stopping")
	errStartFailed    = errors.New("failed to start local LLM")
)

type Monitor struct {
	lock          chan struct{}
	server        *Server
	lifetime      context.Context
	closeLifetime context.CancelFunc
}

func NewMonitor() *Monitor {
	lifetime, closeLifetime := context.WithCancel(context.Background())
	return &Monitor{
		lock:          make(chan struct{}, 1),
		lifetime:      lifetime,
		closeLifetime: closeLifetime,
	}
}

func (m *Monitor) Acquire(ctx context.Context, cfg *config.Config, l *logger.Logger, model string) (*Server, error) {
	select {
	case m.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer m.unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if m.server != nil && m.server.Selector() == model && !m.server.Exited() && m.server.EnterRequest() {
		return m.server, nil
	}

	if m.lifetime.Err() != nil {
		return nil, errMonitorClosed
	}

	m.stopServer(m.lifetime)

	if m.lifetime.Err() != nil {
		return nil, errMonitorClosed
	}

	entry, err := cfg.GetLlamaCppEntry(model)
	if err != nil {
		return nil, err
	}

	srv := NewServer(model, entry, cfg.DefaultServer(), l)
	if err := srv.Start(m.lifetime); err != nil {
		return nil, fmt.Errorf("%w: %w", errStartFailed, err)
	}

	m.server = srv
	if !srv.EnterRequest() {
		return nil, errServerStopping
	}
	return srv, nil
}

func (m *Monitor) Close(ctx context.Context) {
	m.closeLifetime()

	m.lock <- struct{}{}
	defer m.unlock()

	m.stopServer(ctx)
}

func (m *Monitor) unlock() {
	<-m.lock
}

func (m *Monitor) stopServer(ctx context.Context) {
	if m.server == nil {
		return
	}
	m.server.Stop(ctx)
	m.server = nil
}
