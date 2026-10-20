package messages

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/anthropic"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
)

var errUnreadableBody = errors.New("connection reset")

type unreadableBody struct{}

func (unreadableBody) Read(p []byte) (int, error) {
	return 0, errUnreadableBody
}

type endpoint struct {
	name    string
	path    string
	handler func(r *Router) http.HandlerFunc
}

var endpoints = []endpoint{
	{name: "messages", path: strategy.MessagesPath, handler: func(r *Router) http.HandlerFunc { return r.Handler }},
	{name: "count tokens", path: strategy.CountTokensPath, handler: func(r *Router) http.HandlerFunc { return r.CountTokensHandler }},
}

type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAnthropicError(t *testing.T, recorder *httptest.ResponseRecorder) anthropicError {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type %q, want %q", got, "application/json")
	}
	var got anthropicError
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s is not JSON: %v", recorder.Body, err)
	}
	if got.Type != "error" {
		t.Errorf("envelope type %q, want %q", got.Type, "error")
	}
	return got
}

func TestHandlerRejectsInvalidRequest(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{name: "model missing from request", body: `{"max_tokens":16}`, wantMessage: "model is required"},
		{name: "model missing from config", body: `{"model":"missing"}`, wantMessage: `model "missing" not found in config`},
		{name: "provider missing from config", body: `{"model":"orphan"}`, wantMessage: `provider "gone" not found in config`},
		{name: "chat-completion model", body: `{"model":"chat"}`, wantMessage: `model "chat" is type chat-completion, use /v1/chat/completions`},
		{name: "systemone model", body: `{"model":"decider"}`, wantMessage: `model "decider" is type systemone, use /v1/systemone`},
		{name: "unreadable body", wantMessage: "failed to read request body"},
		{name: "malformed JSON", body: `{"model":`, wantMessage: "invalid JSON"},
		{name: "JSON array", body: `[{"model":"claude"}]`, wantMessage: "invalid JSON"},
	}

	cfg := &config.Config{
		Models: map[string]config.ModelEntry{
			"orphan":  {Type: config.ModelTypeAnthropic, Provider: "gone", Model: "upstream-claude"},
			"chat":    {Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-chat"},
			"decider": {Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"},
		},
		Providers: map[string]config.ProviderEntry{"p": {BaseURL: "http://127.0.0.1:1"}},
	}

	for _, e := range endpoints {
		for _, tt := range tests {
			t.Run(e.name+"/"+tt.name, func(t *testing.T) {
				var body io.Reader = strings.NewReader(tt.body)
				if tt.body == "" {
					body = unreadableBody{}
				}
				recorder := httptest.NewRecorder()
				e.handler(New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()))(recorder, httptest.NewRequest(http.MethodPost, e.path, body))

				if recorder.Code != http.StatusBadRequest {
					t.Errorf("status %d, want %d", recorder.Code, http.StatusBadRequest)
				}
				got := decodeAnthropicError(t, recorder)
				if got.Error.Message != tt.wantMessage || got.Error.Type != strategy.ErrorTypeInvalidRequest {
					t.Errorf("error %q of type %q, want %q of type %q", got.Error.Message, got.Error.Type, tt.wantMessage, strategy.ErrorTypeInvalidRequest)
				}
			})
		}
	}
}

