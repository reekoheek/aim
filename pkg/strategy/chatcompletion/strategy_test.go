package chatcompletion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
)

func TestHandleWritesErrorsAsJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(upstream.Close)

	tests := []struct {
		name        string
		providers   map[string]config.ProviderEntry
		provider    string
		body        string
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:        "provider missing from config",
			provider:    "missing",
			body:        `{"model":"m"}`,
			wantStatus:  http.StatusInternalServerError,
			wantType:    strategy.ErrorTypeServer,
			wantMessage: `provider "missing" not found in config`,
		},
		{
			name:        "provider without base URL",
			providers:   map[string]config.ProviderEntry{"p": {}},
			provider:    "p",
			body:        `{"model":"m"}`,
			wantStatus:  http.StatusInternalServerError,
			wantType:    strategy.ErrorTypeServer,
			wantMessage: `provider "p" has empty base_url`,
		},
		{
			name:        "body that is not JSON",
			providers:   map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			provider:    "p",
			body:        `hello`,
			wantStatus:  http.StatusBadRequest,
			wantType:    strategy.ErrorTypeInvalidRequest,
			wantMessage: strategy.ErrBodyNotJSONObject.Error(),
		},
		{
			name:        "JSON null body",
			providers:   map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			provider:    "p",
			body:        `null`,
			wantStatus:  http.StatusBadRequest,
			wantType:    strategy.ErrorTypeInvalidRequest,
			wantMessage: strategy.ErrBodyNotJSONObject.Error(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Providers: tt.providers}
			req := &strategy.Request{Body: []byte(tt.body)}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, logger.New("ERROR"), tt.provider, "upstream-model").Handle(context.Background(), recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
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
			if got.Error.Message != tt.wantMessage || got.Error.Type != tt.wantType {
				t.Errorf("error %q of type %q, want %q of type %q", got.Error.Message, got.Error.Type, tt.wantMessage, tt.wantType)
			}
		})
	}
}

func TestHandleSendsProviderCredentialsAndModel(t *testing.T) {
	tests := []struct {
		name              string
		apiKey            string
		clientHeaders     http.Header
		wantAuthorization string
	}{
		{
			name:              "provider API key",
			apiKey:            "sk-provider",
			wantAuthorization: "Bearer sk-provider",
		},
		{
			name:              "provider API key replaces the client's",
			apiKey:            "sk-provider",
			clientHeaders:     http.Header{"Authorization": {"Bearer sk-client"}},
			wantAuthorization: "Bearer sk-provider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type upstreamRequest struct {
				authorization []string
				model         string
			}
			received := make(chan upstreamRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				received <- upstreamRequest{authorization: r.Header.Values("Authorization"), model: body.Model}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[]}`))
			}))
			t.Cleanup(upstream.Close)

			cfg := &config.Config{Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL, APIKey: tt.apiKey}}}
			req := &strategy.Request{Body: []byte(`{"model":"alias"}`), Headers: tt.clientHeaders}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, logger.New("ERROR"), "p", "upstream-model").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			got := <-received
			if len(got.authorization) != 1 || got.authorization[0] != tt.wantAuthorization {
				t.Errorf("Authorization %q, want %q", got.authorization, tt.wantAuthorization)
			}
			if got.model != "upstream-model" {
				t.Errorf("model %q, want %q", got.model, "upstream-model")
			}
		})
	}
}
