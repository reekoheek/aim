package llamaserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/forwarder"
)

type SlotlessStrategy struct {
	cfg       *config.Config
	logger    *logger.Logger
	monitor   *Monitor
	forwarder *forwarder.Forwarder
	selector  string
	path      string
}

func NewSystemOneStrategy(cfg *config.Config, l *logger.Logger, monitor *Monitor, selector string) *SlotlessStrategy {
	return newSlotlessStrategy(cfg, l, monitor, selector, strategy.SystemOnePath)
}

func NewCountTokensStrategy(cfg *config.Config, l *logger.Logger, monitor *Monitor, selector string) *SlotlessStrategy {
	return newSlotlessStrategy(cfg, l, monitor, selector, strategy.CountTokensPath)
}

func newSlotlessStrategy(cfg *config.Config, l *logger.Logger, monitor *Monitor, selector string, path string) *SlotlessStrategy {
	return &SlotlessStrategy{
		cfg:       cfg,
		logger:    l,
		monitor:   monitor,
		forwarder: forwarder.New(client, l),
		selector:  selector,
		path:      path,
	}
}

func (s *SlotlessStrategy) Handle(ctx context.Context, w http.ResponseWriter, req *strategy.Request) {
	srv, _, err := s.monitor.Acquire(ctx, s.cfg, s.logger, s.selector)
	if err != nil {
		s.logger.LogError("failed to acquire llama server", err, slog.String("selector", s.selector))
		req.WriteError(w, acquireErrorStatus(err), strategy.ErrorTypeServer, err.Error())
		return
	}

	defer srv.ExitRequest()

	targetURL := srv.URL() + s.path
	s.forwarder.Forward(ctx, w, req, targetURL, nil)
}
