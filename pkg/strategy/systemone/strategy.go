package systemone

import (
	"context"
	"encoding/json"
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
	forwarder *forwarder.Forwarder
	provider  string
	model     string
}

func NewStrategy(cfg *config.Config, l *logger.Logger, provider string, model string) *Strategy {
	return &Strategy{
		cfg:       cfg,
		logger:    l,
		forwarder: forwarder.New(client, l),
		provider:  provider,
		model:     model,
	}
}

func (s *Strategy) Handle(ctx context.Context, w http.ResponseWriter, req *strategy.Request) {
	provider, err := s.cfg.GetProvider(s.provider)
	if err != nil {
		s.logger.LogError("failed to get provider", err, slog.String("provider", s.provider))
		req.WriteError(w, http.StatusInternalServerError, strategy.ErrorTypeServer, err.Error())
		return
	}

	fields, err := req.BodyFields()
	if err != nil {
		s.logger.LogError("invalid request body", err, slog.String("provider", s.provider))
		req.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, err.Error())
		return
	}
	fields["model"], _ = json.Marshal(s.model)
	newBody, err := json.Marshal(fields)
	if err != nil {
		s.logger.LogError("failed to encode upstream request body", err)
		req.WriteError(w, http.StatusInternalServerError, strategy.ErrorTypeServer, "failed to encode upstream request body")
		return
	}
	req.Body = newBody

	targetURL := provider.BaseURL + strategy.SystemOnePath
	extraHeaders := map[string]string{
		"Authorization": "Bearer " + provider.APIKey,
	}

	s.forwarder.Forward(ctx, w, req, targetURL, extraHeaders)
}
