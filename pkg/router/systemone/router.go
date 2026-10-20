package systemone

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
	"github.com/reekoheek/aim/pkg/strategy/systemone"
)

const (
	modelParam     = "model"
	questionsParam = "questions"
)

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
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.logger.LogError("failed to read request body", err)
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "failed to read request body")
		return
	}

	var systemOneReq SystemOneRequest
	if err := json.Unmarshal(body, &systemOneReq); err != nil {
		r.logger.LogError("failed to parse request body", err)
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "invalid JSON")
		return
	}

	if systemOneReq.Model == "" {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, "model is required")
		return
	}

	if !systemOneReq.hasQuestions() {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, questionsParam, "questions must be a non-empty object")
		return
	}

	entry, err := r.cfg.ResolveModel(systemOneReq.Model)
	if err != nil {
		r.logger.LogError("failed to resolve model", err, slog.String("model", systemOneReq.Model))
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, err.Error())
		return
	}

	s, err := r.newStrategy(systemOneReq.Model, entry)
	if err != nil {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, err.Error())
		return
	}

	strategyReq := &strategy.Request{
		Body:    body,
		Headers: req.Header,
		Query:   req.URL.RawQuery,
		Errors:  strategy.OpenAIErrors{},
	}

	r.logger.LogRequest(req.Method, req.URL.Path, systemOneReq.Model, req.RemoteAddr)
	r.logger.Info("request metadata", slog.String("model_type", entry.Type))

	s.Handle(req.Context(), w, strategyReq)
}

func (r *Router) newStrategy(selector string, entry config.ModelEntry) (strategy.Strategy, error) {
	switch entry.Type {
	case config.ModelTypeSystemOne:
		return systemone.NewStrategy(r.cfg, r.logger, entry.Provider, entry.Model), nil
	case config.ModelTypeLlamaServer:
		return llamaserver.NewSystemOneStrategy(r.cfg, r.logger, r.llamaServers, selector), nil
	case config.ModelTypeChatCompletion:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.ChatCompletionsPath)
	case config.ModelTypeAnthropic:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.MessagesPath)
	default:
		return nil, fmt.Errorf("unsupported type %q", entry.Type)
	}
}

type SystemOneRequest struct {
	Model     string          `json:"model"`
	Questions json.RawMessage `json:"questions"`
}

func (r SystemOneRequest) hasQuestions() bool {
	var questions map[string]json.RawMessage
	return json.Unmarshal(r.Questions, &questions) == nil && len(questions) > 0
}
