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
	"github.com/reekoheek/aim/pkg/router/chatcompletion"
	"github.com/reekoheek/aim/pkg/router/messages"
	"github.com/reekoheek/aim/pkg/router/systemone"
	"github.com/reekoheek/aim/pkg/strategy"
	"github.com/reekoheek/aim/pkg/strategy/llamaserver"
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
		Short: "aim — an AI Management Server",
		Long:  "aim is a lightweight AI management server that forwards requests to supported APIs and llama.cpp server.",
		RunE:  run,
	}

	cmd.Flags().StringVarP(&cfgFile, "config", "c", "", "config file path (default: .aim.yaml in CWD)")
	cmd.Version = version

	return cmd
}

func run(cmd *cobra.Command, args []string) error {
	l := logger.New(os.Getenv("AIM_LOG_LEVEL"))

	var cfgPath string
	if cfgFile != "" {
		cfgPath = cfgFile
	} else {
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

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if len(cfg.Models) == 0 {
		return fmt.Errorf("no models configured in %s", cfgPath)
	}

	for modelID := range cfg.Models {
		l.Info("configured model", slog.String("id", modelID))
	}

	llamaServers := llamaserver.NewMonitor()
	chatCompletionRouter := chatcompletion.New(cfg, l, llamaServers)
	systemOneRouter := systemone.New(cfg, l, llamaServers)
	messagesRouter := messages.New(cfg, l, llamaServers)
	serverCfg := cfg.DefaultServer()
	addr := fmt.Sprintf("%s:%d", serverCfg.Host, serverCfg.Port)

	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodPost+" "+strategy.ChatCompletionsPath, chatCompletionRouter.Handler)
	mux.HandleFunc(http.MethodPost+" "+strategy.SystemOnePath, systemOneRouter.Handler)
	mux.HandleFunc(http.MethodPost+" "+strategy.MessagesPath, messagesRouter.Handler)
	mux.HandleFunc(http.MethodPost+" "+strategy.CountTokensPath, messagesRouter.CountTokensHandler)

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

	slotPruneCtx, stopSlotPrune := context.WithCancel(context.Background())
	defer stopSlotPrune()
	if serverCfg.SaveSlotPath != "" {
		l.Info("slot cache limits",
			slog.String("dir", serverCfg.SaveSlotPath),
			logger.Seconds("expiration", serverCfg.SlotExpiration),
			slog.Int("max_files", serverCfg.SlotMaxFiles),
		)
		go llamaserver.PruneSlots(slotPruneCtx, serverCfg, l)
	}

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
		llamaServers.Close(ctx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	<-shutdownDone
	return nil
}
