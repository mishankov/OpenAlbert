package config

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config defines launch settings for openalbert-acp, typically loaded from
// ~/.config/openalbert/acp.toml.
type Config struct {
	ServerURL      string
	Token          string
	DefaultModel   string
	SyncInspector  bool
	StateDir       string
	MaxSteps       int
	RequestTimeout time.Duration
}

type fileConfig struct {
	ServerURL      string `toml:"server_url"`
	Token          string `toml:"token"`
	TokenEnv       string `toml:"token_env"`
	TokenFile      string `toml:"token_file"`
	DefaultModel   string `toml:"default_model"`
	SyncInspector  *bool  `toml:"sync_inspector"`
	StateDir       string `toml:"state_dir"`
	MaxSteps       int    `toml:"max_steps"`
	RequestTimeout string `toml:"request_timeout"`
}

// DefaultPath returns ~/.config/openalbert/acp.toml (or $XDG_CONFIG_HOME/openalbert/acp.toml).
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv("OPENALBERT_ACP_CONFIG")); p != "" {
		return expandHome(p)
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "openalbert", "acp.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "openalbert", "acp.toml")
}

// DefaultStateDir returns ~/.local/state/openalbert/acp (or $XDG_STATE_HOME/openalbert/acp).
func DefaultStateDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "openalbert", "acp")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "openalbert", "acp")
}

// Load reads acp.toml (if present) and applies OPENALBERT_SERVER_URL /
// OPENALBERT_API_TOKEN environment overrides.
func Load(path string) (Config, error) {
	return load(path, os.Getenv)
}

func load(path string, getenv func(string) string) (Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	path = expandHome(path)
	var raw fileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Config{}, errors.New("could not read ACP configuration file")
		}
	} else {
		meta, err := toml.Decode(string(data), &raw)
		if err != nil {
			return Config{}, errors.New("ACP configuration file contains invalid TOML")
		}
		if len(meta.Undecoded()) > 0 {
			return Config{}, errors.New("ACP configuration file contains unsupported fields")
		}
	}

	serverURL := strings.TrimSpace(raw.ServerURL)
	if envURL := strings.TrimSpace(getenv("OPENALBERT_SERVER_URL")); envURL != "" {
		serverURL = envURL
	}
	if serverURL == "" {
		return Config{}, errors.New("server_url (or OPENALBERT_SERVER_URL) is required")
	}
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return Config{}, errors.New("server_url must be a valid http(s) URL without embedded credentials")
	}
	serverURL = strings.TrimRight(u.String(), "/")

	token := strings.TrimSpace(raw.Token)
	if envName := strings.TrimSpace(raw.TokenEnv); envName != "" {
		if v := strings.TrimSpace(getenv(envName)); v != "" {
			token = v
		}
	}
	if tokenFile := strings.TrimSpace(raw.TokenFile); tokenFile != "" && token == "" {
		b, err := os.ReadFile(expandHome(tokenFile))
		if err != nil {
			return Config{}, errors.New("could not read token_file")
		}
		token = strings.TrimSpace(string(b))
	}
	if envToken := strings.TrimSpace(getenv("OPENALBERT_API_TOKEN")); envToken != "" {
		token = envToken
	}
	if token == "" {
		return Config{}, errors.New("API token is required (set token, token_env, token_file, or OPENALBERT_API_TOKEN)")
	}

	syncInspector := true
	if raw.SyncInspector != nil {
		syncInspector = *raw.SyncInspector
	}
	stateDir := strings.TrimSpace(raw.StateDir)
	if stateDir == "" {
		stateDir = DefaultStateDir()
	} else {
		stateDir = expandHome(stateDir)
	}
	maxSteps := raw.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 80
	}
	if maxSteps > 300 {
		return Config{}, errors.New("max_steps must be between 1 and 300")
	}
	reqTimeout := 3 * time.Minute
	if strings.TrimSpace(raw.RequestTimeout) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(raw.RequestTimeout))
		if err != nil || d <= 0 {
			return Config{}, errors.New("request_timeout must be a positive duration")
		}
		reqTimeout = d
	}

	return Config{
		ServerURL:      serverURL,
		Token:          token,
		DefaultModel:   strings.TrimSpace(raw.DefaultModel),
		SyncInspector:  syncInspector,
		StateDir:       stateDir,
		MaxSteps:       maxSteps,
		RequestTimeout: reqTimeout,
	}, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
