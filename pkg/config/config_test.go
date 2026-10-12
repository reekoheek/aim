package config

import (
	"os"
	"path/filepath"
	"reflect"
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
			base: LlamaCppFlags{StopTimeout: 30, Args: map[string]any{"-ngl": 999}},
			top:  LlamaCppFlags{},
			want: LlamaCppFlags{StopTimeout: 30, Args: map[string]any{"-ngl": 999}},
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
			name: "provider config",
			yaml: `
models:
  remote/m:
    type: provider
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
				Bin:  "llama-server",
				Port: portFromSelector("m"),
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
				Bin:   "llama-server",
				Port:  portFromSelector("b"),
				Flags: map[string]any{"--temp": 0.6},
			},
		},
		{
			name: "port, url and stop-timeout are read by aim, not passed as flags",
			yaml: `
templates:
  default:
    stop-timeout: 30
models:
  m:
    type: llama-server
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
				Flags:       map[string]any{"-c": 4096},
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
				Bin:  "llama-server",
				URL:  "http://model-level:1",
				Port: portFromSelector("m"),
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
				Bin:  "/opt/llama/llama-server",
				Port: portFromSelector("m"),
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

func TestGetModelURLLlamaServer(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tt.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := cfg.GetModelURL("m")
			if err != nil {
				t.Fatalf("GetModelURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
