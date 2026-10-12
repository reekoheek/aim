package llamacpp

import (
	"slices"
	"testing"
	"time"

	"github.com/reekoheek/aim/pkg/config"
)

func TestFlagArgs(t *testing.T) {
	tests := []struct {
		name  string
		flags map[string]any
		want  []string
	}{
		{
			name:  "no flags",
			flags: nil,
			want:  nil,
		},
		{
			name:  "short flag with int",
			flags: map[string]any{"-ngl": 999},
			want:  []string{"-ngl", "999"},
		},
		{
			name:  "long flag with float",
			flags: map[string]any{"--temp": 0.6},
			want:  []string{"--temp", "0.6"},
		},
		{
			name:  "zero values are still passed",
			flags: map[string]any{"--cache-ram": 0, "--min-p": 0.0},
			want:  []string{"--cache-ram", "0", "--min-p", "0"},
		},
		{
			name:  "negative value",
			flags: map[string]any{"--top-k": -1},
			want:  []string{"--top-k", "-1"},
		},
		{
			name:  "on stays a value",
			flags: map[string]any{"-fa": "on"},
			want:  []string{"-fa", "on"},
		},
		{
			name:  "value with spaces stays one argument",
			flags: map[string]any{"--reasoning-budget-message": "... budget exceeded. Answer now."},
			want:  []string{"--reasoning-budget-message", "... budget exceeded. Answer now."},
		},
		{
			name:  "true becomes bare flag",
			flags: map[string]any{"--jinja": true},
			want:  []string{"--jinja"},
		},
		{
			name:  "false is left out",
			flags: map[string]any{"--offline": false},
			want:  nil,
		},
		{
			name: "sorted by flag name",
			flags: map[string]any{
				"--temp":  0.6,
				"--jinja": true,
				"-hf":     "org/repo",
				"-c":      4096,
			},
			want: []string{"--jinja", "--temp", "0.6", "-c", "4096", "-hf", "org/repo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flagArgs(tt.flags)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServerArgs(t *testing.T) {
	tests := []struct {
		name  string
		entry *config.LlamaCppEntry
		want  []string
	}{
		{
			name:  "managed flags only",
			entry: &config.LlamaCppEntry{Port: 12345},
			want:  []string{"--host", "127.0.0.1", "--port", "12345", "--log-colors", "off"},
		},
		{
			name: "managed flags come after model flags",
			entry: &config.LlamaCppEntry{
				Port:  23456,
				Flags: map[string]any{"-ngl": 999},
			},
			want: []string{
				"-ngl", "999",
				"--host", "127.0.0.1",
				"--port", "23456",
				"--log-colors", "off",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", tt.entry, "", nil).args()
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewServerStopTimeout(t *testing.T) {
	tests := []struct {
		name        string
		stopTimeout time.Duration
		want        time.Duration
	}{
		{name: "default", stopTimeout: 0, want: defaultStopTimeout},
		{name: "configured", stopTimeout: 30 * time.Second, want: 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewServer("m", &config.LlamaCppEntry{StopTimeout: tt.stopTimeout}, "", nil).stopTimeout
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
