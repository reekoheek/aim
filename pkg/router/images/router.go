package images

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/sdserver"
)

const (
	modelParam             = "model"
	multipartFormMediaType = "multipart/form-data"
	boundaryParam          = "boundary"
	maxModelFieldBytes     = 1 << 10
)

var (
	errNotMultipart     = errors.New("Content-Type must be multipart/form-data with a boundary")
	errInvalidMultipart = errors.New("invalid multipart form")
)

type modelReader func(body []byte, contentType string) (string, error)

type Router struct {
	cfg        *config.Config
	logger     *logger.Logger
	supervisor *lane.Supervisor
}

func New(cfg *config.Config, l *logger.Logger, supervisor *lane.Supervisor) *Router {
	return &Router{
		cfg:        cfg,
		logger:     l,
		supervisor: supervisor,
	}
}

func (r *Router) GenerationsHandler(w http.ResponseWriter, req *http.Request) {
	r.handle(w, req, strategy.ImageGenerationsPath, jsonModel)
}

func (r *Router) EditsHandler(w http.ResponseWriter, req *http.Request) {
	r.handle(w, req, strategy.ImageEditsPath, multipartModel)
}

func (r *Router) handle(w http.ResponseWriter, req *http.Request, path string, readModel modelReader) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.logger.LogError("failed to read request body", err)
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, "failed to read request body")
		return
	}

	contentType := req.Header.Get("Content-Type")
	model, err := readModel(body, contentType)
	if err != nil {
		r.logger.LogError("failed to parse request body", err)
		strategy.WriteError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, err.Error())
		return
	}

	if model == "" {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, "model is required")
		return
	}

	entry, err := r.cfg.ResolveModel(model)
	if err != nil {
		r.logger.LogError("failed to resolve model", err, slog.String("model", model))
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, err.Error())
		return
	}

	s, err := r.newStrategy(model, entry, path)
	if err != nil {
		strategy.WriteParamError(w, http.StatusBadRequest, strategy.ErrorTypeInvalidRequest, modelParam, err.Error())
		return
	}

	strategyReq := &strategy.Request{
		Body:        body,
		ContentType: contentType,
		Headers:     req.Header,
		Query:       req.URL.RawQuery,
		Errors:      strategy.OpenAIErrors{},
	}

	r.logger.LogRequest(req.Method, req.URL.Path, model, req.RemoteAddr)
	r.logger.Info("request metadata", slog.String("model_type", entry.Type))

	s.Handle(req.Context(), w, strategyReq)
}

func (r *Router) newStrategy(selector string, entry config.ModelEntry, path string) (strategy.Strategy, error) {
	switch entry.Type {
	case config.ModelTypeSDServer:
		return sdserver.NewStrategy(r.cfg, r.logger, r.supervisor, selector, path), nil
	case config.ModelTypeChatCompletion, config.ModelTypeLlamaServer:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.ChatCompletionsPath)
	case config.ModelTypeSystemOne:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.SystemOnePath)
	case config.ModelTypeAnthropic:
		return nil, fmt.Errorf("model %q is type %s, use %s", selector, entry.Type, strategy.MessagesPath)
	default:
		return nil, fmt.Errorf("unsupported type %q", entry.Type)
	}
}

func jsonModel(body []byte, contentType string) (string, error) {
	var generationReq GenerationRequest
	if err := json.Unmarshal(body, &generationReq); err != nil {
		return "", errors.New("invalid JSON")
	}
	return generationReq.Model, nil
}

func multipartModel(body []byte, contentType string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != multipartFormMediaType || params[boundaryParam] == "" {
		return "", errNotMultipart
	}
	reader := multipart.NewReader(bytes.NewReader(body), params[boundaryParam])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", errInvalidMultipart
		}
		if part.FormName() != modelParam {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, maxModelFieldBytes))
		if err != nil {
			return "", errInvalidMultipart
		}
		return string(value), nil
	}
}

type GenerationRequest struct {
	Model string `json:"model"`
}
