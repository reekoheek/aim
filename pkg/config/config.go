package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LlamaCppFlags holds llama.cpp server flags.
type LlamaCppFlags struct {
	NGL             int     `yaml:"ngl"`
	FA              string  `yaml:"fa"`
	NP              int     `yaml:"np"`
	C               int     `yaml:"c"`
	CTK             string  `yaml:"ctk"`
	CTV             string  `yaml:"ctv"`
	HF              string  `yaml:"hf"`
	ModelPath       string  `yaml:"model"`
	Port            int     `yaml:"port"`
	Temp            float64 `yaml:"temp"`
	TopP            float64 `yaml:"top-p"`
	TopK            int     `yaml:"top-k"`
	MinP            float64 `yaml:"min-p"`
	PresencePenalty float64 `yaml:"presence-penalty"`
	RepeatPenalty   float64 `yaml:"repeat-penalty"`
}

// LlamaCppEntry represents a local llama.cpp model configuration.
type LlamaCppEntry struct {
	Bin   string        `yaml:"bin"`
	URL   string        `yaml:"url"`      // explicit URL, overrides auto-generated
	Flags LlamaCppFlags `yaml:"-"`        // populated during load (merged)
}

// ModelEntry represents a model configuration.
type ModelEntry struct {
	Type       string          `yaml:"type"`
	Provider   string          `yaml:"provider"`
	Model      string          `yaml:"model"`
	LlamaCpp   *LlamaCppEntry  `yaml:"-"` // populated during load
	RawFlags   map[string]any  `yaml:"flags"`
	Bin        string          `yaml:"bin"` // llama.cpp binary name
	URL        string          `yaml:"url"` // explicit URL override
}

// ProviderEntry represents an LLM provider configuration.
type ProviderEntry struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

// ServerConfig holds the server listening configuration.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// Config is the full application configuration.
type Config struct {
	Models    map[string]ModelEntry    `yaml:"models"`
	Providers map[string]ProviderEntry `yaml:"providers"`
	Templates map[string]LlamaCppFlags `yaml:"templates"`
	Server    ServerConfig             `yaml:"server"`
}

