package sdserver

import (
	"context"
	"strconv"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/lane"
	"github.com/reekoheek/aim/pkg/process"
)

const (
	processName = "sd.cpp server"
	healthPath  = "/v1/models"
)

type Server struct {
	process *process.Process
	host    string
	port    int
	flags   map[string]any
}

func NewServer(selector string, entry *config.SDCppEntry, l *logger.Logger) *Server {
	s := &Server{
		host:  process.DefaultHost,
		port:  entry.Port,
		flags: entry.Flags,
	}
	s.process = process.New(process.Spec{
		Name:         processName,
		Selector:     selector,
		Bin:          entry.Bin,
		Host:         s.host,
		Port:         s.port,
		Args:         s.args(),
		HealthPath:   healthPath,
		StartTimeout: entry.StartTimeout,
		StopTimeout:  entry.StopTimeout,
	}, l)
	return s
}

func runServer(ctx context.Context, supervisor *lane.Supervisor, cfg *config.Config, l *logger.Logger, selector string) (*lane.Lease[*Server], error) {
	entry, err := cfg.GetSDCppEntry(selector)
	if err != nil {
		return nil, err
	}
	return lane.Run(ctx, supervisor, entry.Lane, selector, func() (*Server, error) {
		return NewServer(selector, entry, l), nil
	})
}

func (s *Server) Start(ctx context.Context) error {
	return s.process.Start(ctx)
}

func (s *Server) Stop(ctx context.Context) {
	s.process.Stop(ctx)
}

func (s *Server) Exited() bool {
	return s.process.Exited()
}

func (s *Server) URL() string {
	return s.process.URL()
}

func (s *Server) args() []string {
	return append(process.FlagArgs(s.flags),
		"--listen-ip", s.host,
		"--listen-port", strconv.Itoa(s.port),
	)
}
