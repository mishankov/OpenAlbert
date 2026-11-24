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
	cfgPath := filepath.Join(dir, "acp.toml")
	content := `
server_url = "https://albert.example.com/"
token_file = "` + filepath.ToSlash(tokenFile) + `"
default_model = "chatgpt/gpt-5.4"
max_steps = 50
request_timeout = "2m"
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

	// Env overrides work even when file does not exist.
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
}
