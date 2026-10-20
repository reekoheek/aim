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

const handleTimeout = 15 * time.Second

func handleAsync(m *Monitor, cfg *config.Config, model string, session string, delay time.Duration) <-chan int {
	s := NewStrategy(cfg, testLogger, m, model)
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

func TestHandleSurvivesIdleTimeoutDuringRequest(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 1, "a")
	m := newTestMonitor(t)

	status := receiveWithin(t, handleAsync(m, cfg, "a", "session", 1500*time.Millisecond), handleTimeout, "request outlasting the idle timeout")
	if status != http.StatusOK {
		t.Fatalf("status %d, want %d", status, http.StatusOK)
	}

	srv := currentServer(m)
	waitUntil(t, 5*time.Second, "idle stop after the request", srv.Exited)
}

func TestHandleSwitchesModelWhileRequestInFlight(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a", "b")
	m := newTestMonitor(t)

	first := handleAsync(m, cfg, "a", "session", 800*time.Millisecond)
	waitUntil(t, handleTimeout, "first request holding the slot", func() bool {
		slots := currentSlots(m)
		return slots != nil && busySlots(slots) == 1
	})
	second := handleAsync(m, cfg, "b", "session", 0)

	for _, tt := range []struct {
		name     string
		statuses <-chan int
	}{
		{"request on the old model", first},
		{"request switching the model", second},
	} {
		if status := receiveWithin(t, tt.statuses, handleTimeout, tt.name); status != http.StatusOK {
			t.Errorf("%s: status %d, want %d", tt.name, status, http.StatusOK)
		}
	}
	if got := currentServer(m).Selector(); got != "b" {
		t.Errorf("current model %q, want %q", got, "b")
	}
}

func TestHandleServesSessionsInParallelUpToSlotCount(t *testing.T) {
	const slowRequest = 1500 * time.Millisecond
	tests := []struct {
		name         string
		slots        int
		wantParallel bool
	}{
		{name: "one slot queues the second session", slots: 1, wantParallel: false},
		{name: "two slots serve both sessions at once", slots: 2, wantParallel: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			cfg.Models["a"].LlamaCpp.Slots = tt.slots
			m := newTestMonitor(t)

			slow := handleAsync(m, cfg, "a", "slow", slowRequest)
			waitUntil(t, handleTimeout, "slow request holding a slot", func() bool {
				slots := currentSlots(m)
				return slots != nil && busySlots(slots) == 1
			})
			start := time.Now()
			fast := handleAsync(m, cfg, "a", "fast", 0)

			if status := receiveWithin(t, fast, handleTimeout, "fast request"); status != http.StatusOK {
				t.Errorf("fast request status %d, want %d", status, http.StatusOK)
			}
			if parallel := time.Since(start) < slowRequest/2; parallel != tt.wantParallel {
				t.Errorf("fast request served alongside the slow one %v, want %v", parallel, tt.wantParallel)
			}
			if status := receiveWithin(t, slow, handleTimeout, "slow request"); status != http.StatusOK {
				t.Errorf("slow request status %d, want %d", status, http.StatusOK)
			}
		})
	}
}

func TestHandleKeepsSlotForSessionWhateverTheUpstreamStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"successful response", http.StatusOK},
		{"rejected request", http.StatusBadRequest},
		{"upstream failure", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			req := &strategy.Request{
				Body:    []byte(fmt.Sprintf(`{"model":"a","fake_status":%d}`, tt.status)),
				Headers: http.Header{"X-Session-Affinity": {"session"}},
			}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != tt.status {
				t.Fatalf("status %d, want %d", recorder.Code, tt.status)
			}
			slot := currentSlots(m).slots[0]
			if slot.session != "session" || !slot.unsaved {
				t.Errorf("slot session %q unsaved %v, want %q unsaved true", slot.session, slot.unsaved, "session")
			}
		})
	}
}

