package strategy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError(t *testing.T) {
	tests := []struct {
		name       string
		write      func(w http.ResponseWriter)
		wantBody   string
		wantStatus int
	}{
		{
			name: "plain message",
			write: func(w http.ResponseWriter) {
				WriteError(w, http.StatusInternalServerError, ErrorTypeServer, "boom")
			},
			wantBody:   `{"error":{"message":"boom","type":"server_error","param":null,"code":null}}`,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "quotes and backslashes are escaped",
			write: func(w http.ResponseWriter) {
				WriteError(w, http.StatusBadGateway, ErrorTypeUpstream, `model "a\b" not found`)
			},
			wantBody:   `{"error":{"message":"model \"a\\b\" not found","type":"upstream_error","param":null,"code":null}}`,
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "newline is escaped",
			write: func(w http.ResponseWriter) {
				WriteError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, "line\nbreak")
			},
			wantBody:   `{"error":{"message":"line\nbreak","type":"invalid_request_error","param":null,"code":null}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "param is set",
			write: func(w http.ResponseWriter) {
				WriteParamError(w, http.StatusBadRequest, ErrorTypeInvalidRequest, "model", `model "x" not found`)
			},
			wantBody:   `{"error":{"message":"model \"x\" not found","type":"invalid_request_error","param":"model","code":null}}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tt.write(recorder)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("content type %q, want %q", got, "application/json")
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Errorf("body %s, want %s", got, tt.wantBody)
			}
		})
	}
}

func TestErrorWriters(t *testing.T) {
	tests := []struct {
		name      string
		writer    ErrorWriter
		status    int
		errorType string
		message   string
		wantBody  string
	}{
		{
			name:      "OpenAI keeps the error type",
			writer:    OpenAIErrors{},
			status:    http.StatusBadGateway,
			errorType: ErrorTypeUpstream,
			message:   "upstream error: refused",
			wantBody:  `{"error":{"message":"upstream error: refused","type":"upstream_error","param":null,"code":null}}`,
		},
		{
			name:      "Anthropic keeps invalid request errors",
			writer:    AnthropicErrors{},
			status:    http.StatusBadRequest,
			errorType: ErrorTypeInvalidRequest,
			message:   `model "x" not found in config`,
			wantBody:  `{"type":"error","error":{"type":"invalid_request_error","message":"model \"x\" not found in config"}}`,
		},
		{
			name:      "Anthropic maps server errors to api errors",
			writer:    AnthropicErrors{},
			status:    http.StatusServiceUnavailable,
			errorType: ErrorTypeServer,
			message:   "llama server failed to start",
			wantBody:  `{"type":"error","error":{"type":"api_error","message":"llama server failed to start"}}`,
		},
		{
			name:      "Anthropic maps upstream errors to api errors",
			writer:    AnthropicErrors{},
			status:    http.StatusBadGateway,
			errorType: ErrorTypeUpstream,
			message:   "upstream error: refused",
			wantBody:  `{"type":"error","error":{"type":"api_error","message":"upstream error: refused"}}`,
		},
		{
			name:      "Anthropic escapes the message",
			writer:    AnthropicErrors{},
			status:    http.StatusBadRequest,
			errorType: ErrorTypeInvalidRequest,
			message:   "line\nbreak",
			wantBody:  `{"type":"error","error":{"type":"invalid_request_error","message":"line\nbreak"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			tt.writer.WriteError(recorder, tt.status, tt.errorType, tt.message)

			if recorder.Code != tt.status {
				t.Errorf("status %d, want %d", recorder.Code, tt.status)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("content type %q, want %q", got, "application/json")
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Errorf("body %s, want %s", got, tt.wantBody)
			}
		})
	}
}
