package proxy

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

type Proxy struct {
	cfg    *config.Config
	logger *logger.Logger
	client *http.Client
	llama  *llamacpp.Server
	mu     sync.Mutex
}

func (p *Proxy) Llama() *llamacpp.Server {
	return p.llama
}

func (p *Proxy) Close(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.llama != nil {
		p.llama.Stop(ctx)
		p.llama = nil
	}
}

func New(cfg *config.Config, l *logger.Logger) *Proxy {
	return &Proxy{
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

func (p *Proxy) Handler(w http.ResponseWriter, r *http.Request) {
	p.handleCompletion(w, r)
}

type CompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (p *Proxy) handleCompletion(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.LogError("failed to read request body", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var req CompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		p.logger.LogError("failed to parse request body", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.Model == "" {
		http.Error(w, `{"error":{"message":"model is required","type":"invalid_request_error","param":"model","code":null}}`, http.StatusBadRequest)
		return
	}

	modelType, info, err := p.cfg.ResolveModel(req.Model)
	if err != nil {
		p.logger.LogError("failed to resolve model", err, slog.String("model", req.Model))
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error","param":"model","code":null}}`, err.Error()), http.StatusBadRequest)
		return
	}

	var targetURL string
	var llama *llamacpp.Server

	switch modelType {
	case "provider":
		provider, err := p.cfg.GetProvider(info["provider"].(string))
		if err != nil {
			p.logger.LogError("failed to get provider", err, slog.String("provider", info["provider"].(string)))
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
			return
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			p.logger.LogError("failed to parse request body", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		fields["model"], _ = json.Marshal(info["model"].(string))
		body, err = json.Marshal(fields)
		if err != nil {
			p.logger.LogError("failed to encode upstream request body", err)
			http.Error(w, "failed to encode upstream request body", http.StatusInternalServerError)
			return
		}

		targetURL = provider.BaseURL + "/v1/chat/completions"

	case "llama-server":
		p.mu.Lock()
		if r.Context().Err() != nil {
			p.mu.Unlock()
			return
		}
		if p.llama != nil && p.llama.Exited() {
			p.logger.Info("llama server has exited", slog.String("model", p.llama.Selector()))
			p.llama = nil
		}
		if p.llama != nil && p.llama.Selector() != req.Model {
			p.logger.Info("replacing llama server", slog.String("old", p.llama.Selector()), slog.String("new", req.Model))
			p.llama.Stop(context.Background())
			p.llama = nil
		}
		if p.llama == nil {
			entry, err := p.cfg.GetLlamaCppEntry(req.Model)
			if err != nil {
				p.logger.LogError("failed to get llama server config", err)
				p.mu.Unlock()
				http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
				return
			}
			srv := llamacpp.NewServer(req.Model, entry, p.cfg.DefaultServer(), p.logger)
			if err := srv.Start(r.Context()); err != nil {
				p.logger.LogError("failed to start llama server", err)
				p.mu.Unlock()
				http.Error(w, fmt.Sprintf(`{"error":{"message":"failed to start local LLM: %s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusServiceUnavailable)
				return
			}
			p.llama = srv
		}
		llama = p.llama
		p.mu.Unlock()
		targetURL = llama.URL() + "/v1/chat/completions"
	}

	if llama != nil {
		llama.EnterRequest()
		defer llama.ExitRequest()
	}

	session := sessionID(r)

	p.logger.LogRequest(r.Method, r.URL.Path, req.Model, r.RemoteAddr)
	p.logger.Info("request metadata",
		slog.String("target_url", targetURL),
		slog.String("session", session),
		slog.Bool("stream", req.Stream),
	)

	keepSlot := false
	if llama != nil && llama.SlotCacheEnabled() {
		release, err := llama.AcquireSlot(r.Context(), session)
		if err != nil {
			p.logger.LogError("failed to acquire slot", err, slog.String("session", session))
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

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		p.logger.LogError("failed to create upstream request", err)
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

	for k, v := range r.Header {
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
			if p, err := p.cfg.GetProvider(prov); err == nil {
				httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
			}
		}
	}

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		p.logger.LogError("upstream request failed", err,
			slog.String("url", targetURL),
		)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"upstream error: %s","type":"upstream_error","param":null,"code":null}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer httpResp.Body.Close()

	if req.Stream && httpResp.StatusCode/100 == 2 {
		keepSlot = p.streamResponse(w, httpResp, start)
		return
	}

	body, err = io.ReadAll(httpResp.Body)
	if err != nil {
		p.logger.LogError("failed to read upstream response", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	p.logger.LogResponse(httpResp.StatusCode, time.Since(start))
	p.logger.Info("response body",
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

func sessionID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("X-Session-Affinity")); id != "" {
		return id
	}
	return noSession
}

func (p *Proxy) streamResponse(w http.ResponseWriter, httpResp *http.Response, start time.Time) bool {
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
				p.logger.LogError("failed to write stream chunk", writeErr)
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			p.logger.LogResponse(httpResp.StatusCode, time.Since(start))
			return true
		}
		if err != nil {
			p.logger.LogError("stream read error", err)
			return false
		}
	}
}
