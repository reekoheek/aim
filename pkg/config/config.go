package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultLlamaServerBin = "llama-server"
	firstSelectorPort     = 10000
	selectorPortRange     = 55536
	defaultSlotExpiration = time.Hour
	defaultSlotMaxFiles   = 3
	defaultInferenceDelay = 1 * time.Second
	defaultIdleTimeout    = 5 * time.Minute
)

type LlamaCppFlags struct {
	Inherit string         `yaml:"inherit"`
	Port    int            `yaml:"port"`
	URL     string         `yaml:"url"`
	Args    map[string]any `yaml:",inline"`
}

func (f LlamaCppFlags) overlay(top LlamaCppFlags) LlamaCppFlags {
	merged := LlamaCppFlags{
		Inherit: cmp.Or(top.Inherit, f.Inherit),
		Port:    cmp.Or(top.Port, f.Port),
		URL:     cmp.Or(top.URL, f.URL),
		Args:    make(map[string]any, len(f.Args)+len(top.Args)),
	}
	maps.Copy(merged.Args, f.Args)
	maps.Copy(merged.Args, top.Args)
	return merged
}

type LlamaCppEntry struct {
	Bin         string
	URL         string
	Port        int
	StopTimeout time.Duration
	Flags       map[string]any
}

type ModelEntry struct {
	Type        string         `yaml:"type"`
	Provider    string         `yaml:"provider"`
	Model       string         `yaml:"model"`
	LlamaCpp    *LlamaCppEntry `yaml:"-"`
	Flags       LlamaCppFlags  `yaml:"flags"`
	Bin         string         `yaml:"bin"`
	URL         string         `yaml:"url"`
	StopTimeout int            `yaml:"stop_timeout"`
}

type ProviderEntry struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

type ServerConfig struct {
	Host           string        `yaml:"host"`
	Port           int           `yaml:"port"`
	SaveSlotPath   string        `yaml:"save_slot_path"`
	SlotExpiration time.Duration `yaml:"slot_expiration"`
	SlotMaxFiles   int           `yaml:"slot_max_files"`
	InferenceDelay time.Duration `yaml:"inference_delay"`
	IdleTimeout    int           `yaml:"idle_timeout"`
}

type Config struct {
	Models    map[string]ModelEntry    `yaml:"models"`
	Providers map[string]ProviderEntry `yaml:"providers"`
	Templates map[string]LlamaCppFlags `yaml:"templates"`
	Server    ServerConfig             `yaml:"server"`
}

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
		url := entry.LlamaCpp.URL
		if url == "" {
			url = fmt.Sprintf("http://127.0.0.1:%d", entry.LlamaCpp.Port)
		}
		return "llama-server", map[string]any{
			"bin":      entry.LlamaCpp.Bin,
			"flags":    entry.LlamaCpp.Flags,
			"selector": selector,
			"url":      url,
		}, nil
	default:
		return "", nil, fmt.Errorf("model %q has unsupported type %q", selector, entry.Type)
	}
}

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

func (c *Config) DefaultServer() ServerConfig {
	if c.Server.Host == "" {
		c.Server.Host = "0.0.0.0"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Server.SaveSlotPath == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			c.Server.SaveSlotPath = filepath.Join(dir, "aim", "slots")
		}
	}
	if c.Server.SlotExpiration <= 0 {
		c.Server.SlotExpiration = defaultSlotExpiration
	}
	if c.Server.SlotMaxFiles <= 0 {
		c.Server.SlotMaxFiles = defaultSlotMaxFiles
	}
	if c.Server.InferenceDelay <= 0 {
		c.Server.InferenceDelay = defaultInferenceDelay
	}
	if c.Server.IdleTimeout <= 0 {
		c.Server.IdleTimeout = int(defaultIdleTimeout / time.Second)
	}
	return c.Server
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
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

	for id, entry := range cfg.Models {
		if entry.Type != "llama-server" {
			continue
		}
		llama, err := cfg.newLlamaCppEntry(id, entry)
		if err != nil {
			return nil, err
		}
		entry.LlamaCpp = llama
		cfg.Models[id] = entry
	}

	return &cfg, nil
}

func (c *Config) newLlamaCppEntry(selector string, entry ModelEntry) (*LlamaCppEntry, error) {
	flags := entry.Flags
	if flags.Inherit != "" {
		template, ok := c.Templates[flags.Inherit]
		if !ok {
			return nil, fmt.Errorf("template %q not found (used by model %q)", flags.Inherit, selector)
		}
		flags = template.overlay(flags)
	}
	for name := range flags.Args {
		if !strings.HasPrefix(name, "-") {
			return nil, fmt.Errorf("model %q: unknown setting %q (llama-server flags start with - or --)", selector, name)
		}
	}

	llama := &LlamaCppEntry{
		Bin:         cmp.Or(entry.Bin, defaultLlamaServerBin),
		URL:         cmp.Or(entry.URL, flags.URL),
		Port:        cmp.Or(flags.Port, portFromSelector(selector)),
		StopTimeout: time.Duration(cmp.Or(entry.StopTimeout, 30)) * time.Second,
		Flags:       flags.Args,
	}
	return llama, nil
}

func portFromSelector(selector string) int {
	var hash uint32
	for _, b := range selector {
		hash = 31*hash + uint32(b)
	}
	return firstSelectorPort + int(hash%selectorPortRange)
}

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
