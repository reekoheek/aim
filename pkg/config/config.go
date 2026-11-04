package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultLlamaServerBin = "llama-server"
	defaultSDServerBin    = "sd-server"
	defaultStartTimeout   = 30 * time.Second
	defaultStopTimeout    = 30 * time.Second
	firstSelectorPort     = 10000
	selectorPortRange     = 55536
	defaultSlotExpiration = time.Hour
	defaultSlotMaxFiles   = 3
	defaultIdleTimeout    = 5 * time.Minute
	defaultLlamaServerURL = "http://127.0.0.1:%d"
	defaultSlots          = 1
	anyCORSOrigin         = "*"
	DefaultLane           = "default"
)

const (
	ModelTypeChatCompletion = "chat-completion"
	ModelTypeSystemOne      = "systemone"
	ModelTypeLlamaServer    = "llama-server"
	ModelTypeAnthropic      = "anthropic"
	ModelTypeSDServer       = "sd-server"
)

type reservedFlags struct {
	names  []string
	reason string
}

var sdServerReservedFlagGroups = []reservedFlags{
	{names: []string{"-l", "--listen-ip", "--listen-port"}, reason: "is set by aim"},
}

var sdServerModelFlags = []string{"-m", "--model", "--diffusion-model"}

var reservedFlagGroups = []reservedFlags{
	{names: []string{"-np", "--parallel"}, reason: "is set by aim from slots"},
	{names: []string{"-c", "--ctx-size"}, reason: "is set by aim from ctx"},
	{names: []string{"--kv-unified-per-slot"}, reason: "is replaced by ctx"},
	{names: []string{"-kvu", "--kv-unified", "-no-kvu", "--no-kv-unified"}, reason: "is not supported: unified KV purges idle slots that aim keeps cached"},
	{names: []string{
		"--offline",
		"--cors-credentials",
		"--no-cors-credentials",
		"--cors-origins",
		"--ui",
		"--webui",
		"--no-ui",
		"--no-webui",
		"--host",
		"--port",
		"--log-colors",
		"--slot-save-path",
	}, reason: "is set by aim"},
}

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
	Lane         string
	Bin          string
	URL          string
	Port         int
	StartTimeout time.Duration
	StopTimeout  time.Duration
	Slots        int
	Ctx          int
	Flags        map[string]any
}

type SDCppEntry struct {
	Lane         string
	Bin          string
	URL          string
	Port         int
	StartTimeout time.Duration
	StopTimeout  time.Duration
	Flags        map[string]any
}

type ModelEntry struct {
	Type         string         `yaml:"type"`
	Provider     string         `yaml:"provider"`
	Model        string         `yaml:"model"`
	Lane         string         `yaml:"lane"`
	LlamaCpp     *LlamaCppEntry `yaml:"-"`
	SDCpp        *SDCppEntry    `yaml:"-"`
	Flags        LlamaCppFlags  `yaml:"flags"`
	Bin          string         `yaml:"bin"`
	URL          string         `yaml:"url"`
	StartTimeout *int           `yaml:"start_timeout"`
	StopTimeout  int            `yaml:"stop_timeout"`
	Slots        *int           `yaml:"slots"`
	Ctx          *int           `yaml:"ctx"`
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
	IdleTimeout    *int          `yaml:"idle_timeout"`
	CORSOrigins    []string      `yaml:"cors_origins"`
}

type Config struct {
	Models    map[string]ModelEntry    `yaml:"models"`
	Providers map[string]ProviderEntry `yaml:"providers"`
	Templates map[string]LlamaCppFlags `yaml:"templates"`
	Server    ServerConfig             `yaml:"server"`
}

func (c *Config) ResolveModel(selector string) (ModelEntry, error) {
	entry, ok := c.Models[selector]
	if !ok {
		return ModelEntry{}, fmt.Errorf("model %q not found in config", selector)
	}
	switch entry.Type {
	case ModelTypeChatCompletion, ModelTypeSystemOne, ModelTypeAnthropic:
		if err := c.validateChatCompletion(selector, entry); err != nil {
			return ModelEntry{}, err
		}
	case ModelTypeLlamaServer, ModelTypeSDServer:
	default:
		return ModelEntry{}, fmt.Errorf("model %q has unsupported type %q", selector, entry.Type)
	}
	return entry, nil
}

