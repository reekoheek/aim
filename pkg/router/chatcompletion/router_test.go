package chatcompletion

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/chatcompletion"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
)

var errUnreadableBody = errors.New("connection reset")

type unreadableBody struct{}

func (unreadableBody) Read(p []byte) (int, error) {
	return 0, errUnreadableBody
}

func serveCompletion(router *Router, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.Handler(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return recorder
}

func TestHandlerRejectsUnresolvableModel(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "model missing from request",
			body:        `{}`,
			wantMessage: "model is required",
		},
		{
			name:        "model missing from config",
			body:        `{"model":"missing"}`,
			wantMessage: `model "missing" not found in config`,
		},
		{
			name:        "provider missing from config",
			body:        `{"model":"orphan"}`,
			wantMessage: `provider "gone" not found in config`,
		},
		{
			name:        "systemone model",
			body:        `{"model":"decider"}`,
			wantMessage: `model "decider" is type systemone, use /v1/systemone`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Models: map[string]config.ModelEntry{
					"orphan":  {Type: config.ModelTypeChatCompletion, Provider: "gone", Model: "upstream-model"},
					"decider": {Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"},
				},
				Providers: map[string]config.ProviderEntry{"p": {BaseURL: "http://127.0.0.1:1"}},
			}

			recorder := serveCompletion(New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()), tt.body)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("content type %q, want %q", got, "application/json")
			}
			var got struct {
				Error struct {
					Message string  `json:"message"`
					Type    string  `json:"type"`
					Param   *string `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %s is not JSON: %v", recorder.Body, err)
			}
			if got.Error.Message != tt.wantMessage {
				t.Errorf("message %q, want %q", got.Error.Message, tt.wantMessage)
			}
			if got.Error.Param == nil || *got.Error.Param != modelParam {
				t.Errorf("param %v, want %q", got.Error.Param, modelParam)
			}
		})
	}
}

func TestHandlerFollowsModelConfigOnEveryRequest(t *testing.T) {
	tests := []struct {
		name           string
		upstreamModels []string
	}{
		{name: "same upstream model", upstreamModels: []string{"first", "first"}},
		{name: "upstream model changed between requests", upstreamModels: []string{"first", "second"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan string, len(tt.upstreamModels))
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				received <- body.Model
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[]}`))
			}))
			t.Cleanup(upstream.Close)

			cfg := &config.Config{
				Models:    map[string]config.ModelEntry{},
				Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			}
			router := New(cfg, logger.New("ERROR"), llamaserver.NewMonitor())

			var got []string
			for _, upstreamModel := range tt.upstreamModels {
				cfg.Models["m"] = config.ModelEntry{Type: config.ModelTypeChatCompletion, Provider: "p", Model: upstreamModel}
				if recorder := serveCompletion(router, `{"model":"m"}`); recorder.Code != http.StatusOK {
					t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
				}
				got = append(got, <-received)
			}

			if !slices.Equal(got, tt.upstreamModels) {
				t.Errorf("upstream received models %q, want %q", got, tt.upstreamModels)
			}
		})
	}
}

func TestHandlerForwardsEveryHeaderValue(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{name: "single value", values: []string{"first"}},
		{name: "repeated header", values: []string{"first", "second", "third"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan []string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Values("Anthropic-Beta")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[]}`))
			}))
			t.Cleanup(upstream.Close)

			cfg := &config.Config{
				Models:    map[string]config.ModelEntry{"m": {Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-model"}},
				Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			for _, value := range tt.values {
				req.Header.Add("Anthropic-Beta", value)
			}

			recorder := httptest.NewRecorder()
			New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()).Handler(recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := <-received; !slices.Equal(got, tt.values) {
				t.Errorf("upstream received %q, want %q", got, tt.values)
			}
		})
	}
}

func TestHandlerWritesBodyErrorsAsJSON(t *testing.T) {
	tests := []struct {
		name        string
		body        io.Reader
		wantMessage string
	}{
		{name: "unreadable body", body: unreadableBody{}, wantMessage: "failed to read request body"},
		{name: "malformed JSON", body: strings.NewReader(`{"model":`), wantMessage: "invalid JSON"},
		{name: "JSON array", body: strings.NewReader(`[{"model":"m"}]`), wantMessage: "invalid JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			New(&config.Config{}, logger.New("ERROR"), llamaserver.NewMonitor()).Handler(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", tt.body))

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("content type %q, want %q", got, "application/json")
			}
			var got struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %s is not JSON: %v", recorder.Body, err)
			}
			if got.Error.Message != tt.wantMessage || got.Error.Type != strategy.ErrorTypeInvalidRequest {
				t.Errorf("error %q of type %q, want %q of type %q", got.Error.Message, got.Error.Type, tt.wantMessage, strategy.ErrorTypeInvalidRequest)
			}
		})
	}
}

func TestNewStrategy(t *testing.T) {
	tests := []struct {
		name    string
		entry   config.ModelEntry
		want    strategy.Strategy
		wantErr string
	}{
		{
			name:  "chat-completion",
			entry: config.ModelEntry{Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-model"},
			want:  &chatcompletion.Strategy{},
		},
		{
			name:  "llama-server",
			entry: config.ModelEntry{Type: config.ModelTypeLlamaServer},
			want:  &llamaserver.Strategy{},
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
			got, err := New(&config.Config{}, logger.New("ERROR"), llamaserver.NewMonitor()).newStrategy("m", tt.entry)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotType, wantType := fmt.Sprintf("%T", got), fmt.Sprintf("%T", tt.want); gotType != wantType {
				t.Errorf("strategy %s, want %s", gotType, wantType)
			}
		})
	}
}
