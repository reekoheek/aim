package chatcompletion

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/chatcompletion"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
)

const modelParam = "model"

type Router struct {
	cfg          *config.Config
	logger       *logger.Logger
	llamaServers *llamaserver.Monitor
}

func New(cfg *config.Config, l *logger.Logger, llamaServers *llamaserver.Monitor) *Router {
	return &Router{
		cfg:          cfg,
		logger:       l,
		llamaServers: llamaServers,
	}
}

func (r *Router) Handler(w http.ResponseWriter, req *http.Request) {
	r.handleCompletion(w, req)
}

func (r *Router) handleCompletion(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.logger.LogError("failed to read request body", err)
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "failed to read request body")
		return
	}

	var completionReq CompletionRequest
	if err := json.Unmarshal(body, &completionReq); err != nil {
		r.logger.LogError("failed to parse request body", err)
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "invalid JSON")
		return
	}

	if completionReq.Model == "" {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, "model is required")
		return
	}

	entry, err := r.cfg.ResolveModel(completionReq.Model)
	if err != nil {
		r.logger.LogError("failed to resolve model", err, slog.String("model", completionReq.Model))
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, err.Error())
		return
	}

	s, err := r.newStrategy(completionReq.Model, entry)
	if err != nil {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, err.Error())
		return
	}

	strategyReq := &strategy.Request{
		Stream:  completionReq.Stream,
		Body:    body,
		Headers: req.Header,
		Query:   req.URL.RawQuery,
		Errors:  strategy.OpenAIErrors{},
	}

	r.logger.LogRequest(req.Method, req.URL.Path, completionReq.Model, req.RemoteAddr)
	r.logger.Info("request metadata",
		slog.String("model_type", entry.Type),
		slog.String("session", strategyReq.SessionID()),
		slog.Bool("stream", completionReq.Stream),
	)

	s.Handle(req.Context(), w, strategyReq)
}

func (r *Router) newStrategy(selector string, entry config.ModelEntry) (strategy.Strategy, error) {
	switch entry.Type {
	case config.ModelTypeChatCompletion:
		return chatcompletion.NewStrategy(r.cfg, r.logger, entry.Provider, entry.Model), nil
	case config.ModelTypeLlamaServer:
		return llamaserver.NewStrategy(r.cfg, r.logger, r.llamaServers, selector), nil
	case config.ModelTypeSystemOne:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.SystemOnePath)
	case config.ModelTypeAnthropic:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.MessagesPath)
	default:
		return nil, fmt.Errorf("unsupported type %q", entry.Type)
	}
}

type CompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}
