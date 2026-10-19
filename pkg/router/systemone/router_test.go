package systemone

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
	"github.com/reekoheek/aim/pkg/strategy/systemone"
)

const validQuestions = `{"is_angry":{"type":"noul","instructions":"Is the customer angry?"}}`

var errUnreadableBody = errors.New("connection reset")

type unreadableBody struct{}

func (unreadableBody) Read(p []byte) (int, error) {
	return 0, errUnreadableBody
}

func serveSystemOne(router *Router, body io.Reader) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.Handler(recorder, httptest.NewRequest(http.MethodPost, strategy.SystemOnePath, body))
	return recorder
}

func TestHandlerRejectsInvalidRequest(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantParam   string
		wantMessage string
	}{
		{
			name:        "model missing from request",
			body:        `{"questions":` + validQuestions + `}`,
			wantParam:   modelParam,
			wantMessage: "model is required",
		},
		{
			name:        "questions missing",
			body:        `{"model":"decider","state":"hi"}`,
			wantParam:   questionsParam,
			wantMessage: "questions must be a non-empty object",
		},
		{
			name:        "questions null",
			body:        `{"model":"decider","questions":null}`,
			wantParam:   questionsParam,
			wantMessage: "questions must be a non-empty object",
		},
		{
			name:        "questions empty object",
			body:        `{"model":"decider","questions":{}}`,
			wantParam:   questionsParam,
			wantMessage: "questions must be a non-empty object",
		},
		{
			name:        "questions array",
			body:        `{"model":"decider","questions":[` + validQuestions + `]}`,
			wantParam:   questionsParam,
			wantMessage: "questions must be a non-empty object",
		},
		{
			name:        "questions string",
			body:        `{"model":"decider","questions":"is_angry"}`,
			wantParam:   questionsParam,
			wantMessage: "questions must be a non-empty object",
		},
		{
			name:        "llama-server model checked before it starts",
			body:        `{"model":"local","questions":{}}`,
			wantParam:   questionsParam,
			wantMessage: "questions must be a non-empty object",
		},
		{
			name:        "model missing from config",
			body:        `{"model":"missing","questions":` + validQuestions + `}`,
			wantParam:   modelParam,
			wantMessage: `model "missing" not found in config`,
		},
		{
			name:        "provider missing from config",
			body:        `{"model":"orphan","questions":` + validQuestions + `}`,
			wantParam:   modelParam,
			wantMessage: `provider "gone" not found in config`,
		},
		{
			name:        "chat-completion model",
			body:        `{"model":"chat","questions":` + validQuestions + `}`,
			wantParam:   modelParam,
			wantMessage: `model "chat" is type chat-completion, use /v1/chat/completions`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Models: map[string]config.ModelEntry{
					"decider": {Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"},
					"orphan":  {Type: config.ModelTypeSystemOne, Provider: "gone", Model: "upstream-decider"},
					"chat":    {Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-chat"},
					"local": {
						Type:     config.ModelTypeLlamaServer,
						LlamaCpp: &config.LlamaCppEntry{Bin: filepath.Join(t.TempDir(), "missing-llama-server")},
					},
				},
				Providers: map[string]config.ProviderEntry{"p": {BaseURL: "http://127.0.0.1:1"}},
			}

			recorder := serveSystemOne(New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()), strings.NewReader(tt.body))

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
			if got.Error.Message != tt.wantMessage || got.Error.Type != strategy.ErrorTypeInvalidRequest {
				t.Errorf("error %q of type %q, want %q of type %q", got.Error.Message, got.Error.Type, tt.wantMessage, strategy.ErrorTypeInvalidRequest)
			}
			if got.Error.Param == nil || *got.Error.Param != tt.wantParam {
				t.Errorf("param %v, want %q", got.Error.Param, tt.wantParam)
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
			recorder := serveSystemOne(New(&config.Config{}, logger.New("ERROR"), llamaserver.NewMonitor()), tt.body)

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

func TestHandlerForwardsSystemOneModelToProvider(t *testing.T) {
	tests := []struct {
		name  string
		extra string
	}{
		{name: "state and questions only", extra: ``},
		{name: "fields aim does not read", extra: `,"stream":true,"files":["data:image/png;base64,AA=="]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type upstreamRequest struct {
				path string
				body map[string]json.RawMessage
			}
			received := make(chan upstreamRequest, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				received <- upstreamRequest{path: r.URL.Path, body: body}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"answers":{}}`))
			}))
			t.Cleanup(upstream.Close)

			cfg := &config.Config{
				Models:    map[string]config.ModelEntry{"decider": {Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"}},
				Providers: map[string]config.ProviderEntry{"p": {BaseURL: upstream.URL}},
			}
			body := `{"model":"decider","state":"hi","questions":` + validQuestions + tt.extra + `}`

			recorder := serveSystemOne(New(cfg, logger.New("ERROR"), llamaserver.NewMonitor()), strings.NewReader(body))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := recorder.Body.String(); got != `{"answers":{}}` {
				t.Errorf("response %s, want %s", got, `{"answers":{}}`)
			}
			got := <-received
			if got.path != strategy.SystemOnePath {
				t.Errorf("path %q, want %q", got.path, strategy.SystemOnePath)
			}
			if model := string(got.body["model"]); model != `"upstream-decider"` {
				t.Errorf("model %s, want %s", model, `"upstream-decider"`)
			}
			if questions := string(got.body["questions"]); questions != validQuestions {
				t.Errorf("questions %s, want %s", questions, validQuestions)
			}
			var sent map[string]json.RawMessage
			json.Unmarshal([]byte(body), &sent)
			if len(got.body) != len(sent) {
				t.Errorf("upstream received %d fields, want %d", len(got.body), len(sent))
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
			name:  "systemone",
			entry: config.ModelEntry{Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"},
			want:  &systemone.Strategy{},
		},
		{
			name:  "llama-server",
			entry: config.ModelEntry{Type: config.ModelTypeLlamaServer},
			want:  &llamaserver.SystemOneStrategy{},
		},
		{
			name:    "chat-completion belongs to its own endpoint",
			entry:   config.ModelEntry{Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-chat"},
			wantErr: `model "m" is type chat-completion, use /v1/chat/completions`,
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
