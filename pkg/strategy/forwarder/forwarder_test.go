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
