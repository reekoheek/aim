package strategy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestRequestSessionID(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{name: "no headers", headers: nil, want: noSession},
		{name: "header missing", headers: http.Header{"Authorization": {"Bearer key"}}, want: noSession},
		{name: "header blank", headers: http.Header{"X-Session-Affinity": {"   "}}, want: noSession},
		{name: "header set", headers: http.Header{"X-Session-Affinity": {"abc"}}, want: "abc"},
		{name: "surrounding whitespace is trimmed", headers: http.Header{"X-Session-Affinity": {" abc "}}, want: "abc"},
		{name: "first of repeated values", headers: http.Header{"X-Session-Affinity": {"abc", "def"}}, want: "abc"},
		{name: "claude code session", headers: http.Header{"X-Claude-Code-Session-Id": {"cc"}}, want: "cc"},
		{name: "blank claude code session", headers: http.Header{"X-Claude-Code-Session-Id": {"  "}}, want: noSession},
		{name: "affinity wins over claude code session", headers: http.Header{"X-Session-Affinity": {"abc"}, "X-Claude-Code-Session-Id": {"cc"}}, want: "abc"},
		{name: "blank affinity falls back to claude code session", headers: http.Header{"X-Session-Affinity": {" "}, "X-Claude-Code-Session-Id": {"cc"}}, want: "cc"},
		{name: "subagent of claude code session", headers: http.Header{"X-Claude-Code-Session-Id": {"cc"}, "X-Claude-Code-Agent-Id": {" agent "}}, want: "cc/agent"},
		{name: "subagent of affinity session", headers: http.Header{"X-Session-Affinity": {"abc"}, "X-Claude-Code-Agent-Id": {"agent"}}, want: "abc/agent"},
		{name: "subagent without session", headers: http.Header{"X-Claude-Code-Agent-Id": {"agent"}}, want: noSession + "/agent"},
		{name: "blank agent is ignored", headers: http.Header{"X-Claude-Code-Session-Id": {"cc"}, "X-Claude-Code-Agent-Id": {" "}}, want: "cc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &Request{Headers: tt.headers}
			if got := req.SessionID(); got != tt.want {
				t.Errorf("SessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestBodyFields(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantFields []string
		wantErr    error
	}{
		{name: "JSON object", body: `{"model":"m","stream":true}`, wantFields: []string{"model", "stream"}},
		{name: "empty JSON object", body: `{}`, wantFields: []string{}},
		{name: "malformed JSON", body: `{"model":"m"`, wantErr: ErrBodyNotJSONObject},
		{name: "plain text", body: `hello`, wantErr: ErrBodyNotJSONObject},
		{name: "JSON array", body: `[{"model":"m"}]`, wantErr: ErrBodyNotJSONObject},
		{name: "JSON string", body: `"m"`, wantErr: ErrBodyNotJSONObject},
		{name: "JSON null", body: `null`, wantErr: ErrBodyNotJSONObject},
		{name: "empty body", body: ``, wantErr: ErrBodyNotJSONObject},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &Request{Body: []byte(tt.body)}
			fields, err := req.BodyFields()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			got := []string{}
			for name := range fields {
				got = append(got, name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantFields) {
				t.Errorf("fields %q, want %q", got, tt.wantFields)
			}
		})
	}
}

func TestRequestWriteError(t *testing.T) {
	tests := []struct {
		name     string
		errors   ErrorWriter
		wantBody string
	}{
		{
			name:     "no error writer uses the OpenAI shape",
			wantBody: `{"error":{"message":"boom","type":"server_error","param":null,"code":null}}`,
		},
		{
			name:     "OpenAI error writer",
			errors:   OpenAIErrors{},
			wantBody: `{"error":{"message":"boom","type":"server_error","param":null,"code":null}}`,
		},
		{
			name:     "Anthropic error writer",
			errors:   AnthropicErrors{},
			wantBody: `{"type":"error","error":{"type":"api_error","message":"boom"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := &Request{Errors: tt.errors}
			req.WriteError(recorder, http.StatusServiceUnavailable, ErrorTypeServer, "boom")

			if recorder.Code != http.StatusServiceUnavailable {
				t.Errorf("status %d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Errorf("body %s, want %s", got, tt.wantBody)
			}
			if !json.Valid(recorder.Body.Bytes()) {
				t.Errorf("body %s is not valid JSON", recorder.Body)
			}
		})
	}
}
