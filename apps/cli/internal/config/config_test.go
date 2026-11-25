package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("file-token-123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
server_url = "https://albert.example.com/"
token_file = "` + filepath.ToSlash(tokenFile) + `"
default_model = "chatgpt/gpt-5.4"
max_steps = 50
request_timeout = "2m"
bridge = true
host_label = "MacBook Air"
bridge_tick_prompt = "__custom_tick__"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerURL != "https://albert.example.com" || cfg.Token != "file-token-123" || cfg.DefaultModel != "chatgpt/gpt-5.4" || !cfg.SyncInspector || cfg.MaxSteps != 50 || cfg.RequestTimeout != 2*time.Minute {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if !cfg.Bridge || cfg.HostLabel != "MacBook Air" || cfg.BridgeTickPrompt != "__custom_tick__" {
		t.Fatalf("unexpected bridge config fields: %+v", cfg)
	}

	// Env overrides work even when file does not exist, and defaults apply.
	cfg2, err := load(filepath.Join(dir, "missing.toml"), func(k string) string {
		switch k {
		case "OPENALBERT_SERVER_URL":
			return "http://127.0.0.1:8080"
		case "OPENALBERT_API_TOKEN":
			return "env-token"
		default:
			return ""
		}
	})
	if err != nil || cfg2.ServerURL != "http://127.0.0.1:8080" || cfg2.Token != "env-token" || !cfg2.SyncInspector {
		t.Fatalf("env fallback failed: %+v %v", cfg2, err)
	}
	if cfg2.Bridge || cfg2.HostLabel == "" || cfg2.BridgeTickPrompt != DefaultBridgeTickPrompt {
		t.Fatalf("unexpected bridge defaults: %+v", cfg2)
	}
}

func TestDefaultConfigTomlAndAcpTomlFallback(t *testing.T) {
	xdg := t.TempDir()
	oaDir := filepath.Join(xdg, "openalbert")
	if err := os.MkdirAll(oaDir, 0700); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return xdg
		}
		if k == "XDG_STATE_HOME" {
			return filepath.Join(xdg, "state")
		}
		return ""
	}

	// 1. Only legacy acp.toml exists -> falls back automatically.
	acpPath := filepath.Join(oaDir, "acp.toml")
	if err := os.WriteFile(acpPath, []byte("server_url = \"http://192.168.1.31:8080\"\ntoken = \"legacy-tok\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := load("", getenv)
	if err != nil {
		t.Fatalf("expected fallback to acp.toml, got error: %v", err)
	}
	if cfg.Token != "legacy-tok" || cfg.ServerURL != "http://192.168.1.31:8080" {
		t.Fatalf("unexpected fallback config: %+v", cfg)
	}

	// 2. Once config.toml exists, config.toml takes precedence over acp.toml.
	cfgPath := filepath.Join(oaDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("server_url = \"http://192.168.1.31:8080\"\ntoken = \"new-tok\"\nstate_dir = \""+filepath.ToSlash(filepath.Join(xdg, "custom-state"))+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := load("", getenv)
	if err != nil {
		t.Fatalf("expected config.toml load, got error: %v", err)
	}
	if cfg2.Token != "new-tok" {
		t.Fatalf("expected config.toml to take precedence over acp.toml, got %+v", cfg2)
	}
	wantLock := filepath.Join(xdg, "custom-state", "bridge.lock")
	if got := cfg2.BridgeLockPath(); got != wantLock {
		t.Fatalf("expected BridgeLockPath=%q, got %q", wantLock, got)
	}
}
