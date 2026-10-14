package strategy

import (
	"errors"
	"net/http"
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
