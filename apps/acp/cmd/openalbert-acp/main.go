package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"openalbert/apps/acp/internal/acp"
	"openalbert/apps/acp/internal/config"
)

func main() {
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "Path to OpenAlbert ACP config (default ~/.config/openalbert/acp.toml)")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("openalbert-acp", acp.Version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load ACP configuration", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := acp.NewServer(cfg, os.Stdin, os.Stdout, logger)
	if err := srv.Run(ctx); err != nil {
		logger.Error("openalbert-acp stopped with error", "error", err)
		os.Exit(1)
	}
}
