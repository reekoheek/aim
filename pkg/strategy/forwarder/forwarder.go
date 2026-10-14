package forwarder

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/strategy"
)

var hopByHop = map[string]bool{
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

type Forwarder struct {
	client *http.Client
	logger *logger.Logger
}

func New(client *http.Client, l *logger.Logger) *Forwarder {
	return &Forwarder{client: client, logger: l}
}

func (f *Forwarder) Forward(ctx context.Context, w http.ResponseWriter, req *strategy.Request, targetURL string, extraHeaders map[string]string) {
	start := time.Now()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(req.Body))
	if err != nil {
		f.logger.LogError("failed to create upstream request", err, slog.String("url", targetURL))
		strategy.WriteError(w, http.StatusInternalServerError, strategy.ErrorTypeServer, "failed to create upstream request")
		return
	}

	for k, values := range req.Headers {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}

	for k, v := range extraHeaders {
		httpReq.Header.Set(k, v)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := f.client.Do(httpReq)
	if err != nil {
		f.logger.LogError("upstream request failed", err, slog.String("url", targetURL))
		strategy.WriteError(w, http.StatusBadGateway, strategy.ErrorTypeUpstream, "upstream error: "+err.Error())
		return
	}
	defer httpResp.Body.Close()

	if req.Stream && httpResp.StatusCode/100 == 2 {
		f.streamResponse(w, httpResp, start)
		return
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		f.logger.LogError("failed to read upstream response", err)
		strategy.WriteError(w, http.StatusBadGateway, strategy.ErrorTypeServer, "failed to read upstream response")
		return
	}

	f.logger.LogResponse(httpResp.StatusCode, time.Since(start))
	f.logger.Info("response body",
		slog.Int("status", httpResp.StatusCode),
		slog.Int("body_len", len(body)),
		slog.String("body", string(body)),
	)

	w.Header().Set("Content-Type", httpResp.Header.Get("Content-Type"))
	w.Header().Set("X-Request-Id", httpResp.Header.Get("X-Request-Id"))
	w.WriteHeader(httpResp.StatusCode)
	w.Write(body)
}

func (f *Forwarder) streamResponse(w http.ResponseWriter, httpResp *http.Response, start time.Time) {
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
				f.logger.LogError("failed to write stream chunk", writeErr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			f.logger.LogResponse(httpResp.StatusCode, time.Since(start))
			return
		}
		if err != nil {
			f.logger.LogError("stream read error", err)
			return
		}
	}
}
