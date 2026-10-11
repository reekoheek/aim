package proxy

import (
	"bytes"
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

// Proxy is the LLM proxy handler.
type Proxy struct {
	cfg    *config.Config
	logger *logger.Logger
	client *http.Client
	llama  *llamacpp.Server
	mu     sync.Mutex
}

// Llama returns the llama.cpp server, or nil if not using one.
func (p *Proxy) Llama() *llamacpp.Server {
	return p.llama
}

// formatJSON pretty-prints JSON for logging.
func (p *Proxy) formatJSON(data []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return string(data)
	}
	return out.String()
}

// New creates a new Proxy.
func New(cfg *config.Config, l *logger.Logger) *Proxy {
	return &Proxy{
		cfg:    cfg,
		logger: l,
		// No overall timeout: long streaming generations must not be cut off.
		// The request is canceled when the downstream client disconnects.
		client: &http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
		llama: nil,
	}
}

// Handler returns the HTTP handler for the proxy.
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			p.handleCompletion(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// CompletionRequest represents the OpenAI chat completion request body.
// Only parses model and stream — messages are forwarded as-is.
type CompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (p *Proxy) handleCompletion(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Parse request body
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

	// Validate model
	if req.Model == "" {
		http.Error(w, `{"error":{"message":"model is required","type":"invalid_request_error","param":"model","code":null}}`, http.StatusBadRequest)
		return
	}

	// Resolve model
	modelType, info, err := p.cfg.ResolveModel(req.Model)
	if err != nil {
		p.logger.LogError("failed to resolve model", err, slog.String("model", req.Model))
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error","param":"model","code":null}}`, err.Error()), http.StatusBadRequest)
		return
	}

	var targetURL string

	switch modelType {
	case "provider":
		provider, err := p.cfg.GetProvider(info["provider"].(string))
		if err != nil {
			p.logger.LogError("failed to get provider", err, slog.String("provider", info["provider"].(string)))
			http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
			return
		}

		// Rewrite model name
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

	case "llama.cpp":
		p.mu.Lock()
		if p.llama == nil {
			entry, err := p.cfg.GetLlamaCppEntry(req.Model)
			if err != nil {
				p.logger.LogError("failed to get llama.cpp config", err)
				p.mu.Unlock()
				http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
				return
			}
			p.llama = llamacpp.NewServer(req.Model, entry, p.logger)
			if err := p.llama.Start(r.Context()); err != nil {
				p.logger.LogError("failed to start llama.cpp server", err)
				p.mu.Unlock()
				http.Error(w, fmt.Sprintf(`{"error":{"message":"failed to start local LLM: %s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusServiceUnavailable)
				return
			}
		}
		targetURL = p.llama.URL() + "/v1/chat/completions"
		p.mu.Unlock()
	}

	p.logger.LogRequest(r.Method, r.URL.Path, req.Model, r.RemoteAddr)
	p.logger.Info("request metadata",
		slog.String("target_url", targetURL),
		slog.String("session", r.Header.Get("X-Session-Affinity")),
		slog.Bool("stream", req.Stream),
	)

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		p.logger.LogError("failed to create upstream request", err)
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}

	// Copy headers (except hop-by-hop)
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
		// Let the transport negotiate compression; the response is forwarded
		// without Content-Encoding, so it must arrive decompressed.
		"accept-encoding": true,
	}

	for k, v := range r.Header {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, val := range v {
			httpReq.Header.Add(k, val)
		}
	}

	// Set required headers
	httpReq.Header.Set("Content-Type", "application/json")
	if modelType == "provider" {
		if prov, ok := info["provider"].(string); ok {
			if p, err := p.cfg.GetProvider(prov); err == nil {
				httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
			}
		}
	}

	// Execute request
	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		p.logger.LogError("upstream request failed", err,
			slog.String("url", targetURL),
		)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"upstream error: %s","type":"upstream_error","param":null,"code":null}}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer httpResp.Body.Close()

	// Stream only successful responses; upstream errors are plain JSON
	// and are forwarded below with their original status code.
	if req.Stream && httpResp.StatusCode/100 == 2 {
		p.streamResponse(w, httpResp, start)
		return
	}

	// Non-streaming: read full response
	body, err = io.ReadAll(httpResp.Body)
	if err != nil {
		p.logger.LogError("failed to read upstream response", err)
		http.Error(w, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	// Log response
	p.logger.LogResponse(httpResp.StatusCode, time.Since(start))
	p.logger.Info("response body",
		slog.Int("status", httpResp.StatusCode),
		slog.Int("body_len", len(body)),
		slog.String("body", string(body)),
	)

	// Forward response
	w.Header().Set("Content-Type", httpResp.Header.Get("Content-Type"))
	w.Header().Set("X-Request-Id", httpResp.Header.Get("X-Request-Id"))
	w.WriteHeader(httpResp.StatusCode)
	w.Write(body)
}

func (p *Proxy) streamResponse(w http.ResponseWriter, httpResp *http.Response, start time.Time) {
	// Set streaming headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(httpResp.StatusCode)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	// Pass the upstream SSE stream through unchanged (including its own
	// "data: [DONE]"), flushing after every read.
	buf := make([]byte, 32*1024)
	for {
		n, err := httpResp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				p.logger.LogError("failed to write stream chunk", writeErr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			p.logger.LogResponse(httpResp.StatusCode, time.Since(start))
			return
		}
		if err != nil {
			p.logger.LogError("stream read error", err)
			return
		}
	}
}
