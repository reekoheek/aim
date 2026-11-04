package sdserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/forwarder"
)

type Strategy struct {
	cfg        *config.Config
	logger     *logger.Logger
	supervisor *lane.Supervisor
	forwarder  *forwarder.Forwarder
	selector   string
	path       string
}

func NewStrategy(cfg *config.Config, l *logger.Logger, supervisor *lane.Supervisor, selector string, path string) *Strategy {
	return &Strategy{
		cfg:        cfg,
		logger:     l,
		supervisor: supervisor,
		forwarder:  forwarder.New(client, l),
		selector:   selector,
		path:       path,
	}
}

func (s *Strategy) Handle(ctx context.Context, w http.ResponseWriter, req *strategy.Request) {
	lease, err := runServer(ctx, s.supervisor, s.cfg, s.logger, s.selector)
	if err != nil {
		s.logger.LogError("failed to run sd server", err, slog.String("selector", s.selector))
		req.WriteError(w, runErrorStatus(err), strategy.ErrorTypeServer, err.Error())
		return
	}

	defer lease.Release()

	targetURL := lease.Process.URL() + s.path
	s.forwarder.Forward(ctx, w, req, targetURL, nil)
}

func runErrorStatus(err error) int {
	if errors.Is(err, lane.ErrStartFailed) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
