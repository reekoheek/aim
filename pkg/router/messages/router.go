package messages

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/anthropic"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
)

type strategyFactory func(selector string, entry config.ModelEntry) (strategy.Strategy, error)

type Router struct {
	cfg          *config.Config
	logger       *logger.Logger
	llamaServers *llamaserver.Monitor
	errors       strategy.AnthropicErrors
}

func New(cfg *config.Config, l *logger.Logger, llamaServers *llamaserver.Monitor) *Router {
	return &Router{
		cfg:          cfg,
		logger:       l,
		llamaServers: llamaServers,
	}
}

func (r *Router) Handler(w http.ResponseWriter, req *http.Request) {
	r.handle(w, req, r.newMessagesStrategy)
}

func (r *Router) CountTokensHandler(w http.ResponseWriter, req *http.Request) {
	r.handle(w, req, r.newCountTokensStrategy)
}

func (r *Router) handle(w http.ResponseWriter, req *http.Request, newStrategy strategyFactory) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.logger.LogError("failed to read request body", err)
		r.errors.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "failed to read request body")
		return
	}

	var messagesReq MessagesRequest
	if err := json.Unmarshal(body, &messagesReq); err != nil {
		r.logger.LogError("failed to parse request body", err)
		r.errors.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "invalid JSON")
		return
	}

	if messagesReq.Model == "" {
		r.errors.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "model is required")
		return
	}

	entry, err := r.cfg.ResolveModel(messagesReq.Model)
	if err != nil {
		r.logger.LogError("failed to resolve model", err, slog.String("model", messagesReq.Model))
		r.errors.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, err.Error())
		return
	}

	s, err := newStrategy(messagesReq.Model, entry)
	if err != nil {
		r.errors.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, err.Error())
		return
	}

	strategyReq := &strategy.Request{
		Stream:  messagesReq.Stream,
		Body:    body,
		Headers: req.Header,
		Query:   req.URL.RawQuery,
		Errors:  r.errors,
	}

	r.logger.LogRequest(req.Method, req.URL.Path, messagesReq.Model, req.RemoteAddr)
	r.logger.Info("request metadata",
		slog.String("model_type", entry.Type),
		slog.String("session", strategyReq.SessionID()),
		slog.Bool("stream", messagesReq.Stream),
	)

	s.Handle(req.Context(), w, strategyReq)
}

func (r *Router) newMessagesStrategy(selector string, entry config.ModelEntry) (strategy.Strategy, error) {
	switch entry.Type {
	case config.ModelTypeAnthropic:
		return anthropic.NewStrategy(r.cfg, r.logger, entry.Provider, entry.Model, strategy.MessagesPath), nil
	case config.ModelTypeLlamaServer:
		return llamaserver.NewMessagesStrategy(r.cfg, r.logger, r.llamaServers, selector), nil
	default:
		return nil, otherEndpointError(selector, entry)
	}
}

func (r *Router) newCountTokensStrategy(selector string, entry config.ModelEntry) (strategy.Strategy, error) {
	switch entry.Type {
	case config.ModelTypeAnthropic:
		return anthropic.NewStrategy(r.cfg, r.logger, entry.Provider, entry.Model, strategy.CountTokensPath), nil
	case config.ModelTypeLlamaServer:
		return llamaserver.NewCountTokensStrategy(r.cfg, r.logger, r.llamaServers, selector), nil
	default:
		return nil, otherEndpointError(selector, entry)
	}
}

func otherEndpointError(selector string, entry config.ModelEntry) error {
	switch entry.Type {
	case config.ModelTypeChatCompletion:
		return fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.ChatCompletionsPath)
	case config.ModelTypeSystemOne:
		return fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.SystemOnePath)
	default:
		return fmt.Errorf("unsupported type %q", entry.Type)
	}
}

type MessagesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}