func (c *Config) validateChatCompletion(selector string, entry ModelEntry) error {
	if entry.Provider == "" {
		return fmt.Errorf("model %q has empty provider", selector)
	}
	if entry.Model == "" {
		return fmt.Errorf("model %q has empty model name", selector)
	}
	if _, ok := c.Providers[entry.Provider]; !ok {
		return fmt.Errorf("provider %q not found in config", entry.Provider)
	}
	return nil
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
	if entry.Type != ModelTypeLlamaServer {
		return nil, fmt.Errorf("model %q is not type %s", selector, ModelTypeLlamaServer)
	}
	if entry.LlamaCpp == nil {
		return nil, fmt.Errorf("model %q has no llama.cpp config", selector)
	}
	return entry.LlamaCpp, nil
}

func (c *Config) GetSDCppEntry(selector string) (*SDCppEntry, error) {
	entry, ok := c.Models[selector]
	if !ok {
		return nil, fmt.Errorf("model %q not found in config", selector)
	}
	if entry.Type != ModelTypeSDServer {
		return nil, fmt.Errorf("model %q is not type %s", selector, ModelTypeSDServer)
	}
	if entry.SDCpp == nil {
		return nil, fmt.Errorf("model %q has no sd.cpp config", selector)
	}
	return entry.SDCpp, nil
}

func (c *Config) GetModelURL(selector string) (string, error) {
	llama, err := c.GetLlamaCppEntry(selector)
	if err != nil {
		return "", err
	}
	return cmp.Or(llama.URL, fmt.Sprintf(defaultLlamaServerURL, llama.Port)), nil
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
	if c.Server.IdleTimeout == nil {
		defaultIdleTimeoutSeconds := int(defaultIdleTimeout / time.Second)
		c.Server.IdleTimeout = &defaultIdleTimeoutSeconds
	}
	return c.Server
}

func (s ServerConfig) IdleTimeoutDuration() time.Duration {
	if s.IdleTimeout == nil {
		return defaultIdleTimeout
	}
	return time.Duration(*s.IdleTimeout) * time.Second
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

	if err := validateCORSOrigins(cfg.Server.CORSOrigins); err != nil {
		return nil, err
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
		var err error
		switch entry.Type {
		case ModelTypeLlamaServer:
			entry.LlamaCpp, err = cfg.newLlamaCppEntry(id, entry)
		case ModelTypeSDServer:
			entry.SDCpp, err = cfg.newSDCppEntry(id, entry)
		default:
			err = rejectLocalProcessSettings(id, entry)
		}
		if err != nil {
			return nil, err
		}
		cfg.Models[id] = entry
	}

	return &cfg, nil
}

func rejectLocalProcessSettings(selector string, entry ModelEntry) error {
	if entry.Lane != "" {
		return fmt.Errorf("model %q: lane is only for types %s and %s", selector, ModelTypeLlamaServer, ModelTypeSDServer)
	}
	if entry.StartTimeout != nil {
		return fmt.Errorf("model %q: start_timeout is only for types %s and %s", selector, ModelTypeLlamaServer, ModelTypeSDServer)
	}
	return nil
}

func (c *Config) newLlamaCppEntry(selector string, entry ModelEntry) (*LlamaCppEntry, error) {
	flags, err := c.inheritFlags(selector, entry.Flags)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedFlagNames(flags.Args) {
		if err := validateFlagName(selector, name, ModelTypeLlamaServer, reservedFlagGroups); err != nil {
			return nil, err
		}
	}
	slots, err := slotCount(selector, entry.Slots)
	if err != nil {
		return nil, err
	}
	ctx, err := slotContext(selector, entry.Ctx)
	if err != nil {
		return nil, err
	}
	startTimeout, err := startTimeoutDuration(selector, entry.StartTimeout)
	if err != nil {
		return nil, err
	}

	llama := &LlamaCppEntry{
		Lane:         cmp.Or(entry.Lane, DefaultLane),
		Bin:          cmp.Or(entry.Bin, defaultLlamaServerBin),
		URL:          cmp.Or(entry.URL, flags.URL),
		Port:         cmp.Or(flags.Port, portFromSelector(selector)),
		StartTimeout: startTimeout,
		StopTimeout:  stopTimeoutDuration(entry.StopTimeout),
		Slots:        slots,
		Ctx:          ctx,
		Flags:        flags.Args,
	}
	return llama, nil
}

