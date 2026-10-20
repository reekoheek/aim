package forwarder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/strategy"
)

const unbuildableTargetURL = "http://bad host/v1/chat/completions"

func TestForward(t *testing.T) {
	tests := []struct {
		name            string
		upstream        http.HandlerFunc
		upstreamDown    bool
		targetURL       string
		stream          bool
		wantStatus      int
		wantContentType string
		wantBody        string
	}{
		{
			name: "response is copied",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"ok":true}`))
			},
			wantStatus:      http.StatusOK,
			wantContentType: "application/json",
			wantBody:        `{"ok":true}`,
		},
		{
			name: "error status is passed through",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"slow down"}`))
			},
			wantStatus:      http.StatusTooManyRequests,
			wantContentType: "application/json",
			wantBody:        `{"error":"slow down"}`,
		},
		{
			name: "stream is relayed as server-sent events",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("data: chunk\n\n"))
			},
			stream:          true,
			wantStatus:      http.StatusOK,
			wantContentType: "text/event-stream",
			wantBody:        "data: chunk\n\n",
		},
		{
			name:            "unreachable upstream returns bad gateway",
			upstreamDown:    true,
			wantStatus:      http.StatusBadGateway,
			wantContentType: "application/json",
			wantBody:        `"type":"upstream_error"`,
		},
		{
			name:            "unbuildable target URL returns a server error",
			targetURL:       unbuildableTargetURL,
			wantStatus:      http.StatusInternalServerError,
			wantContentType: "application/json",
			wantBody:        `"message":"failed to create upstream request"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targetURL := tt.targetURL
			if targetURL == "" {
				upstream := httptest.NewServer(tt.upstream)
				targetURL = upstream.URL
				if tt.upstreamDown {
					upstream.Close()
				} else {
					t.Cleanup(upstream.Close)
				}
			}

			recorder := httptest.NewRecorder()
			req := &strategy.Request{Stream: tt.stream, Body: []byte(`{}`)}
			New(&http.Client{}, logger.New("ERROR")).Forward(context.Background(), recorder, req, targetURL, nil)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
			}
			if got := recorder.Header().Get("Content-Type"); got != tt.wantContentType {
				t.Errorf("content type %q, want %q", got, tt.wantContentType)
			}
			if got := recorder.Body.String(); !strings.Contains(got, tt.wantBody) {
				t.Errorf("body %q, want containing %q", got, tt.wantBody)
			}
			if tt.wantContentType == "application/json" && !json.Valid(recorder.Body.Bytes()) {
				t.Errorf("body %s is not valid JSON", recorder.Body)
			}
		})
	}
}

func TestForwardUpstreamHeaders(t *testing.T) {
	tests := []struct {
		name         string
		headers      http.Header
		extraHeaders map[string]string
		want         http.Header
	}{
		{
			name:    "end-to-end headers are forwarded",
			headers: http.Header{"X-Session-Affinity": {"s1"}},
			want:    http.Header{"X-Session-Affinity": {"s1"}},
		},
		{
			name:    "every value of a repeated header is forwarded",
			headers: http.Header{"Anthropic-Beta": {"first", "second"}},
			want:    http.Header{"Anthropic-Beta": {"first", "second"}},
		},
		{
			name:    "hop-by-hop headers are dropped",
			headers: http.Header{"Proxy-Authorization": {"secret"}, "Keep-Alive": {"timeout=5"}},
			want:    http.Header{"Proxy-Authorization": nil, "Keep-Alive": nil},
		},
		{
			name:         "extra headers override client headers",
			headers:      http.Header{"Authorization": {"Bearer client", "Bearer other"}},
			extraHeaders: map[string]string{"Authorization": "Bearer provider"},
			want:         http.Header{"Authorization": {"Bearer provider"}},
		},
		{
			name:    "content type is always JSON",
			headers: http.Header{"Content-Type": {"text/plain"}},
			want:    http.Header{"Content-Type": {"application/json"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Clone()
			}))
			t.Cleanup(upstream.Close)

			req := &strategy.Request{Body: []byte(`{}`), Headers: tt.headers}
			New(&http.Client{}, logger.New("ERROR")).Forward(context.Background(), httptest.NewRecorder(), req, upstream.URL, tt.extraHeaders)

			got := <-received
			for name, want := range tt.want {
				if values := got.Values(name); !slices.Equal(values, want) {
					t.Errorf("%s = %q, want %q", name, values, want)
				}
			}
		})
	}
}

func TestForwardQuery(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		wantQuery string
	}{
		{name: "no query", query: "", wantQuery: ""},
		{name: "beta flag", query: "beta=true", wantQuery: "beta=true"},
		{name: "several parameters", query: "beta=true&x=1", wantQuery: "beta=true&x=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.URL.RawQuery
			}))
			t.Cleanup(upstream.Close)

			req := &strategy.Request{Body: []byte(`{}`), Query: tt.query}
			New(&http.Client{}, logger.New("ERROR")).Forward(context.Background(), httptest.NewRecorder(), req, upstream.URL+strategy.MessagesPath, nil)

			if got := <-received; got != tt.wantQuery {
				t.Errorf("query %q, want %q", got, tt.wantQuery)
			}
		})
	}
}

func TestForwardResponseHeaders(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
		want   http.Header
	}{
		{
			name: "end-to-end headers are copied",
			want: http.Header{
				"Request-Id":                         {"req_1"},
				"Retry-After":                        {"7"},
				"X-Should-Retry":                     {"true"},
				"Anthropic-Ratelimit-Requests-Limit": {"50"},
				"X-Request-Id":                       {"abc"},
				"Content-Type":                       {"application/json"},
				"Proxy-Authenticate":                 nil,
			},
		},
		{
			name:   "streamed response keeps end-to-end headers",
			stream: true,
			want: http.Header{
				"Request-Id":         {"req_1"},
				"Retry-After":        {"7"},
				"Content-Type":       {"text/event-stream"},
				"Proxy-Authenticate": nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Request-Id", "req_1")
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Should-Retry", "true")
				w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "50")
				w.Header().Set("X-Request-Id", "abc")
				w.Header().Set("Proxy-Authenticate", "Basic")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{}`))
			}))
			t.Cleanup(upstream.Close)

			recorder := httptest.NewRecorder()
			req := &strategy.Request{Stream: tt.stream, Body: []byte(`{}`)}
			New(&http.Client{}, logger.New("ERROR")).Forward(context.Background(), recorder, req, upstream.URL, nil)

			for name, want := range tt.want {
				if values := recorder.Header().Values(name); !slices.Equal(values, want) {
					t.Errorf("%s = %q, want %q", name, values, want)
				}
			}
		})
	}
}

func TestForwardWritesErrorsWithRequestErrorWriter(t *testing.T) {
	tests := []struct {
		name     string
		errors   strategy.ErrorWriter
		wantBody string
	}{
		{name: "OpenAI shape by default", wantBody: `{"error":{"message":"failed to create upstream request","type":"server_error","param":null,"code":null}}`},
		{name: "Anthropic shape", errors: strategy.AnthropicErrors{}, wantBody: `{"type":"error","error":{"type":"api_error","message":"failed to create upstream request"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := &strategy.Request{Body: []byte(`{}`), Errors: tt.errors}
			New(&http.Client{}, logger.New("ERROR")).Forward(context.Background(), recorder, req, unbuildableTargetURL, nil)

			if recorder.Code != http.StatusInternalServerError {
				t.Errorf("status %d, want %d", recorder.Code, http.StatusInternalServerError)
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Errorf("body %s, want %s", got, tt.wantBody)
			}
		})
	}
}
