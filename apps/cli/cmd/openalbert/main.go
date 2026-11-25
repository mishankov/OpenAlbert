package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/acp"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "acp":
		return runACP(ctx, args[1:], stdin, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "openalbert", acp.Version)
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "openalbert: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runACP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("openalbert acp", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var configPath string
	var bridgeFlag bool
	fs.StringVar(&configPath, "config", "", "Path to OpenAlbert config (default ~/.config/openalbert/config.toml, fallback acp.toml)")
	fs.BoolVar(&bridgeFlag, "bridge", false, "Enable 100%-LAN T3 Code bridge mode")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: openalbert acp [--config PATH] [--bridge]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Run the OpenAlbert Agent Client Protocol (ACP) server over stdio.")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "openalbert acp: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("failed to load OpenAlbert configuration", "error", err)
		return 1
	}
	if bridgeFlag {
		cfg.Bridge = true
	}

	srv := acp.NewServer(cfg, stdin, stdout, logger)
	if err := srv.Run(ctx); err != nil {
		logger.Error("openalbert acp stopped with error", "error", err)
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "openalbert - OpenAlbert client CLI")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  openalbert <command> [flags]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  acp       Run the Agent Client Protocol (ACP) stdio server (with optional --bridge)")
	fmt.Fprintln(w, "  version   Print the OpenAlbert CLI version")
	fmt.Fprintln(w, "  help      Show this help message")
}
