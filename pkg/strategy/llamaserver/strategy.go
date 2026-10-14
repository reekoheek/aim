package llamaserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/forwarder"
)

type Strategy struct {
	cfg       *config.Config
	logger    *logger.Logger
	monitor   *Monitor
	forwarder *forwarder.Forwarder
	selector  string
}

func NewStrategy(cfg *config.Config, l *logger.Logger, monitor *Monitor, selector string) *Strategy {
	return &Strategy{
		cfg:       cfg,
		logger:    l,
		monitor:   monitor,
		forwarder: forwarder.New(client, l),
		selector:  selector,
	}
}

func (s *Strategy) Handle(ctx context.Context, w http.ResponseWriter, req *strategy.Request) {
	fields, err := req.BodyFields()
	if err != nil {
		s.logger.LogError("invalid request body", err, slog.String("selector", s.selector))
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, err.Error())
		return
	}

	srv, err := s.monitor.Acquire(ctx, s.cfg, s.logger, s.selector)
	if err != nil {
		s.logger.LogError("failed to acquire llama server", err, slog.String("selector", s.selector))
		strategy.WriteError(w, acquireErrorStatus(err), strategy.ErrorTypeServer, err.Error())
		return
	}

	defer srv.ExitRequest()

	if srv.SlotCacheEnabled() {
		session := req.SessionID()
		release, err := srv.AcquireSlot(ctx, session)
		if err != nil {
			s.logger.LogError("failed to acquire slot", err, slog.String("session", session))
			strategy.WriteError(w, http.StatusServiceUnavailable, strategy.ErrorTypeServer, err.Error())
			return
		}
		defer release()

		fields["id_slot"], _ = json.Marshal(CacheSlot)
		fields["cache_prompt"], _ = json.Marshal(true)
		body, _ := json.Marshal(fields)
		req.Body = body
	}

	targetURL := srv.URL() + "/v1/chat/completions"
	s.forwarder.Forward(ctx, w, req, targetURL, nil)
}

func acquireErrorStatus(err error) int {
	if errors.Is(err, errStartFailed) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
