package llamaserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/strategy"
)

func handleSystemOneAsync(m *Monitor, cfg *config.Config, model string, session string, delay time.Duration) <-chan int {
	s := NewSystemOneStrategy(cfg, testLogger, m, model)
	req := &strategy.Request{
		Body:    []byte(fmt.Sprintf(`{"model":%q,"fake_delay":%q}`, model, delay)),
		Headers: http.Header{"X-Session-Affinity": {session}},
	}
	statuses := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		s.Handle(context.Background(), recorder, req)
		statuses <- recorder.Code
	}()
	return statuses
}

func TestSystemOneHandleForwardsToSystemOne(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"successful response", http.StatusOK},
		{"rejected request", http.StatusBadRequest},
		{"model that is not a decision model", http.StatusNotImplemented},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			req := &strategy.Request{Body: []byte(fmt.Sprintf(`{"model":"a","fake_status":%d}`, tt.status))}

			recorder := httptest.NewRecorder()
			NewSystemOneStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != tt.status {
				t.Errorf("status %d, want %d", recorder.Code, tt.status)
			}
			if got := recorder.Body.String(); got != fakeSystemOneResponse {
				t.Errorf("response %s, want %s", got, fakeSystemOneResponse)
			}
		})
	}
}

func TestSystemOneHandlePassesBodyThroughWithoutSlot(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
	}{
		{name: "with session", headers: http.Header{"X-Session-Affinity": {"session"}}},
		{name: "without session", headers: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			body := `{"model":"a","fake_echo":true,"state":"hi","questions":{"q":{"type":"noul","instructions":"ok?"}},"seed":9007199254740993}`
			req := &strategy.Request{Body: []byte(body), Headers: tt.headers}

			recorder := httptest.NewRecorder()
			NewSystemOneStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := recorder.Body.String(); got != body {
				t.Errorf("upstream body %s, want %s", got, body)
			}
			for _, slot := range currentSlots(m).slots {
				if slot.session != "" || slot.busy || slot.unsaved {
					t.Errorf("slot %d session %q busy %v unsaved %v, want untouched", slot.id, slot.session, slot.busy, slot.unsaved)
				}
			}
		})
	}
}

func TestSystemOneHandleIsNotLimitedBySlots(t *testing.T) {
	const slowRequest = 1500 * time.Millisecond
	tests := []struct {
		name        string
		slowSession string
		fastSession string
	}{
		{name: "same session", slowSession: "session", fastSession: "session"},
		{name: "different sessions", slowSession: "slow", fastSession: "fast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			if status := receiveWithin(t, handleSystemOneAsync(m, cfg, "a", tt.slowSession, 0), handleTimeout, "server start"); status != http.StatusOK {
				t.Fatalf("server start status %d, want %d", status, http.StatusOK)
			}

			slow := handleSystemOneAsync(m, cfg, "a", tt.slowSession, slowRequest)
			waitUntil(t, handleTimeout, "slow request in flight", func() bool {
				srv := currentServer(m)
				srv.idleMu.Lock()
				defer srv.idleMu.Unlock()
				return srv.active == 1
			})
			start := time.Now()
			fast := handleSystemOneAsync(m, cfg, "a", tt.fastSession, 0)

			if status := receiveWithin(t, fast, handleTimeout, "fast request"); status != http.StatusOK {
				t.Errorf("fast request status %d, want %d", status, http.StatusOK)
			}
			if waited := time.Since(start); waited >= slowRequest/2 {
				t.Errorf("fast request waited %v for the slow one, want served alongside", waited)
			}
			if status := receiveWithin(t, slow, handleTimeout, "slow request"); status != http.StatusOK {
				t.Errorf("slow request status %d, want %d", status, http.StatusOK)
			}
		})
	}
}

