package chatcompletion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/llamacpp"
)

type Router struct {
	cfg    *config.Config
	logger *logger.Logger
	client *http.Client
	llama  *llamacpp.Server
	mu     sync.Mutex
}

func (r *Router) Llama() *llamacpp.Server {
	return r.llama
}

func (r *Router) Close(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.llama != nil {
		r.llama.Stop(ctx)
		r.llama = nil
	}
}

func New(cfg *config.Config, l *logger.Logger) *Router {
	return &Router{
		cfg:    cfg,
		logger: l,
		client: &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
		llama: nil,
	}
}

func (r *Router) Handler(w http.ResponseWriter, req *http.Request) {
	r.handleCompletion(w, req)
}

type CompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (r *Router) handleCompletion(w http.ResponseWriter, req *http.Request) {
	start := time.Now()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.logger.LogError("failed to read request body", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var completionReq CompletionRequest
	if err := json.Unmarshal(body, &completionReq); err != nil {
		r.logger.LogError("failed to parse request body", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if completionReq.Model == "" {
		http.Error(w, `{"error":{"message":"model is required","type":"invalid_request_error","param":"model","code":null}}`, http.StatusBadRequest)
		return
	}

	modelType, info, err := r.cfg.ResolveModel(completionReq.Model)
	if err != nil {
		r.logger.LogError("failed to resolve model", err, slog.String("model", completionReq.Model))
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error","param":"model","code":null}}`, err.Error()), http.StatusBadRequest)
		return
	}

	var targetURL string
	var llama *llamacpp.Server

	switch modelType {
	case "provider":
		provider, err := r.cfg.GetProvider(info["provider"].(string))
		if err != nil {
			r.logger.LogError("failed to get provider", err, slog.String("provider", info["provider"].(string)))
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
			return
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			r.logger.LogError("failed to parse request body", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		fields["model"], _ = json.Marshal(info["model"].(string))
		body, err = json.Marshal(fields)
		if err != nil {
			r.logger.LogError("failed to encode upstream request body", err)
			http.Error(w, "failed to encode upstream request body", http.StatusInternalServerError)
			return
		}

		targetURL = provider.BaseURL + "/v1/chat/completions"

	case "llama-server":
		r.mu.Lock()
		if req.Context().Err() != nil {
			r.mu.Unlock()
			return
		}
		if r.llama != nil && r.llama.Exited() {
			r.logger.Info("llama server has exited", slog.String("model", r.llama.Selector()))
			r.llama = nil
		}
		if r.llama != nil && r.llama.Selector() != completionReq.Model {
			r.logger.Info("replacing llama server", slog.String("old", r.llama.Selector()), slog.String("new", completionReq.Model))
			r.llama.Stop(context.Background())
			r.llama = nil
		}
		if r.llama == nil {
			entry, err := r.cfg.GetLlamaCppEntry(completionReq.Model)
			if err != nil {
				r.logger.LogError("failed to get llama server config", err)
				r.mu.Unlock()
				http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
				return
			}
			srv := llamacpp.NewServer(completionReq.Model, entry, r.cfg.DefaultServer(), r.logger)
			if err := srv.Start(req.Context()); err != nil {
				r.logger.LogError("failed to start llama server", err)
				r.mu.Unlock()
				http.Error(w, fmt.Sprintf(`{"error":{"message":"failed to start local LLM: %s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusServiceUnavailable)
				return
			}
			r.llama = srv
		}
		llama = r.llama
		r.mu.Unlock()
		targetURL = llama.URL() + "/v1/chat/completions"
	}

	if llama != nil {
		llama.EnterRequest()
		defer llama.ExitRequest()
	}

	session := sessionID(req)

	r.logger.LogRequest(req.Method, req.URL.Path, completionReq.Model, req.RemoteAddr)
	r.logger.Info("request metadata",
		slog.String("target_url", targetURL),
		slog.String("session", session),
		slog.Bool("stream", completionReq.Stream),
	)

	keepSlot := false
	if llama != nil && llama.SlotCacheEnabled() {
		release, err := llama.AcquireSlot(req.Context(), session)
		if err != nil {
			r.logger.LogError("failed to acquire slot", err, slog.String("session", session))
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusServiceUnavailable)
			return
		}
		defer func() { release(keepSlot) }()

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err == nil {
			fields["id_slot"], _ = json.Marshal(llamacpp.CacheSlot)
			fields["cache_prompt"], _ = json.Marshal(true)
			body, _ = json.Marshal(fields)
		}
	}

	httpReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		r.logger.LogError("failed to create upstream request", err)
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	hopByHop := map[string]bool{
		"connection":          true,
		"transfer-encoding":   true,
		"host":                true,
		"keep-alive":          true,
		"proxy-authenticate":  true,
		"proxy-authorization": true,
		"te":                  true,
		"trailer":             true,
		"upgrade":             true,
		"accept-encoding":     true,
	}

	for k, v := range req.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, val := range v {
			httpReq.Header.Add(k, val)
		}
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if modelType == "provider" {
		if prov, ok := info["provider"].(string); ok {
			if p, err := r.cfg.GetProvider(prov); err == nil {
				httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
			}
		}
	}

	httpResp, err := r.client.Do(httpReq)
	if err != nil {
		r.logger.LogError("upstream request failed", err,
			slog.String("url", targetURL),
		)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"upstream error: %s","type":"upstream_error","param":null,"code":null}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer httpResp.Body.Close()

	if completionReq.Stream && httpResp.StatusCode/100 == 2 {
		keepSlot = r.streamResponse(w, httpResp, start)
		return
	}

	body, err = io.ReadAll(httpResp.Body)
	if err != nil {
		r.logger.LogError("failed to read upstream response", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	r.logger.LogResponse(httpResp.StatusCode, time.Since(start))
	r.logger.Info("response body",
		slog.Int("status", httpResp.StatusCode),
		slog.Int("body_len", len(body)),
		slog.String("body", string(body)),
	)

	w.Header().Set("Content-Type", httpResp.Header.Get("Content-Type"))
	w.Header().Set("X-Request-Id", httpResp.Header.Get("X-Request-Id"))
	w.WriteHeader(httpResp.StatusCode)
	w.Write(body)
	keepSlot = httpResp.StatusCode/100 == 2
}

const noSession = "-"

func sessionID(req *http.Request) string {
	if id := strings.TrimSpace(req.Header.Get("X-Session-Affinity")); id != "" {
		return id
	}
	return noSession
}

func (r *Router) streamResponse(w http.ResponseWriter, httpResp *http.Response, start time.Time) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(httpResp.StatusCode)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, err := httpResp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				r.logger.LogError("failed to write stream chunk", writeErr)
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			r.logger.LogResponse(httpResp.StatusCode, time.Since(start))
			return true
		}
		if err != nil {
			r.logger.LogError("stream read error", err)
			return false
		}
	}
}
