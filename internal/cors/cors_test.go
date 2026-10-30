package cors

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestNew(t *testing.T) {
	const allowedOrigin = "http://localhost:3000"

	tests := []struct {
		name        string
		origins     []string
		method      string
		headers     map[string]string
		wantStatus  int
		wantNext    bool
		wantHeaders http.Header
	}{
		{
			name:       "no origins configured leaves cors off",
			method:     http.MethodPost,
			headers:    map[string]string{"Origin": allowedOrigin},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin": nil,
				"Vary":                        nil,
			},
		},
		{
			name:       "no origins configured does not answer preflight",
			method:     http.MethodOptions,
			headers:    map[string]string{"Origin": allowedOrigin, "Access-Control-Request-Method": "POST"},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin":  nil,
				"Access-Control-Allow-Methods": nil,
			},
		},
		{
			name:       "allowed origin on actual request",
			origins:    []string{allowedOrigin},
			method:     http.MethodPost,
			headers:    map[string]string{"Origin": allowedOrigin},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin":  {allowedOrigin},
				"Access-Control-Allow-Methods": nil,
				"Vary":                         {"Origin"},
			},
		},
		{
			name:       "allowed origin preflight",
			origins:    []string{allowedOrigin},
			method:     http.MethodOptions,
			headers:    map[string]string{"Origin": allowedOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type, x-api-key"},
			wantStatus: http.StatusNoContent,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin":  {allowedOrigin},
				"Access-Control-Allow-Methods": {allowedMethods},
				"Access-Control-Allow-Headers": {"content-type, x-api-key"},
				"Access-Control-Max-Age":       {preflightMaxAgeSeconds},
				"Vary":                         {"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"},
			},
		},
		{
			name:       "preflight without requested headers",
			origins:    []string{allowedOrigin},
			method:     http.MethodOptions,
			headers:    map[string]string{"Origin": allowedOrigin, "Access-Control-Request-Method": "POST"},
			wantStatus: http.StatusNoContent,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin":  {allowedOrigin},
				"Access-Control-Allow-Headers": nil,
			},
		},
		{
			name:       "options without request method is not a preflight",
			origins:    []string{allowedOrigin},
			method:     http.MethodOptions,
			headers:    map[string]string{"Origin": allowedOrigin},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin":  {allowedOrigin},
				"Access-Control-Allow-Methods": nil,
			},
		},
		{
			name:       "disallowed origin gets no cors headers",
			origins:    []string{allowedOrigin},
			method:     http.MethodOptions,
			headers:    map[string]string{"Origin": "http://evil.example", "Access-Control-Request-Method": "POST"},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin":  nil,
				"Access-Control-Allow-Methods": nil,
				"Vary":                         {"Origin"},
			},
		},
		{
			name:       "request without origin passes through",
			origins:    []string{allowedOrigin},
			method:     http.MethodPost,
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin": nil,
				"Vary":                        {"Origin"},
			},
		},
		{
			name:       "wildcard allows any origin",
			origins:    []string{anyOrigin},
			method:     http.MethodPost,
			headers:    map[string]string{"Origin": "http://anything.example"},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin": {anyOrigin},
			},
		},
		{
			name:       "origin match is exact",
			origins:    []string{allowedOrigin},
			method:     http.MethodPost,
			headers:    map[string]string{"Origin": "http://localhost:3001"},
			wantStatus: http.StatusOK,
			wantNext:   true,
			wantHeaders: http.Header{
				"Access-Control-Allow-Origin": nil,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(tt.method, "/v1/chat/completions", nil)
			for name, value := range tt.headers {
				req.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			New(tt.origins, next).ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if nextCalled != tt.wantNext {
				t.Errorf("next called = %v, want %v", nextCalled, tt.wantNext)
			}
			for name, want := range tt.wantHeaders {
				if values := recorder.Header().Values(name); !slices.Equal(values, want) {
					t.Errorf("%s = %q, want %q", name, values, want)
				}
			}
		})
	}
}
