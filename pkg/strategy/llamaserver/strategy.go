package llamaserver

import (
	"context"
	"encoding/json"
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

func NewStrategy(cfg *config.Config, l *logger.Logger, supervisor *lane.Supervisor, selector string) *Strategy {
	return newStrategy(cfg, l, supervisor, selector, strategy.ChatCompletionsPath)
}

func NewMessagesStrategy(cfg *config.Config, l *logger.Logger, supervisor *lane.Supervisor, selector string) *Strategy {
	return newStrategy(cfg, l, supervisor, selector, strategy.MessagesPath)
}

func newStrategy(cfg *config.Config, l *logger.Logger, supervisor *lane.Supervisor, selector string, path string) *Strategy {
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
	fields, err := req.BodyFields()
	if err != nil {
		s.logger.LogError("invalid request body", err, slog.String("selector", s.selector))
		req.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, err.Error())
		return
	}

	lease, err := runServer(ctx, s.supervisor, s.cfg, s.logger, s.selector)
	if err != nil {
		s.logger.LogError("failed to run llama server", err, slog.String("selector", s.selector))
		req.WriteError(w, runErrorStatus(err), strategy.ErrorTypeServer, err.Error())
		return
	}

	defer lease.Release()
	srv := lease.Process

	if slots := srv.cachedSlots(); slots != nil {
		session := req.SessionID()
		slotID, release, err := slots.acquire(ctx, session)
		if err != nil {
			s.logger.LogError("failed to acquire slot", err, slog.String("session", session))
			req.WriteError(w, http.StatusServiceUnavailable, strategy.ErrorTypeServer, err.Error())
			return
		}
		defer release()

		pinSlot(fields, slotID)
		body, _ := json.Marshal(fields)
		req.Body = body
	}

	targetURL := srv.URL() + s.path
	s.forwarder.Forward(ctx, w, req, targetURL, nil)
}

func runErrorStatus(err error) int {
	if errors.Is(err, lane.ErrStartFailed) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
