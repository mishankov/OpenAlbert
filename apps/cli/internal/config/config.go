package config

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const DefaultBridgeTickPrompt = "__openalbert_bridge_tick__"

// Config defines launch settings for the openalbert CLI, typically loaded from
// ~/.config/openalbert/config.toml (with automatic fallback to acp.toml).
type Config struct {
	ServerURL        string
	Token            string
	DefaultModel     string
	SyncInspector    bool
	StateDir         string
	CustomStateDir   bool
	MaxSteps         int
	RequestTimeout   time.Duration
	Bridge           bool
	HostLabel        string
	BridgeTickPrompt string
}

type fileConfig struct {
	ServerURL        string `toml:"server_url"`
	Token            string `toml:"token"`
	TokenEnv         string `toml:"token_env"`
	TokenFile        string `toml:"token_file"`
	DefaultModel     string `toml:"default_model"`
	SyncInspector    *bool  `toml:"sync_inspector"`
	StateDir         string `toml:"state_dir"`
	MaxSteps         int    `toml:"max_steps"`
	RequestTimeout   string `toml:"request_timeout"`
	Bridge           *bool  `toml:"bridge"`
	HostLabel        string `toml:"host_label"`
	BridgeTickPrompt string `toml:"bridge_tick_prompt"`
}

func configDir(getenv func(string) string) string {
	if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(expandHome(xdg), "openalbert")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "openalbert")
}

func stateRootDir(getenv func(string) string) string {
	if xdg := strings.TrimSpace(getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(expandHome(xdg), "openalbert")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "openalbert")
}

// DefaultPath returns ~/.config/openalbert/config.toml (or $XDG_CONFIG_HOME/openalbert/config.toml),
// honoring OPENALBERT_CONFIG and legacy OPENALBERT_ACP_CONFIG.
func DefaultPath() string {
	return defaultPath(os.Getenv)
}

func defaultPath(getenv func(string) string) string {
	if p := strings.TrimSpace(getenv("OPENALBERT_CONFIG")); p != "" {
		return expandHome(p)
	}
	if p := strings.TrimSpace(getenv("OPENALBERT_ACP_CONFIG")); p != "" {
		return expandHome(p)
	}
	return filepath.Join(configDir(getenv), "config.toml")
}

// DefaultStateDir returns ~/.local/state/openalbert/acp (or $XDG_STATE_HOME/openalbert/acp).
func DefaultStateDir() string {
	return filepath.Join(stateRootDir(os.Getenv), "acp")
}

// BridgeLockPath returns the singleton lock path for the LAN T3 bridge.
// When StateDir is explicitly configured (e.g. in tests or custom installs),
// the lock lives inside StateDir; otherwise it uses ~/.local/state/openalbert/bridge.lock.
func (c Config) BridgeLockPath() string {
	if c.CustomStateDir && strings.TrimSpace(c.StateDir) != "" {
		return filepath.Join(c.StateDir, "bridge.lock")
	}
	if strings.TrimSpace(c.StateDir) != "" && c.StateDir != DefaultStateDir() {
		return filepath.Join(c.StateDir, "bridge.lock")
	}
	return filepath.Join(stateRootDir(os.Getenv), "bridge.lock")
}

// Load reads config.toml (falling back to acp.toml if config.toml does not exist)
// and applies OPENALBERT_SERVER_URL / OPENALBERT_API_TOKEN / OPENALBERT_BRIDGE overrides.
func Load(path string) (Config, error) {
	return load(path, os.Getenv)
}

func load(path string, getenv func(string) string) (Config, error) {
	explicitPath := strings.TrimSpace(path) != ""
	var candidates []string
	if explicitPath {
		candidates = []string{expandHome(strings.TrimSpace(path))}
	} else if envPath := strings.TrimSpace(getenv("OPENALBERT_CONFIG")); envPath != "" {
		candidates = []string{expandHome(envPath)}
	} else if envPath := strings.TrimSpace(getenv("OPENALBERT_ACP_CONFIG")); envPath != "" {
		candidates = []string{expandHome(envPath)}
	} else {
		dir := configDir(getenv)
		candidates = []string{
			filepath.Join(dir, "config.toml"),
			filepath.Join(dir, "acp.toml"),
		}
	}

	var raw fileConfig
	for _, candidate := range candidates {
		data, err := os.ReadFile(candidate)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return Config{}, errors.New("could not read OpenAlbert configuration file")
		}
		meta, err := toml.Decode(string(data), &raw)
		if err != nil {
			return Config{}, errors.New("OpenAlbert configuration file contains invalid TOML")
		}
		if len(meta.Undecoded()) > 0 {
			return Config{}, errors.New("OpenAlbert configuration file contains unsupported fields")
		}
		break
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
	customStateDir := stateDir != ""
	if !customStateDir {
		stateDir = filepath.Join(stateRootDir(getenv), "acp")
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

	bridgeEnabled := false
	if raw.Bridge != nil {
		bridgeEnabled = *raw.Bridge
	}
	if envBridge := strings.TrimSpace(getenv("OPENALBERT_BRIDGE")); envBridge != "" {
		parsed, err := strconv.ParseBool(envBridge)
		if err != nil {
			return Config{}, errors.New("OPENALBERT_BRIDGE must be a boolean")
		}
		bridgeEnabled = parsed
	}

	hostLabel := strings.TrimSpace(raw.HostLabel)
	if hostLabel == "" {
		if hn, err := os.Hostname(); err == nil {
			hostLabel = strings.TrimSpace(hn)
		}
		if hostLabel == "" {
			hostLabel = "unknown-host"
		}
	}

	tickPrompt := strings.TrimSpace(raw.BridgeTickPrompt)
	if tickPrompt == "" {
		tickPrompt = DefaultBridgeTickPrompt
	}

	return Config{
		ServerURL:        serverURL,
		Token:            token,
		DefaultModel:     strings.TrimSpace(raw.DefaultModel),
		SyncInspector:    syncInspector,
		StateDir:         stateDir,
		CustomStateDir:   customStateDir,
		MaxSteps:         maxSteps,
		RequestTimeout:   reqTimeout,
		Bridge:           bridgeEnabled,
		HostLabel:        hostLabel,
		BridgeTickPrompt: tickPrompt,
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
