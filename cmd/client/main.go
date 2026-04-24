package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	clientruntime "moe-asset-client/internal/client"
	"moe-asset-client/internal/config"
	harukiLogger "moe-asset-client/internal/logger"
)

var Version = "dev"

func main() {
	configPath := flag.String("config", "", "path to client config yaml")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	cfg.Worker.Version = Version

	logWriter := io.Writer(os.Stdout)
	if cfg.Logging.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Logging.File), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "failed to create log directory: %v\n", err)
			os.Exit(1)
		}
		file, err := os.OpenFile(cfg.Logging.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = file.Close() }()
		logWriter = io.MultiWriter(os.Stdout, file)
	}
	logger := harukiLogger.NewLogger("Client", cfg.Logging.Level, logWriter)
	logger.Infof("========================= Moe Asset Client %s =========================", Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := clientruntime.NewWorker(cfg, logger).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Errorf("client stopped: %v", err)
		os.Exit(1)
	}
}
