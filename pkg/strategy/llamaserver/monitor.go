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
	slots         *slotCache
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

func (m *Monitor) Acquire(ctx context.Context, cfg *config.Config, l *logger.Logger, model string) (*Server, *slotCache, error) {
	select {
	case m.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer m.unlock()

	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	if m.server != nil && m.server.Selector() == model && !m.server.Exited() && m.server.EnterRequest() {
		return m.server, m.slots, nil
	}

	if m.lifetime.Err() != nil {
		return nil, nil, errMonitorClosed
	}

	m.stopServer(m.lifetime)

	if m.lifetime.Err() != nil {
		return nil, nil, errMonitorClosed
	}

	entry, err := cfg.GetLlamaCppEntry(model)
	if err != nil {
		return nil, nil, err
	}

	if err := m.startServer(model, entry, cfg.DefaultServer(), l); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errStartFailed, err)
	}

	if !m.server.EnterRequest() {
		return nil, nil, errServerStopping
	}
	return m.server, m.slots, nil
}

func (m *Monitor) startServer(model string, entry *config.LlamaCppEntry, server config.ServerConfig, l *logger.Logger) error {
	var slotDir string
	if server.SaveSlotPath != "" && entry.Slots > 0 {
		dir, err := prepareSlotDir(server.SaveSlotPath)
		if err != nil {
			l.LogError("invalid slot save path", err)
			return err
		}
		slotDir = dir
		entry = withSlotSavePath(entry, dir)
	}

	srv := NewServer(model, entry, server, l, m.onIdle)
	if err := srv.Start(m.lifetime); err != nil {
		return err
	}

	m.server = srv
	if slotDir != "" {
		m.slots = newSlotCache(slotDir, model, srv.URL(), entry.Slots, server, l, srv.processDone())
	}
	return nil
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

func (m *Monitor) onIdle(srv *Server) {
	m.lock <- struct{}{}
	defer m.unlock()

	if m.server != srv {
		return
	}
	m.stopServer(context.Background())
}

func (m *Monitor) stopServer(ctx context.Context) {
	if m.server == nil {
		return
	}
	if m.slots != nil {
		m.slots.shutdown(ctx)
	}
	m.server.Stop()
	m.server = nil
	m.slots = nil
}
