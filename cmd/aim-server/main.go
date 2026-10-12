package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/reekoheek/aim/internal/logger"
	"github.com/reekoheek/aim/pkg/config"
	"github.com/reekoheek/aim/pkg/proxy"
)

var (
	cfgFile string
	version = "dev"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "aim-server",
		Short: "aim — an LLM proxy server",
		Long:  "aim is a lightweight LLM proxy server that forwards requests to OpenAI-compatible APIs.",
		RunE:  run,
	}

	cmd.Flags().StringVarP(&cfgFile, "config", "c", "", "config file path (default: .aim.yaml in CWD)")
	cmd.Version = version

	return cmd
}

func run(cmd *cobra.Command, args []string) error {
	l := logger.New(os.Getenv("AIM_LOG_LEVEL"))

	// Determine config path
	var cfgPath string
	if cfgFile != "" {
		// Use explicit path
		cfgPath = cfgFile
	} else {
		// Find in CWD
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		found, err := config.FindConfig(cwd)
		if err != nil {
			return fmt.Errorf("config not found: %w", err)
		}
		cfgPath = found
	}

	l.Info("loading config", slog.String("path", cfgPath))

	// Load config
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Validate models
	if len(cfg.Models) == 0 {
		return fmt.Errorf("no models configured in %s", cfgPath)
	}

	// Log configured models
	for modelID := range cfg.Models {
		l.Info("configured model", slog.String("id", modelID))
	}

	// Setup proxy
	p := proxy.New(cfg, l)

	// Setup server
	serverCfg := cfg.DefaultServer()
	addr := fmt.Sprintf("%s:%d", serverCfg.Host, serverCfg.Port)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", p.Handler)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	l.Info("starting server",
		slog.String("addr", addr),
		slog.String("config", cfgPath),
	)

	absPath, _ := filepath.Abs(cfgPath)
	l.Info("config loaded", slog.String("path", absPath))

	// Setup signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-sigCh
		l.Info("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		// Stop llama-server so it does not outlive us holding its port.
		p.Close(ctx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	// ListenAndServe returns as soon as Shutdown begins; wait for it to finish.
	<-shutdownDone
	return nil
}
