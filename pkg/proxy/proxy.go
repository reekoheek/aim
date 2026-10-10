package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
)

// Proxy is the LLM proxy handler.
type Proxy struct {
	cfg    *config.Config
	logger *logger.Logger
	client *http.Client
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
		client: &http.Client{},
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
	providerID, modelName, err := p.cfg.ResolveModel(req.Model)
	if err != nil {
		p.logger.LogError("failed to resolve model", err, slog.String("model", req.Model))
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"invalid_request_error","param":"model","code":null}}`, err.Error()), http.StatusBadRequest)
		return
	}

	// Get provider
	provider, err := p.cfg.GetProvider(providerID)
	if err != nil {
		p.logger.LogError("failed to get provider", err, slog.String("provider", providerID))
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error","param":null,"code":null}}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Rewrite model to the upstream model name, keeping other fields as-is
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		p.logger.LogError("failed to parse request body", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	fields["model"], _ = json.Marshal(modelName)
	body, err = json.Marshal(fields)
	if err != nil {
		p.logger.LogError("failed to encode upstream request body", err)
		http.Error(w, "failed to encode upstream request body", http.StatusInternalServerError)
		return
	}

	// Count messages from raw body (avoid parsing messages)
	msgCount := 0
	if idx := bytes.Index(body, []byte(`"messages"`)); idx >= 0 {
		arrStart := bytes.Index(body[idx:], []byte("["))
		if arrStart >= 0 {
			arrBody := body[idx+arrStart:]
			for i := 0; i < len(arrBody); i++ {
				if arrBody[i] == '{' {
					msgCount++
				}
			}
		}
	}

	// Log request
	p.logger.LogRequest(r.Method, r.URL.Path, req.Model, r.RemoteAddr)
	p.logger.LogHeaders(r.Header)
	p.logger.Info("request metadata",
		slog.String("model", modelName),
		slog.String("base_url", provider.BaseURL),
		slog.Int("messages", msgCount),
		slog.Bool("stream", req.Stream),
	)
	// p.logger.Info("request body",
	// 	slog.String("body", string(p.formatJSON(body))),
	// )

	// Build target URL
	targetURL := provider.BaseURL
	if !strings.HasSuffix(targetURL, "/") {
		targetURL += "/"
	}
	targetURL += "v1/chat/completions"

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
	httpReq.Header.Set("Authorization", "Bearer "+provider.APIKey)

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
