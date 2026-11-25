package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/acp"
)

func TestCLISubcommands(t *testing.T) {
	t.Run("version subcommand", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run(context.Background(), []string{"version"}, strings.NewReader(""), &out, &errBuf)
		if code != 0 {
			t.Fatalf("expected exit 0, got %d (stderr=%q)", code, errBuf.String())
		}
		if got := strings.TrimSpace(out.String()); got != "openalbert "+acp.Version {
			t.Fatalf("unexpected version output: %q", got)
		}
	})

	t.Run("--version flag", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run(context.Background(), []string{"--version"}, strings.NewReader(""), &out, &errBuf)
		if code != 0 || strings.TrimSpace(out.String()) != "openalbert "+acp.Version {
			t.Fatalf("unexpected --version result: code=%d out=%q", code, out.String())
		}
	})

	t.Run("help and empty args", func(t *testing.T) {
		for _, args := range [][]string{nil, {"help"}, {"--help"}} {
			var out, errBuf bytes.Buffer
			code := run(context.Background(), args, strings.NewReader(""), &out, &errBuf)
			if code != 0 || !strings.Contains(out.String(), "openalbert <command>") || !strings.Contains(out.String(), "acp") {
				t.Fatalf("unexpected help output for %v: code=%d out=%q", args, code, out.String())
			}
		}
	})

	t.Run("unknown subcommand", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run(context.Background(), []string{"bogus"}, strings.NewReader(""), &out, &errBuf)
		if code != 2 || !strings.Contains(errBuf.String(), `unknown subcommand "bogus"`) {
			t.Fatalf("expected exit 2 for unknown subcommand, got code=%d stderr=%q", code, errBuf.String())
		}
	})

	t.Run("acp subcommand runs to clean EOF with --config and --bridge", func(t *testing.T) {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.toml")
		content := "server_url = \"http://127.0.0.1:8080\"\ntoken = \"tok\"\nstate_dir = \"" + filepath.ToSlash(dir) + "\"\n"
		if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		var out, errBuf bytes.Buffer
		code := run(context.Background(), []string{"acp", "--config", cfgPath, "--bridge"}, strings.NewReader(""), &out, &errBuf)
		if code != 0 {
			t.Fatalf("expected clean EOF exit 0, got %d (stderr=%q)", code, errBuf.String())
		}
	})

	t.Run("acp --help", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		code := run(context.Background(), []string{"acp", "--help"}, strings.NewReader(""), &out, &errBuf)
		if code != 0 || !strings.Contains(errBuf.String(), "--bridge") || !strings.Contains(errBuf.String(), "--config") {
			t.Fatalf("unexpected acp --help: code=%d stderr=%q", code, errBuf.String())
		}
	})
}