// ResolveModel resolves a model selector to its type and info.
// Returns: type ("provider" or "llama.cpp"), additional info, error.
func (c *Config) ResolveModel(selector string) (modelType string, info map[string]any, err error) {
	entry, ok := c.Models[selector]
	if !ok {
		return "", nil, fmt.Errorf("model %q not found in config", selector)
	}
	switch entry.Type {
	case "provider":
		if entry.Provider == "" {
			return "", nil, fmt.Errorf("model %q has empty provider", selector)
		}
		if entry.Model == "" {
			return "", nil, fmt.Errorf("model %q has empty model name", selector)
		}
		if _, ok := c.Providers[entry.Provider]; !ok {
			return "", nil, fmt.Errorf("provider %q not found in config", entry.Provider)
		}
		return "provider", map[string]any{
			"provider": entry.Provider,
			"model":    entry.Model,
		}, nil
	case "llama-server":
		info := map[string]any{
			"bin":      entry.LlamaCpp.Bin,
			"flags":    entry.LlamaCpp.Flags,
			"selector": selector,
		}
		// Resolve URL: explicit url > auto-generated from port
		if entry.LlamaCpp.URL != "" {
			info["url"] = entry.LlamaCpp.URL
		} else {
			port := entry.LlamaCpp.Flags.Port
			if port == 0 {
				// Generate port from selector hash (range 10000-65535)
				var h uint32
				for _, b := range selector {
					h = 31*h + uint32(b)
				}
				port = 10000 + int(h%55536)
			}
			info["url"] = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		return "llama-server", info, nil
	default:
		return "", nil, fmt.Errorf("model %q has unsupported type %q", selector, entry.Type)
	}
}

// GetProvider returns the provider configuration for the given provider ID.
func (c *Config) GetProvider(id string) (ProviderEntry, error) {
	p, ok := c.Providers[id]
	if !ok {
		return ProviderEntry{}, fmt.Errorf("provider %q not found in config", id)
	}
	if p.BaseURL == "" {
		return ProviderEntry{}, fmt.Errorf("provider %q has empty base_url", id)
	}
	return p, nil
}

// GetLlamaCppEntry returns the llama.cpp entry for a model.
func (c *Config) GetLlamaCppEntry(selector string) (*LlamaCppEntry, error) {
	entry, ok := c.Models[selector]
	if !ok {
		return nil, fmt.Errorf("model %q not found in config", selector)
	}
	if entry.Type != "llama-server" {
		return nil, fmt.Errorf("model %q is not type llama-server", selector)
	}
	if entry.LlamaCpp == nil {
		return nil, fmt.Errorf("model %q has no llama.cpp config", selector)
	}
	return entry.LlamaCpp, nil
}

// GetModelURL returns the target URL for a model (provider or llama.cpp).
func (c *Config) GetModelURL(selector string) (string, error) {
	_, info, err := c.ResolveModel(selector)
	if err != nil {
		return "", err
	}
	url, ok := info["url"]
	if !ok || url == "" {
		return "", fmt.Errorf("model %q has no URL", selector)
	}
	return url.(string), nil
}

// DefaultServer returns the default server configuration.
func (c *Config) DefaultServer() ServerConfig {
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	return c.Server
}

// Load reads and parses a YAML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var rawCfg map[string]any
	if err := yaml.Unmarshal(data, &rawCfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Parse providers
	var providers map[string]ProviderEntry
	if provRaw, ok := rawCfg["providers"]; ok {
		if provBytes, err := yaml.Marshal(provRaw); err == nil {
			_ = yaml.Unmarshal(provBytes, &providers)
		}
	}

	// Parse models
	var models map[string]ModelEntry
	if modRaw, ok := rawCfg["models"]; ok {
		if modBytes, err := yaml.Marshal(modRaw); err == nil {
			_ = yaml.Unmarshal(modBytes, &models)
		}
	}

	// Parse templates
	var templates map[string]LlamaCppFlags
	if tplRaw, ok := rawCfg["templates"]; ok {
		if tplBytes, err := yaml.Marshal(tplRaw); err == nil {
			_ = yaml.Unmarshal(tplBytes, &templates)
		}
	}

	// Parse server
	var server ServerConfig
	if srvRaw, ok := rawCfg["server"]; ok {
		if srvBytes, err := yaml.Marshal(srvRaw); err == nil {
			_ = yaml.Unmarshal(srvBytes, &server)
		}
	}

	cfg := Config{
		Models:    models,
		Providers: providers,
		Templates: templates,
		Server:    server,
	}

	if cfg.Models == nil {
		cfg.Models = make(map[string]ModelEntry)
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderEntry)
	}
	if cfg.Templates == nil {
		cfg.Templates = make(map[string]LlamaCppFlags)
	}

	// Parse llama.cpp entries from raw flags, merging with template if set
	for id, entry := range cfg.Models {
		if entry.Type == "llama-server" && entry.RawFlags != nil {
			llama := &LlamaCppEntry{Bin: "llama-server"}
			if entry.Bin != "" {
				llama.Bin = entry.Bin
			}
			// Resolve inherit from flags (e.g. inherit: balanced)
			if inheritRaw, ok := entry.RawFlags["inherit"]; ok {
				templateName := toString(inheritRaw)
				tpl, ok := cfg.Templates[templateName]
				if !ok {
					return nil, fmt.Errorf("template %q not found (used by model %q)", templateName, id)
				}
				// Start with template as base
				llama.Flags = tpl
			}
			// Overlay model-level raw flags onto base (model wins for overlapping keys)
			for k, v := range entry.RawFlags {
				switch k {
				case "ngl":
					llama.Flags.NGL = toInt(v)
				case "fa":
					llama.Flags.FA = toString(v)
				case "np":
					llama.Flags.NP = toInt(v)
				case "c":
					llama.Flags.C = toInt(v)
				case "ctk":
					llama.Flags.CTK = toString(v)
				case "ctv":
					llama.Flags.CTV = toString(v)
				case "hf":
					llama.Flags.HF = toString(v)
				case "port":
					llama.Flags.Port = toInt(v)
				case "model":
					llama.Flags.ModelPath = toString(v)
				case "temp":
					llama.Flags.Temp = toFloat(v)
				case "top-p":
					llama.Flags.TopP = toFloat(v)
				case "top-k":
					llama.Flags.TopK = toInt(v)
				case "min-p":
					llama.Flags.MinP = toFloat(v)
				case "presence-penalty":
					llama.Flags.PresencePenalty = toFloat(v)
				case "repeat-penalty":
					llama.Flags.RepeatPenalty = toFloat(v)
				}
			}
			// Parse explicit URL if present
			if entry.URL != "" {
				llama.URL = entry.URL
			} else if urlRaw, ok := entry.RawFlags["url"]; ok {
				llama.URL = toString(urlRaw)
			}

			// Generate port from selector hash if not explicitly set
			if llama.Flags.Port == 0 {
				var h uint32
				for _, b := range id {
					h = 31*h + uint32(b)
				}
				llama.Flags.Port = 10000 + int(h%55536)
			}

			entry := cfg.Models[id]
			entry.LlamaCpp = llama
			cfg.Models[id] = entry
		}
	}

	return &cfg, nil
}

func toInt(v any) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		var n int
		fmt.Sscanf(val, "%d", &n)
		return n
	}
	return 0
}

func toString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case fmt.Stringer:
		return val.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func toFloat(v any) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case string:
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	}
	return 0
}

// FindConfig searches for .aim.yaml starting from the given directory.
func FindConfig(dir string) (string, error) {
	path := filepath.Join(dir, ".aim.yaml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if _, err := os.Stat(filepath.Join(dir, ".aim.yml")); err == nil {
		return filepath.Join(dir, ".aim.yml"), nil
	}
	return "", fmt.Errorf("config file .aim.yaml not found in %s", dir)
}
