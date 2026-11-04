package images

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/strategy"
)

var testLogger = logger.New("ERROR")

type formField struct {
	name  string
	value string
	file  bool
}

func multipartBody(t *testing.T, fields []formField) (string, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, field := range fields {
		if field.file {
			part, err := writer.CreateFormFile(field.name, field.name+".png")
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte(field.value))
			continue
		}
		if err := writer.WriteField(field.name, field.value); err != nil {
			t.Fatal(err)
		}
	}
	writer.Close()
	return body.String(), writer.FormDataContentType()
}

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	cfg := &config.Config{
		Models: map[string]config.ModelEntry{
			"flux": {Type: config.ModelTypeSDServer, SDCpp: &config.SDCppEntry{
				Lane: config.DefaultLane,
				Bin:  filepath.Join(t.TempDir(), "missing-sd-server"),
			}},
			"chat":    {Type: config.ModelTypeChatCompletion, Provider: "p", Model: "upstream-chat"},
			"local":   {Type: config.ModelTypeLlamaServer, LlamaCpp: &config.LlamaCppEntry{}},
			"decider": {Type: config.ModelTypeSystemOne, Provider: "p", Model: "upstream-decider"},
			"claude":  {Type: config.ModelTypeAnthropic, Provider: "p", Model: "upstream-claude"},
		},
		Providers: map[string]config.ProviderEntry{"p": {BaseURL: "http://127.0.0.1:1"}},
	}
	return New(cfg, testLogger, lane.NewSupervisor(0, testLogger))
}

type errorBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
	} `json:"error"`
}

func decodeError(t *testing.T, recorder *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s is not JSON: %v", recorder.Body, err)
	}
	return got
}

func TestGenerationsHandler(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{name: "invalid JSON", body: `not json`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: "invalid JSON"},
		{name: "model missing", body: `{"prompt":"a cat"}`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: "model is required"},
		{name: "model not in config", body: `{"model":"missing"}`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: `model "missing" not found in config`},
		{name: "chat-completion model", body: `{"model":"chat"}`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: `model "chat" is type chat-completion, use /v1/chat/completions`},
		{name: "llama-server model", body: `{"model":"local"}`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: `model "local" is type llama-server, use /v1/chat/completions`},
		{name: "systemone model", body: `{"model":"decider"}`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: `model "decider" is type systemone, use /v1/systemone`},
		{name: "anthropic model", body: `{"model":"claude"}`, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: `model "claude" is type anthropic, use /v1/messages`},
		{name: "sd-server model reaches sd-server", body: `{"model":"flux","prompt":"a cat"}`, wantStatus: http.StatusServiceUnavailable, wantType: strategy.ErrorTypeServer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, strategy.ImageGenerationsPath, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", strategy.JSONContentType)
			newTestRouter(t).GenerationsHandler(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
			}
			got := decodeError(t, recorder)
			if got.Error.Type != tt.wantType {
				t.Errorf("type %q, want %q", got.Error.Type, tt.wantType)
			}
			if tt.wantMessage != "" && got.Error.Message != tt.wantMessage {
				t.Errorf("message %q, want %q", got.Error.Message, tt.wantMessage)
			}
		})
	}
}

func TestEditsHandler(t *testing.T) {
	tests := []struct {
		name        string
		fields      []formField
		contentType string
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{name: "not multipart", contentType: strategy.JSONContentType, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: errNotMultipart.Error()},
		{name: "multipart without boundary", contentType: "multipart/form-data", wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: errNotMultipart.Error()},
		{name: "model missing", fields: []formField{{name: "prompt", value: "a hat"}}, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: "model is required"},
		{name: "llama-server model", fields: []formField{{name: "model", value: "local"}}, wantStatus: http.StatusBadRequest, wantType: strategy.ErrorTypeInvalidRequest, wantMessage: `model "local" is type llama-server, use /v1/chat/completions`},
		{name: "sd-server model after image reaches sd-server", fields: []formField{{name: "image", value: "png", file: true}, {name: "model", value: "flux"}}, wantStatus: http.StatusServiceUnavailable, wantType: strategy.ErrorTypeServer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := multipartBody(t, tt.fields)
			if tt.contentType != "" {
				contentType = tt.contentType
			}
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, strategy.ImageEditsPath, strings.NewReader(body))
			req.Header.Set("Content-Type", contentType)
			newTestRouter(t).EditsHandler(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
			}
			got := decodeError(t, recorder)
			if got.Error.Type != tt.wantType {
				t.Errorf("type %q, want %q", got.Error.Type, tt.wantType)
			}
			if tt.wantMessage != "" && got.Error.Message != tt.wantMessage {
				t.Errorf("message %q, want %q", got.Error.Message, tt.wantMessage)
			}
		})
	}
}

func TestMultipartModel(t *testing.T) {
	tests := []struct {
		name    string
		fields  []formField
		body    string
		want    string
		wantErr error
	}{
		{name: "model field", fields: []formField{{name: "model", value: "flux"}}, want: "flux"},
		{name: "model after file", fields: []formField{{name: "image", value: "png", file: true}, {name: "model", value: "flux"}}, want: "flux"},
		{name: "first model field wins", fields: []formField{{name: "model", value: "flux"}, {name: "model", value: "other"}}, want: "flux"},
		{name: "no model field", fields: []formField{{name: "prompt", value: "x"}}, want: ""},
		{name: "broken body", body: "--not-the-boundary", wantErr: errInvalidMultipart},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType := multipartBody(t, tt.fields)
			if tt.body != "" {
				body = tt.body
			}
			got, err := multipartModel([]byte(body), contentType)
			if err != tt.wantErr {
				t.Fatalf("got error %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
