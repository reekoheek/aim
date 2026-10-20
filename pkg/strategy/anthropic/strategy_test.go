package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
)

func TestHandleWritesAnthropicErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)

	tests := []struct {
		name       string
		providers  map[string]config.ProviderEntry
		provider   string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "provider missing from config",
			provider:   "missing",
			body:       `{"model":"m"}`,
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"provider \"missing\" not found in config"}}`,
		},
		{
			name:       "provider without base URL",
			providers:  map[string]config.ProviderEntry{"p": {}},
			provider:   "p",
			body:       `{"model":"m"}`,
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"type":"error","error":{"type":"api_error","message":"provider \"p\" has empty base_url"}}`,
		},
		{
			name:       "body that is not JSON",
			providers:  map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			provider:   "p",
			body:       `hello`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"type":"error","error":{"type":"invalid_request_error","message":"request body must be a JSON object"}}`,
		},
		{
			name:       "JSON null body",
			providers:  map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			provider:   "p",
			body:       `null`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"type":"error","error":{"type":"invalid_request_error","message":"request body must be a JSON object"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Providers: tt.providers}
			req := &strategy.Request{Body: []byte(tt.body), Errors: strategy.AnthropicErrors{}}
			recorder := httptest.NewRecorder()

			NewStrategy(cfg, logger.New("ERROR"), tt.provider, "upstream-model", strategy.MessagesPath).Handle(context.Background(), recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Errorf("body %s, want %s", got, tt.wantBody)
			}
		})
	}
}

func TestHandleForwardsToProvider(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		query         string
		apiKey        string
		clientHeaders http.Header
		wantPath      string
		wantQuery     string
		wantHeaders   http.Header
	}{
		{
			name:     "messages with the provider API key",
			path:     strategy.MessagesPath,
			apiKey:   "sk-ant-provider",
			wantPath: strategy.MessagesPath,
			wantHeaders: http.Header{
				"X-Api-Key":     {"sk-ant-provider"},
				"Authorization": nil,
			},
		},
		{
			name:     "count tokens",
			path:     strategy.CountTokensPath,
			apiKey:   "sk-ant-provider",
			wantPath: strategy.CountTokensPath,
			wantHeaders: http.Header{
				"X-Api-Key": {"sk-ant-provider"},
			},
		},
		{
			name:   "client credentials are replaced",
			path:   strategy.MessagesPath,
			apiKey: "sk-ant-provider",
			clientHeaders: http.Header{
				"X-Api-Key":     {"sk-client"},
				"Authorization": {"Bearer sk-client"},
			},
			wantPath: strategy.MessagesPath,
			wantHeaders: http.Header{
				"X-Api-Key":     {"sk-ant-provider"},
				"Authorization": nil,
			},
		},
		{
			name:   "anthropic headers pass through",
			path:   strategy.MessagesPath,
			apiKey: "sk-ant-provider",
			clientHeaders: http.Header{
				"Anthropic-Version":        {"2023-06-01"},
				"Anthropic-Beta":           {"first", "second"},
				"X-Claude-Code-Session-Id": {"cc"},
			},
			wantPath: strategy.MessagesPath,
			wantHeaders: http.Header{
				"Anthropic-Version":        {"2023-06-01"},
				"Anthropic-Beta":           {"first", "second"},
				"X-Claude-Code-Session-Id": {"cc"},
			},
		},
		{
			name:      "query passes through",
			path:      strategy.MessagesPath,
			query:     "beta=true",
			apiKey:    "sk-ant-provider",
			wantPath:  strategy.MessagesPath,
			wantQuery: "beta=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type upstreamRequest struct {
				path    string
				query   string
				headers http.Header
				model   string
				fields  int
			}
			received := make(chan upstreamRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				var model string
				json.Unmarshal(body["model"], &model)
				received <- upstreamRequest{path: r.URL.Path, query: r.URL.RawQuery, headers: r.Header.Clone(), model: model, fields: len(body)}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"type":"message"}`))
			}))
			t.Cleanup(upstream.Close)

			cfg := &config.Config{Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL, APIKey: tt.apiKey}}}
			clientHeaders := tt.clientHeaders.Clone()
			req := &strategy.Request{
				Body:    []byte(`{"model":"alias","max_tokens":16,"messages":[]}`),
				Headers: tt.clientHeaders,
				Query:   tt.query,
				Errors:  strategy.AnthropicErrors{},
			}
			recorder := httptest.NewRecorder()

			NewStrategy(cfg, logger.New("ERROR"), "p", "claude-upstream", tt.path).Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			got := <-received
			if got.path != tt.wantPath {
				t.Errorf("path %q, want %q", got.path, tt.wantPath)
			}
			if got.query != tt.wantQuery {
				t.Errorf("query %q, want %q", got.query, tt.wantQuery)
			}
			if got.model != "claude-upstream" {
				t.Errorf("model %q, want %q", got.model, "claude-upstream")
			}
			if got.fields != 3 {
				t.Errorf("upstream received %d fields, want 3", got.fields)
			}
			for name, want := range tt.wantHeaders {
				if values := got.headers.Values(name); !slices.Equal(values, want) {
					t.Errorf("%s = %q, want %q", name, values, want)
				}
			}
			for name, values := range clientHeaders {
				if got := tt.clientHeaders.Values(name); !slices.Equal(got, values) {
					t.Errorf("client header %s changed to %q, want %q", name, got, values)
				}
			}
		})
	}
}