func (c *Config) newSDCppEntry(selector string, entry ModelEntry) (*SDCppEntry, error) {
	if entry.Slots != nil {
		return nil, fmt.Errorf("model %q: slots is only for type %s", selector, ModelTypeLlamaServer)
	}
	if entry.Ctx != nil {
		return nil, fmt.Errorf("model %q: ctx is only for type %s", selector, ModelTypeLlamaServer)
	}
	flags, err := c.inheritFlags(selector, entry.Flags)
	if err != nil {
		return nil, err
	}
	for _, name := range sortedFlagNames(flags.Args) {
		if err := validateFlagName(selector, name, ModelTypeSDServer, sdServerReservedFlagGroups); err != nil {
			return nil, err
		}
	}
	if !hasAnyFlag(flags.Args, sdServerModelFlags) {
		return nil, fmt.Errorf("model %q: flags need one of %s", selector, strings.Join(sdServerModelFlags, ", "))
	}
	startTimeout, err := startTimeoutDuration(selector, entry.StartTimeout)
	if err != nil {
		return nil, err
	}

	sd := &SDCppEntry{
		Lane:         cmp.Or(entry.Lane, DefaultLane),
		Bin:          cmp.Or(entry.Bin, defaultSDServerBin),
		URL:          cmp.Or(entry.URL, flags.URL),
		Port:         cmp.Or(flags.Port, portFromSelector(selector)),
		StartTimeout: startTimeout,
		StopTimeout:  stopTimeoutDuration(entry.StopTimeout),
		Flags:        flags.Args,
	}
	return sd, nil
}

func (c *Config) inheritFlags(selector string, flags LlamaCppFlags) (LlamaCppFlags, error) {
	if flags.Inherit == "" {
		return flags, nil
	}
	template, ok := c.Templates[flags.Inherit]
	if !ok {
		return LlamaCppFlags{}, fmt.Errorf("template %q not found (used by model %q)", flags.Inherit, selector)
	}
	return template.overlay(flags), nil
}

func hasAnyFlag(flags map[string]any, names []string) bool {
	for _, name := range names {
		if value, ok := flags[name]; ok && value != false && value != nil && value != "" {
			return true
		}
	}
	return false
}

func startTimeoutDuration(selector string, seconds *int) (time.Duration, error) {
	if seconds == nil {
		return defaultStartTimeout, nil
	}
	if *seconds < 0 {
		return 0, fmt.Errorf("model %q: start_timeout must be >= 0", selector)
	}
	return time.Duration(*seconds) * time.Second, nil
}

func stopTimeoutDuration(seconds int) time.Duration {
	if seconds == 0 {
		return defaultStopTimeout
	}
	return time.Duration(seconds) * time.Second
}

func sortedFlagNames(flags map[string]any) []string {
	names := make([]string, 0, len(flags))
	for name := range flags {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func validateFlagName(selector string, name string, modelType string, reserved []reservedFlags) error {
	if !strings.HasPrefix(name, "-") {
		return fmt.Errorf("model %q: unknown setting %q (%s flags start with - or --)", selector, name, modelType)
	}
	for _, group := range reserved {
		if slices.Contains(group.names, name) {
			return fmt.Errorf("model %q: flag %s %s", selector, name, group.reason)
		}
	}
	return nil
}

func slotCount(selector string, slots *int) (int, error) {
	if slots == nil {
		return defaultSlots, nil
	}
	if *slots < 0 {
		return 0, fmt.Errorf("model %q: slots must be >= 0", selector)
	}
	return *slots, nil
}

func slotContext(selector string, ctx *int) (int, error) {
	if ctx == nil {
		return 0, nil
	}
	if *ctx <= 0 {
		return 0, fmt.Errorf("model %q: ctx must be > 0", selector)
	}
	return *ctx, nil
}

func validateCORSOrigins(origins []string) error {
	for _, origin := range origins {
		if origin == anyCORSOrigin {
			continue
		}
		parsed, err := url.Parse(origin)
		isOrigin := err == nil &&
			(parsed.Scheme == "http" || parsed.Scheme == "https") &&
			parsed.Host != "" &&
			parsed.User == nil &&
			parsed.Path == "" &&
			!parsed.ForceQuery && parsed.RawQuery == "" &&
			parsed.Fragment == ""
		if !isOrigin {
			return fmt.Errorf("server: cors_origins entry %q must be %q or scheme://host[:port] such as http://localhost:3000", origin, anyCORSOrigin)
		}
	}
	return nil
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
