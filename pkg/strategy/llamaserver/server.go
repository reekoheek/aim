package llamaserver

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/process"
)

const (
	processName      = "llama.cpp server"
	healthPath       = "/health"
	configFileNotice = "using config file"
	minParallelSlots = 1
)

type Server struct {
	process       *process.Process
	port          int
	host          string
	flags         map[string]any
	parallelSlots int
	slotContext   int
	selector      string
	server        config.ServerConfig
	slotDir       string
	slots         *slotCache
	logger        *logger.Logger
	mu            sync.Mutex
}

func NewServer(selector string, entry *config.LlamaCppEntry, server config.ServerConfig, l *logger.Logger) *Server {
	s := &Server{
		port:          entry.Port,
		host:          process.DefaultHost,
		flags:         entry.Flags,
		parallelSlots: max(entry.Slots, minParallelSlots),
		slotContext:   entry.Ctx,
		selector:      selector,
		server:        server,
		logger:        l,
	}
	s.process = process.New(process.Spec{
		Name:          processName,
		Selector:      selector,
		Bin:           entry.Bin,
		Host:          s.host,
		Port:          s.port,
		Args:          s.args(),
		HealthPath:    healthPath,
		StartTimeout:  entry.StartTimeout,
		StopTimeout:   entry.StopTimeout,
		MutedLogLines: []string{configFileNotice},
	}, l)
	return s
}

func prepareServer(selector string, entry *config.LlamaCppEntry, server config.ServerConfig, l *logger.Logger) (*Server, error) {
	if server.SaveSlotPath == "" || entry.Slots == 0 {
		return NewServer(selector, entry, server, l), nil
	}
	dir, err := prepareSlotDir(server.SaveSlotPath)
	if err != nil {
		l.LogError("invalid slot save path", err)
		return nil, fmt.Errorf("%w: %w", lane.ErrStartFailed, err)
	}
	srv := NewServer(selector, withSlotSavePath(entry, dir), server, l)
	srv.slotDir = dir
	return srv, nil
}

func runServer(ctx context.Context, supervisor *lane.Supervisor, cfg *config.Config, l *logger.Logger, selector string) (*lane.Lease[*Server], error) {
	entry, err := cfg.GetLlamaCppEntry(selector)
	if err != nil {
		return nil, err
	}
	return lane.Run(ctx, supervisor, entry.Lane, selector, func() (*Server, error) {
		return prepareServer(selector, entry, cfg.DefaultServer(), l)
	})
}

func (s *Server) Start(ctx context.Context) error {
	if err := s.process.Start(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.slotDir != "" && s.slots == nil {
		s.slots = newSlotCache(s.slotDir, s.selector, s.URL(), s.parallelSlots, s.server, s.logger, s.process.Done())
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) {
	if slots := s.cachedSlots(); slots != nil {
		slots.shutdown(ctx)
	}
	s.process.Stop(ctx)
}

func (s *Server) Exited() bool {
	return s.process.Exited()
}

func (s *Server) cachedSlots() *slotCache {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.slots
}

func (s *Server) URL() string {
	return s.process.URL()
}

func (s *Server) Selector() string {
	return s.selector
}

func (s *Server) GetFlags() map[string]any {
	return s.flags
}

func (s *Server) args() []string {
	args := append([]string{
		"--offline",
		"--no-cors-credentials",
		"--cors-origins", "localhost",
		"--no-ui",
	}, process.FlagArgs(s.flags)...)
	args = append(args, s.slotArgs()...)
	return append(args,
		"--host", s.host,
		"--port", strconv.Itoa(s.port),
		"--log-colors", "off",
	)
}

func (s *Server) slotArgs() []string {
	args := []string{"-np", strconv.Itoa(s.parallelSlots)}
	if s.slotContext > 0 {
		args = append(args, "-c", strconv.Itoa(s.slotContext*s.parallelSlots))
	}
	return args
}