func TestHandleRejectsBodyThatIsNotJSONObject(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"model":"a"`},
		{"plain text", `hello`},
		{"JSON array", `[{"model":"a"}]`},
		{"JSON string", `"a"`},
		{"JSON null", `null`},
		{"empty body", ``},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			req := &strategy.Request{
				Body:    []byte(tt.body),
				Headers: http.Header{"X-Session-Affinity": {"session"}},
			}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if currentServer(m) != nil {
				t.Error("started a llama server for a rejected body")
			}
		})
	}
}

func TestHandleWritesErrorsAsJSON(t *testing.T) {
	tests := []struct {
		name              string
		selector          string
		body              string
		wantStatus        int
		wantType          string
		wantMessagePrefix string
	}{
		{
			name:              "body that is not a JSON object",
			selector:          "a",
			body:              `hello`,
			wantStatus:        http.StatusBadRequest,
			wantType:          strategy.ErrorTypeInvalidRequest,
			wantMessagePrefix: strategy.ErrBodyNotJSONObject.Error(),
		},
		{
			name:              "model missing from config",
			selector:          "missing",
			body:              `{"model":"missing"}`,
			wantStatus:        http.StatusInternalServerError,
			wantType:          strategy.ErrorTypeServer,
			wantMessagePrefix: `model "missing" not found in config`,
		},
		{
			name:              "llama server fails to start",
			selector:          "broken",
			body:              `{"model":"broken"}`,
			wantStatus:        http.StatusServiceUnavailable,
			wantType:          strategy.ErrorTypeServer,
			wantMessagePrefix: errStartFailed.Error() + ": ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a", "broken")
			cfg.Models["broken"].LlamaCpp.Bin = filepath.Join(t.TempDir(), "missing-llama-server")
			m := newTestMonitor(t)
			req := &strategy.Request{Body: []byte(tt.body)}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, testLogger, m, tt.selector).Handle(context.Background(), recorder, req)

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
			if !strings.HasPrefix(got.Error.Message, tt.wantMessagePrefix) || got.Error.Type != tt.wantType {
				t.Errorf("error %q of type %q, want prefix %q of type %q", got.Error.Message, got.Error.Type, tt.wantMessagePrefix, tt.wantType)
			}
		})
	}
}

func TestHandlePassesBodyValuesThroughUnchanged(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	m := newTestMonitor(t)

	tests := []struct {
		name  string
		field string
	}{
		{name: "integer beyond float64 precision", field: `"seed":9007199254740993`},
		{name: "decimal with trailing zero", field: `"temperature":0.10`},
		{name: "number in exponent form", field: `"top_p":1e-1`},
		{name: "nested object", field: `"response_format":{"type":"json_object","max":18446744073709551615}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &strategy.Request{
				Body: []byte(`{"model":"a","fake_echo":true,` + tt.field + `}`),
			}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := recorder.Body.String(); !strings.Contains(got, tt.field) {
				t.Errorf("upstream body %s, want containing %s", got, tt.field)
			}
		})
	}
}

func TestHandleUsesTrimmedSessionForSlot(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	m := newTestMonitor(t)

	tests := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{name: "session with surrounding whitespace", headers: http.Header{"X-Session-Affinity": {" session "}}, want: "session"},
		{name: "blank session", headers: http.Header{"X-Session-Affinity": {"   "}}, want: "-"},
		{name: "missing session", headers: nil, want: "-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &strategy.Request{Body: []byte(`{"model":"a"}`), Headers: tt.headers}

			recorder := httptest.NewRecorder()
			NewStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := currentSlots(m).slots[0].session; got != tt.want {
				t.Errorf("slot session %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleForwardsToEndpointPath(t *testing.T) {
	tests := []struct {
		name         string
		newStrategy  func(cfg *config.Config, m *Monitor) *Strategy
		wantResponse string
	}{
		{
			name: "chat completions",
			newStrategy: func(cfg *config.Config, m *Monitor) *Strategy {
				return NewStrategy(cfg, testLogger, m, "a")
			},
			wantResponse: fakeChatCompletionResponse,
		},
		{
			name: "messages",
			newStrategy: func(cfg *config.Config, m *Monitor) *Strategy {
				return NewMessagesStrategy(cfg, testLogger, m, "a")
			},
			wantResponse: fakeMessagesResponse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			req := &strategy.Request{
				Body:    []byte(`{"model":"a"}`),
				Headers: http.Header{"X-Claude-Code-Session-Id": {"cc"}},
			}

			recorder := httptest.NewRecorder()
			tt.newStrategy(cfg, m).Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
			}
			if got := recorder.Body.String(); got != tt.wantResponse {
				t.Errorf("response %s, want %s", got, tt.wantResponse)
			}
			if got := currentSlots(m).slots[0].session; got != "cc" {
				t.Errorf("slot session %q, want %q", got, "cc")
			}
		})
	}
}

func TestMessagesHandlePinsSlot(t *testing.T) {
	cfg := newFakeLlamaConfig(t, 0, "a")
	m := newTestMonitor(t)
	req := &strategy.Request{
		Body:    []byte(`{"model":"a","fake_echo":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
		Headers: http.Header{"X-Session-Affinity": {"session"}},
	}

	recorder := httptest.NewRecorder()
	NewMessagesStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body)
	}
	var upstream map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &upstream); err != nil {
		t.Fatalf("upstream body %s is not JSON: %v", recorder.Body, err)
	}
	for field, want := range map[string]string{
		"id_slot":      `0`,
		"cache_prompt": `true`,
		"max_tokens":   `16`,
		"messages":     `[{"role":"user","content":"hi"}]`,
	} {
		if got := string(upstream[field]); got != want {
			t.Errorf("upstream %s = %s, want %s", field, got, want)
		}
	}
}

func TestHandleWritesErrorsWithRequestErrorWriter(t *testing.T) {
	tests := []struct {
		name     string
		errors   strategy.ErrorWriter
		wantBody string
	}{
		{
			name:     "OpenAI shape",
			errors:   strategy.OpenAIErrors{},
			wantBody: `{"error":{"message":"request body must be a JSON object","type":"invalid_request_error","param":null,"code":null}}`,
		},
		{
			name:     "Anthropic shape",
			errors:   strategy.AnthropicErrors{},
			wantBody: `{"type":"error","error":{"type":"invalid_request_error","message":"request body must be a JSON object"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newFakeLlamaConfig(t, 0, "a")
			m := newTestMonitor(t)
			req := &strategy.Request{Body: []byte(`hello`), Errors: tt.errors}

			recorder := httptest.NewRecorder()
			NewMessagesStrategy(cfg, testLogger, m, "a").Handle(context.Background(), recorder, req)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Errorf("body %s, want %s", got, tt.wantBody)
			}
		})
	}
}
