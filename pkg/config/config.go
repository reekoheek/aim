package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ModelEntry represents a model configuration referencing a provider.
type ModelEntry struct {
	Type     string `yaml:"type"`
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
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
	Models    map[string]ModelEntry   `yaml:"models"`
	Providers map[string]ProviderEntry `yaml:"providers"`
	Server    ServerConfig            `yaml:"server"`
}

// ResolveModel resolves a model selector (e.g., "provider/model") to its provider and model info.
func (c *Config) ResolveModel(selector string) (provider string, model string, err error) {
	entry, ok := c.Models[selector]
	if !ok {
		return "", "", fmt.Errorf("model %q not found in config", selector)
	}
	if entry.Type != "provider" {
		return "", "", fmt.Errorf("model %q has invalid type %q, expected %q", selector, entry.Type, "provider")
	}
	if entry.Provider == "" {
		return "", "", fmt.Errorf("model %q has empty provider", selector)
	}
	if entry.Model == "" {
		return "", "", fmt.Errorf("model %q has empty model name", selector)
	}
	if _, ok := c.Providers[entry.Provider]; !ok {
		return "", "", fmt.Errorf("provider %q not found in config", entry.Provider)
	}
	return entry.Provider, entry.Model, nil
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

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if cfg.Models == nil {
		cfg.Models = make(map[string]ModelEntry)
	}
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderEntry)
	}

	return &cfg, nil
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
