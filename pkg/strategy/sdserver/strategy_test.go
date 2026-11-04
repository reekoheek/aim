package sdserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/reekoheek/aim/pkg/strategy"
)

func TestHandleForwardsToEndpointPath(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		contentType     string
		wantContentType string
	}{
		{name: "generations as JSON", path: strategy.ImageGenerationsPath, wantContentType: strategy.JSONContentType},
		{name: "edits keep multipart content type", path: strategy.ImageEditsPath, contentType: "multipart/form-data; boundary=b1", wantContentType: "multipart/form-data; boundary=b1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeSDConfig(t, "m")
			supervisor := newTestSupervisor(t, cfg)
			s := NewStrategy(cfg, testLogger, supervisor, "m", tt.path)
			req := &strategy.Request{Body: []byte(`{"model":"m"}`), ContentType: tt.contentType, Headers: http.Header{}}

			recorder := httptest.NewRecorder()
			s.Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, body %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Body.String(); got != fakeImagesResponse {
				t.Errorf("body %s, want %s", got, fakeImagesResponse)
			}
			if got := recorder.Header().Get(echoContentTypeHeader); got != tt.wantContentType {
				t.Errorf("upstream content type %q, want %q", got, tt.wantContentType)
			}
		})
	}
}

func TestHandleWritesStartFailureWithRequestErrorWriter(t *testing.T) {
	tests := []struct {
		name       string
		errors     strategy.ErrorWriter
		wantStatus int
	}{
		{name: "OpenAI errors", errors: strategy.OpenAIErrors{}, wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeSDConfig(t, "m")
			cfg.Models["m"].SDCpp.Bin = filepath.Join(t.TempDir(), "missing-sd-server")
			supervisor := newTestSupervisor(t, cfg)
			s := NewStrategy(cfg, testLogger, supervisor, "m", strategy.ImageGenerationsPath)

			recorder := httptest.NewRecorder()
			s.Handle(context.Background(), recorder, &strategy.Request{Body: []byte(`{}`), Errors: tt.errors})

			if recorder.Code != tt.wantStatus {
				t.Errorf("status %d, want %d", recorder.Code, tt.wantStatus)
			}
			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Type != strategy.ErrorTypeServer {
				t.Errorf("body %s, want %s error", recorder.Body.String(), strategy.ErrorTypeServer)
			}
		})
	}
}
