package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".aim.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLlamaCppFlagsOverlay(t *testing.T) {
	tests := []struct {
		name string
		base LlamaCppFlags
		top  LlamaCppFlags
		want LlamaCppFlags
	}{
		{
			name: "empty top keeps base",
			base: LlamaCppFlags{Args: map[string]any{"-ngl": 999}},
			top:  LlamaCppFlags{},
			want: LlamaCppFlags{Args: map[string]any{"-ngl": 999}},
		},
		{
			name: "top wins and adds",
			base: LlamaCppFlags{Port: 1, Args: map[string]any{"--temp": 0.6, "-c": 128000}},
			top:  LlamaCppFlags{Port: 2, Inherit: "default", Args: map[string]any{"--temp": 0.4, "-hf": "org/repo"}},
			want: LlamaCppFlags{Port: 2, Inherit: "default", Args: map[string]any{"--temp": 0.4, "-c": 128000, "-hf": "org/repo"}},
		},
		{
			name: "false in top overrides true in base",
			base: LlamaCppFlags{Args: map[string]any{"--offline": true}},
			top:  LlamaCppFlags{Args: map[string]any{"--offline": false}},
			want: LlamaCppFlags{Args: map[string]any{"--offline": false}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.base.overlay(tt.top)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name: "empty file",
			yaml: "",
		},
		{
			name: "chat-completion config",
			yaml: `
models:
  remote/m:
    type: chat-completion
    provider: p
    model: m
providers:
  p:
    base_url: http://p:8080
    api_key: sk-x
server:
  host: 127.0.0.1
  port: 8081
`,
		},
		{
			name: "slot_expiration not a duration",
			yaml: `
server:
  slot_expiration: 1d
`,
			wantErr: true,
		},
		{
			name: "unknown top-level key",
			yaml: `
unknown: 1
`,
			wantErr: true,
		},
		{
			name: "aim setting with wrong type",
			yaml: `
templates:
  default:
    port: lots
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Errorf("got error %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadLlamaServerEntry(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		selector string
		want     *LlamaCppEntry
		wantErr  bool
	}{
		{
			name: "template flags merged with model flags winning",
			yaml: `
templates:
  default:
    -ngl: 999
    -fa: on
    --jinja: true
    --offline: true
    --temp: 0.6
models:
  m:
    type: llama-server
    flags:
      inherit: default
      -hf: org/repo:Q4_K_M
      --offline: false
      --temp: 0.4
      --top-k: -1
`,
			selector: "m",
			want: &LlamaCppEntry{
				Bin:         "llama-server",
				Port:        portFromSelector("m"),
				StopTimeout: 30 * time.Second,
				Flags: map[string]any{
					"-ngl":      999,
					"-fa":       "on",
					"--jinja":   true,
					"--offline": false,
					"--temp":    0.4,
					"-hf":       "org/repo:Q4_K_M",
					"--top-k":   -1,
				},
			},
		},
		{
			name: "model override does not leak into template shared by another model",
			yaml: `
templates:
  default:
    --temp: 0.6
models:
  a:
    type: llama-server
    flags:
      inherit: default
      --temp: 0.1
  b:
    type: llama-server
    flags:
      inherit: default
`,
			selector: "b",
			want: &LlamaCppEntry{
				Bin:         "llama-server",
				Port:        portFromSelector("b"),
				StopTimeout: 30 * time.Second,
				Flags:       map[string]any{"--temp": 0.6},
			},
		},
		{
			name: "stop_timeout is read by aim, not passed as flags",
			yaml: `
templates:
  default:
    --temp: 0.6
models:
  m:
    type: llama-server
    stop_timeout: 30
    flags:
      inherit: default
      port: 12345
      url: http://127.0.0.1:12345
      -c: 4096
`,
			selector: "m",
			want: &LlamaCppEntry{
				Bin:         "llama-server",
				URL:         "http://127.0.0.1:12345",
				Port:        12345,
				StopTimeout: 30 * time.Second,
				Flags:       map[string]any{"--temp": 0.6, "-c": 4096},
			},
		},
		{
			name: "model url wins over flags url",
			yaml: `
models:
  m:
    type: llama-server
    url: http://model-level:1
    flags:
      url: http://flag-level:2
`,
			selector: "m",
			want: &LlamaCppEntry{
				Bin:         "llama-server",
				URL:         "http://model-level:1",
				Port:        portFromSelector("m"),
				StopTimeout: 30 * time.Second,
			},
		},
		{
			name: "model without flags still gets an entry",
			yaml: `
models:
  m:
    type: llama-server
    bin: /opt/llama/llama-server
`,
			selector: "m",
			want: &LlamaCppEntry{
				Bin:         "/opt/llama/llama-server",
				Port:        portFromSelector("m"),
				StopTimeout: 30 * time.Second,
			},
		},
		{
			name: "unknown template",
			yaml: `
models:
  m:
    type: llama-server
    flags:
      inherit: missing
`,
			selector: "m",
			wantErr:  true,
		},
		{
			name: "flag without dash",
			yaml: `
models:
  m:
    type: llama-server
    flags:
      temp: 0.6
`,
			selector: "m",
			wantErr:  true,
		},
		{
			name: "flag without dash in inherited template",
			yaml: `
templates:
  default:
    ngl: 999
models:
  m:
    type: llama-server
    flags:
      inherit: default
`,
			selector: "m",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := cfg.GetLlamaCppEntry(tt.selector)
			if err != nil {
				t.Fatalf("GetLlamaCppEntry: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDefaultServerSlotExpiration(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{
			name: "default",
			yaml: "",
			want: time.Hour,
		},
		{
			name: "hours",
			yaml: `
server:
  slot_expiration: 48h
`,
			want: 48 * time.Hour,
		},
		{
			name: "minutes",
			yaml: `
server:
  slot_expiration: 90m
`,
			want: 90 * time.Minute,
		},
		{
			name: "zero falls back to default",
			yaml: `
server:
  slot_expiration: 0s
`,
			want: time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.DefaultServer().SlotExpiration; got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultServerSlotMaxFiles(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{
			name: "default",
			yaml: "",
			want: 3,
		},
		{
			name: "configured",
			yaml: `
server:
  slot_max_files: 5
`,
			want: 5,
		},
		{
			name: "zero falls back to default",
			yaml: `
server:
  slot_max_files: 0
`,
			want: 3,
		},
		{
			name: "negative falls back to default",
			yaml: `
server:
  slot_max_files: -1
`,
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.DefaultServer().SlotMaxFiles; got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDefaultServerInferenceDelay(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{
			name: "default",
			yaml: "",
			want: 1 * time.Second,
		},
		{
			name: "configured",
			yaml: `
server:
  inference_delay: 5s
`,
			want: 5 * time.Second,
		},
		{
			name: "zero falls back to default",
			yaml: `
server:
  inference_delay: 0s
`,
			want: 1 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.DefaultServer().InferenceDelay; got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultServerIdleTimeout(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{
			name: "default",
			yaml: "",
			want: 300,
		},
		{
			name: "configured",
			yaml: `
server:
  idle_timeout: 120
`,
			want: 120,
		},
		{
			name: "zero disables",
			yaml: `
server:
  idle_timeout: 0
`,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.DefaultServer().IdleTimeout
			if got == nil {
				t.Fatal("IdleTimeout is nil")
			}
			if *got != tt.want {
				t.Errorf("got %d, want %d", *got, tt.want)
			}
		})
	}
}

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		yaml     string
		want     ModelEntry
		wantErr  string
	}{
		{
			name:     "chat-completion",
			selector: "m",
			yaml: `
models:
  m:
    type: chat-completion
    provider: p
    model: remote-m
providers:
  p:
    base_url: http://example.com
`,
			want: ModelEntry{Type: ModelTypeChatCompletion, Provider: "p", Model: "remote-m"},
		},
		{
			name:     "llama-server",
			selector: "m",
			yaml: `
models:
  m:
    type: llama-server
`,
			want: ModelEntry{Type: ModelTypeLlamaServer},
		},
		{
			name:     "model missing from config",
			selector: "missing",
			yaml:     "",
			wantErr:  `model "missing" not found in config`,
		},
		{
			name:     "chat-completion without provider",
			selector: "m",
			yaml: `
models:
  m:
    type: chat-completion
    model: remote-m
`,
			wantErr: `model "m" has empty provider`,
		},
		{
			name:     "chat-completion without model name",
			selector: "m",
			yaml: `
models:
  m:
    type: chat-completion
    provider: p
providers:
  p:
    base_url: http://example.com
`,
			wantErr: `model "m" has empty model name`,
		},
		{
			name:     "chat-completion with provider missing from config",
			selector: "m",
			yaml: `
models:
  m:
    type: chat-completion
    provider: gone
    model: remote-m
`,
			wantErr: `provider "gone" not found in config`,
		},
		{
			name:     "legacy provider type rejected",
			selector: "m",
			yaml: `
models:
  m:
    type: provider
    provider: p
    model: remote-m
providers:
  p:
    base_url: http://example.com
`,
			wantErr: `unsupported type "provider"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := cfg.ResolveModel(tt.selector)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got err %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveModel: %v", err)
			}
			if got.Type != tt.want.Type || got.Provider != tt.want.Provider || got.Model != tt.want.Model {
				t.Errorf("got type %q provider %q model %q, want type %q provider %q model %q",
					got.Type, got.Provider, got.Model, tt.want.Type, tt.want.Provider, tt.want.Model)
			}
		})
	}
}

func TestGetModelURLLlamaServer(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    string
		wantErr string
	}{
		{
			name: "derived from port",
			yaml: `
models:
  m:
    type: llama-server
    flags:
      port: 23456
`,
			want: "http://127.0.0.1:23456",
		},
		{
			name: "explicit url",
			yaml: `
models:
  m:
    type: llama-server
    url: http://other-host:8080
`,
			want: "http://other-host:8080",
		},
		{
			name: "chat-completion model has no llama-server URL",
			yaml: `
models:
  m:
    type: chat-completion
    provider: p
    model: remote-m
`,
			wantErr: `model "m" is not type llama-server`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := cfg.GetModelURL("m")
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("got err %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetModelURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