func TestSystemOneHandleSharesServerWithChat(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "chat", "decider")
	m := newTestMonitor(t)

	steps := []struct {
		name  string
		serve func() <-chan int
		want  string
	}{
		{name: "chat request", serve: func() <-chan int { return handleAsync(m, cfg, "chat", "session", 0) }, want: "chat"},
		{name: "systemone request swaps the model", serve: func() <-chan int { return handleSystemOneAsync(m, cfg, "decider", "session", 0) }, want: "decider"},
		{name: "chat request swaps back", serve: func() <-chan int { return handleAsync(m, cfg, "chat", "session", 0) }, want: "chat"},
	}

	for _, step := range steps {
		if status := receiveWithin(t, step.serve(), handleTimeout, step.name); status != http.StatusOK {
			t.Fatalf("%s: status %d, want %d", step.name, status, http.StatusOK)
		}
		if got := currentServer(m).Selector(); got != step.want {
			t.Errorf("%s: current model %q, want %q", step.name, got, step.want)
		}
	}
}

func TestSystemOneHandleWritesErrorsAsJSON(t *testing.T) {
	tests := []struct {
		name              string
		selector          string
		wantStatus        int
		wantMessagePrefix string
	}{
		{
			name:              "model missing from config",
			selector:          "missing",
			wantStatus:        http.StatusInternalServerError,
			wantMessagePrefix: `model "missing" not found in config`,
		},
		{
			name:              "llama server fails to start",
			selector:          "broken",
			wantStatus:        http.StatusServiceUnavailable,
			wantMessagePrefix: errStartFailed.Error() + ": ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "broken")
			cfg.Models["broken"].LlamaCpp.Bin = filepath.Join(t.TempDir(), "missing-llama-server")
			m := newTestMonitor(t)
			req := &strategy.Request{Body: []byte(fmt.Sprintf(`{"model":%q}`, tt.selector))}

			recorder := httptest.NewRecorder()
			NewSystemOneStrategy(cfg, testLogger, m, tt.selector).Handle(context.Background(), recorder, req)

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
			if !strings.HasPrefix(got.Error.Message, tt.wantMessagePrefix) || got.Error.Type != strategy.ErrorTypeServer {
				t.Errorf("error %q of type %q, want prefix %q of type %q", got.Error.Message, got.Error.Type, tt.wantMessagePrefix, strategy.ErrorTypeServer)
			}
		})
	}
}

func TestCountTokensHandleForwardsWithoutSlot(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
	}{
		{name: "claude code session", headers: http.Header{"X-Claude-Code-Session-Id": {"cc"}}},
		{name: "without session", headers: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			req := &strategy.Request{Body: []byte(`{"model":"a","messages":[]}`), Headers: tt.headers}

			recorder := httptest.NewRecorder()
			NewCountTokensStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := recorder.Body.String(); got != fakeCountTokensResponse {
				t.Errorf("response %s, want %s", got, fakeCountTokensResponse)
			}
			for _, slot := range currentSlots(m).slots {
				if slot.session != "" || slot.busy || slot.unsaved {
					t.Errorf("slot %d session %q busy %v unsaved %v, want untouched", slot.id, slot.session, slot.busy, slot.unsaved)
				}
			}
		})
	}
}

func TestCountTokensHandleWritesAnthropicErrors(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "broken")
	cfg.Models["broken"].LlamaCpp.Bin = filepath.Join(t.TempDir(), "missing-llama-server")
	m := newTestMonitor(t)
	req := &strategy.Request{Body: []byte(`{"model":"broken"}`), Errors: strategy.AnthropicErrors{}}

	recorder := httptest.NewRecorder()
	NewCountTokensStrategy(cfg, testLogger, m, "broken").Handle(context.Background(), recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s is not JSON: %v", recorder.Body, err)
	}
	if got.Type != "error" || got.Error.Type != "api_error" || !strings.HasPrefix(got.Error.Message, errStartFailed.Error()+": ") {
		t.Errorf("error %+v, want an api_error starting with %q", got, errStartFailed.Error())
	}
}