func TestHandlerForwardsAnthropicModelToProvider(t *testing.T) {
	for _, e := range endpoints {
		t.Run(e.name, func(t *testing.T) {
			type upstreamRequest struct {
				path   string
				query  string
				apiKey string
				model  string
			}
			received := make(chan upstreamRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				received <- upstreamRequest{path: r.URL.Path, query: r.URL.RawQuery, apiKey: r.Header.Get("X-Api-Key"), model: body.Model}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "req_1")
				w.Write([]byte(`{"type":"message"}`))
			}))
			t.Cleanup(upstream.Close)

			cfg := &config.Config{
				Models:    map[string]config.ModelEntry{"claude": {Type: config.ModelTypeAnthropic, Provider: "p", Model: "claude-upstream"}},
				Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL, APIKey: "sk-ant-provider"}},
			}
			req := httptest.NewRequest(http.MethodPost, e.path+"?beta=true", strings.NewReader(`{"model":"claude","messages":[]}`))
			req.Header.Set("X-Api-Key", "sk-client")

			recorder := httptest.NewRecorder()
			e.handler(New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()))(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := recorder.Body.String(); got != `{"type":"message"}` {
				t.Errorf("response %s, want %s", got, `{"type":"message"}`)
			}
			if got := recorder.Header().Get("Request-Id"); got != "req_1" {
				t.Errorf("Request-Id %q, want %q", got, "req_1")
			}
			want := upstreamRequest{path: e.path, query: "beta=true", apiKey: "sk-ant-provider", model: "claude-upstream"}
			if got := <-received; got != want {
				t.Errorf("upstream received %+v, want %+v", got, want)
			}
		})
	}
}

func TestHandlerPassesUpstreamErrorsThrough(t *testing.T) {
	const upstreamError = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(529)
		w.Write([]byte(upstreamError))
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		Models:    map[string]config.ModelEntry{"claude": {Type: config.ModelTypeAnthropic, Provider: "p", Model: "claude-upstream"}},
		Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
	}

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream %v", stream), func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"claude","stream":%v}`, stream)
			recorder := httptest.NewRecorder()
			New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()).Handler(recorder, httptest.NewRequest(http.MethodPost, strategy.MessagesPath, strings.NewReader(body)))

			if recorder.Code != 529 {
				t.Errorf("status %d, want %d", recorder.Code, 529)
			}
			if got := recorder.Body.String(); got != upstreamError {
				t.Errorf("body %s, want %s", got, upstreamError)
			}
			if got := recorder.Header().Get("Retry-After"); got != "3" {
				t.Errorf("Retry-After %q, want %q", got, "3")
			}
		})
	}
}

func TestNewStrategy(t *testing.T) {
	tests := []struct {
		name            string
		entry           config.ModelEntry
		wantMessages    strategy.Strategy
		wantCountTokens strategy.Strategy
		wantErr         string
	}{
		{
			name:            "anthropic",
			entry:           config.ModelEntry{Type: config.ModelTypeAnthropic, Provider: "p", Model: "claude-upstream"},
			wantMessages:    &anthropic.Strategy{},
			wantCountTokens: &anthropic.Strategy{},
		},
		{
			name:            "llama-server",
			entry:           config.ModelEntry{Type: config.ModelTypeLlamaServer},
			wantMessages:    &llamaserver.Strategy{},
			wantCountTokens: &llamaserver.SlotlessStrategy{},
		},
		{
			name:    "chat-completion belongs to its own endpoint",
			entry:   config.ModelEntry{Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-chat"},
			wantErr: `model "m" is type chat-completion, use /v1/chat/completions`,
		},
		{
			name:    "systemone belongs to its own endpoint",
			entry:   config.ModelEntry{Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"},
			wantErr: `model "m" is type systemone, use /v1/systemone`,
		},
		{
			name:    "unsupported type",
			entry:   config.ModelEntry{Type: "provider"},
			wantErr: `unsupported type "provider"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := New(&config.Config{}, logger.New("ERROR"), llamaserver.NewMonitor())
			for _, factory := range []struct {
				name string
				new  strategyFactory
				want strategy.Strategy
			}{
				{name: "messages", new: router.newMessagesStrategy, want: tt.wantMessages},
				{name: "count tokens", new: router.newCountTokensStrategy, want: tt.wantCountTokens},
			} {
				got, err := factory.new("m", tt.entry)
				if tt.wantErr != "" {
					if err == nil || err.Error() != tt.wantErr {
						t.Errorf("%s: error %v, want %q", factory.name, err, tt.wantErr)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", factory.name, err)
				}
				if gotType, wantType := fmt.Sprintf("%T", got), fmt.Sprintf("%T", factory.want); gotType != wantType {
					t.Errorf("%s: strategy %s, want %s", factory.name, gotType, wantType)
				}
			}
		})
	}
}
