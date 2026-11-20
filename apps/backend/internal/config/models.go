package config

import (
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/net/http/httpguts"
	"openalbert/apps/backend/internal/domain"
)

const maxModelConfigBytes = 256 << 10

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type modelFileConfig struct {
	Routes   map[string]string             `toml:"routes"`
	Profiles map[string]modelProfileConfig `toml:"profiles"`
}

type modelProfileConfig struct {
	Adapter             string             `toml:"adapter"`
	BaseURL             string             `toml:"base_url"`
	HeadersEnv          map[string]string  `toml:"headers_env"`
	Model               string             `toml:"model"`
	APIKeyEnv           string             `toml:"api_key_env"`
	Capabilities        []string           `toml:"capabilities"`
	ContextWindowTokens int                `toml:"context_window_tokens"`
	MaxOutputTokens     int                `toml:"max_output_tokens"`
	Timeout             string             `toml:"timeout"`
	Options             modelOptionsConfig `toml:"options"`
	Pricing             *modelPriceConfig  `toml:"pricing"`
}

type modelOptionsConfig struct {
	ReasoningEffort         string `toml:"reasoning_effort"`
	Thinking                string `toml:"thinking"`
	IncludeReasoningContent bool   `toml:"include_reasoning_content"`
	SessionHeader           string `toml:"session_header"`
	OutputTokenField        string `toml:"output_token_field"`
}

type modelPriceConfig struct {
	InputPerMillion       *float64 `toml:"input_per_million"`
	CachedInputPerMillion *float64 `toml:"cached_input_per_million"`
	OutputPerMillion      *float64 `toml:"output_per_million"`
}

// LoadModelProfiles loads a complete routing configuration with credentials from
// the process environment. Parser source text and configured secrets are never
// included in returned errors.
func LoadModelProfiles(path string) (domain.ModelProfiles, error) {
	return loadModelProfiles(path, os.Getenv)
}

func loadModelProfiles(path string, get func(string) string) (domain.ModelProfiles, error) {
	return loadModelProfilesForAuth(path, get, "profiles")
}

func loadModelProfilesForAuth(path string, get func(string) string, authMode string) (domain.ModelProfiles, error) {
	var empty domain.ModelProfiles
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return empty, errors.New("LLM_CONFIG_FILE must reference a readable regular file")
	}
	if info.Size() > maxModelConfigBytes {
		return empty, errors.New("LLM_CONFIG_FILE exceeds 256 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return empty, errors.New("LLM_CONFIG_FILE could not be opened")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxModelConfigBytes+1))
	if err != nil || len(data) > maxModelConfigBytes {
		return empty, errors.New("LLM_CONFIG_FILE could not be read within its size limit")
	}
	var raw modelFileConfig
	meta, err := toml.Decode(string(data), &raw)
	if err != nil {
		return empty, errors.New("LLM_CONFIG_FILE contains invalid TOML")
	}
	if len(meta.Undecoded()) > 0 || !exactModelConfigKeys(meta) {
		return empty, errors.New("LLM_CONFIG_FILE contains unsupported fields")
	}
	out := domain.ModelProfiles{Routes: map[domain.ModelRole]string{}, Profiles: map[string]domain.ModelProfile{}}
	if len(raw.Profiles) == 0 {
		return empty, errors.New("model configuration requires at least one profile")
	}
	for name, profile := range raw.Profiles {
		if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\r\n\x00") {
			return empty, errors.New("model profile name must be nonempty without surrounding whitespace or control characters")
		}
		if profile.Adapter == "openai_chatgpt" && authMode != "profiles" {
			return empty, errors.New("openai_chatgpt profiles require OPENAI_AUTH_MODE=profiles")
		}
		if profile.Adapter == "openai_chatgpt" {
			for _, field := range []string{"api_key_env", "options", "pricing"} {
				if meta.IsDefined("profiles", name, field) {
					return empty, errors.New("ChatGPT subscription profiles do not accept API keys, provider options or per-token pricing")
				}
			}
		}
		validated, err := validateModelProfile(profile, get, authMode == "siwc")
		if err != nil {
			return empty, err
		}
		out.Profiles[name] = validated
	}
	for role, name := range raw.Routes {
		r := domain.ModelRole(role)
		if r != domain.RoleChat && r != domain.RoleBackground && r != domain.RoleAnalysis {
			return empty, errors.New("model routes contain an unsupported role")
		}
		profile, ok := out.Profiles[name]
		if !ok {
			return empty, errors.New("model route must reference a defined profile")
		}
		required := "tools"
		if r == domain.RoleAnalysis {
			required = "structured_output"
		}
		if !hasCapability(profile.Capabilities, required) {
			return empty, errors.New("model route profile lacks a required capability")
		}
		out.Routes[r] = name
	}
	if len(out.Routes) != 3 {
		return empty, errors.New("model routes must explicitly define chat, background and analysis")
	}
	return out, nil
}

// TOML struct decoding accepts case-insensitive aliases. Require exact schema
// tags so distinct TOML keys cannot compete to populate the same Go field.
// Map keys (profile names and routes) keep their own validation below.
func exactModelConfigKeys(meta toml.MetaData) bool {
	for _, key := range meta.Keys() {
		typ := reflect.TypeOf(modelFileConfig{})
		for _, part := range key {
			if typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
			switch typ.Kind() {
			case reflect.Map:
				typ = typ.Elem()
			case reflect.Struct:
				var field reflect.Type
				for i := 0; i < typ.NumField(); i++ {
					if f := typ.Field(i); f.Tag.Get("toml") == part {
						field = f.Type
						break
					}
				}
				if field == nil {
					return false
				}
				typ = field
			default:
				return false
			}
		}
	}
	return true
}

func validateModelProfile(raw modelProfileConfig, get func(string) string, allowMissingKey bool) (domain.ModelProfile, error) {
	var empty domain.ModelProfile
	if raw.Adapter != "openai_chat_completions" && raw.Adapter != "openai_chatgpt" {
		return empty, errors.New("model profile adapter is unsupported")
	}
	if strings.TrimSpace(raw.Model) == "" || raw.Model != strings.TrimSpace(raw.Model) {
		return empty, errors.New("model profile model must be a nonempty identifier without surrounding whitespace")
	}
	if raw.BaseURL != "" || raw.Adapter == "openai_chat_completions" {
		u, err := url.Parse(raw.BaseURL)
		if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw.BaseURL, "#") {
			return empty, errors.New("model profile base_url must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	if len(raw.HeadersEnv) > 0 && raw.BaseURL == "" {
		return empty, errors.New("model profile headers_env requires an explicit base_url")
	}
	headers, err := resolveModelHeaders(raw.HeadersEnv, raw.Options.SessionHeader, get)
	if err != nil {
		return empty, err
	}
	key := ""
	if raw.Adapter == "openai_chat_completions" {
		if !environmentName.MatchString(raw.APIKeyEnv) {
			return empty, errors.New("model profile api_key_env must name an environment variable")
		}
		key = get(raw.APIKeyEnv)
		if strings.TrimSpace(key) == "" && !allowMissingKey {
			return empty, errors.New("model profile credential environment variable is unset")
		}
	}
	if raw.ContextWindowTokens <= 0 || raw.MaxOutputTokens <= 0 || raw.MaxOutputTokens >= raw.ContextWindowTokens {
		return empty, errors.New("model profile requires positive context/output token limits with output smaller than context")
	}
	timeout, err := time.ParseDuration(raw.Timeout)
	if err != nil || timeout <= 0 || timeout > domain.MaxModelTimeout {
		return empty, errors.New("model profile timeout must be a positive duration no greater than 150s")
	}
	seen := map[string]bool{}
	for _, capability := range raw.Capabilities {
		if (capability != "tools" && capability != "vision" && capability != "structured_output") || seen[capability] {
			return empty, errors.New("model profile capabilities must contain unique supported names")
		}
		seen[capability] = true
	}
	if len(seen) == 0 {
		return empty, errors.New("model profile requires capabilities")
	}
	o := raw.Options
	switch o.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return empty, errors.New("model profile reasoning_effort is unsupported")
	}
	if o.Thinking != "" && o.Thinking != "enabled" && o.Thinking != "disabled" {
		return empty, errors.New("model profile thinking must be enabled or disabled")
	}
	if o.SessionHeader != "" && o.SessionHeader != "x-opencode-session" {
		return empty, errors.New("model profile session_header is unsupported")
	}
	if raw.Adapter == "openai_chat_completions" && o.OutputTokenField == "" {
		o.OutputTokenField = "max_tokens"
	}
	if o.OutputTokenField != "" && o.OutputTokenField != "max_tokens" && o.OutputTokenField != "max_completion_tokens" {
		return empty, errors.New("model profile output_token_field is unsupported")
	}
	profile := domain.ModelProfile{
		Adapter: raw.Adapter, BaseURL: strings.TrimRight(raw.BaseURL, "/"), Headers: headers, Model: raw.Model,
		APIKeyEnv: raw.APIKeyEnv, APIKey: key, Capabilities: append([]string(nil), raw.Capabilities...),
		ContextWindowTokens: raw.ContextWindowTokens, MaxOutputTokens: raw.MaxOutputTokens, Timeout: timeout,
		Options: domain.ModelOptions{ReasoningEffort: o.ReasoningEffort, Thinking: o.Thinking,
			IncludeReasoningContent: o.IncludeReasoningContent, SessionHeader: o.SessionHeader, OutputTokenField: o.OutputTokenField},
	}
	if raw.Pricing != nil {
		p := raw.Pricing
		for _, price := range []*float64{p.InputPerMillion, p.CachedInputPerMillion, p.OutputPerMillion} {
			if price == nil || math.IsNaN(*price) || math.IsInf(*price, 0) || *price < 0 {
				return empty, errors.New("model profile pricing requires finite nonnegative input, cached-input and output USD prices")
			}
		}
		profile.Pricing = &domain.ModelPricing{InputPerMillion: *p.InputPerMillion, CachedInputPerMillion: *p.CachedInputPerMillion, OutputPerMillion: *p.OutputPerMillion}
	}
	return profile, nil
}

func resolveModelHeaders(refs map[string]string, sessionHeader string, get func(string) string) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(refs))
	for name, ref := range refs {
		if !httpguts.ValidHeaderFieldName(name) || !environmentName.MatchString(ref) {
			return nil, errors.New("model profile headers_env must map HTTP header names to environment variable names")
		}
		name = http.CanonicalHeaderKey(name)
		switch name {
		case "Authorization", "Host", "Content-Length", "Content-Type", "Accept", "Accept-Encoding", "User-Agent",
			"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade":
			return nil, errors.New("model profile headers_env cannot set managed HTTP headers")
		}
		if strings.EqualFold(name, sessionHeader) {
			return nil, errors.New("model profile headers_env cannot override options.session_header")
		}
		if _, exists := headers[name]; exists {
			return nil, errors.New("model profile headers_env cannot contain duplicate HTTP headers")
		}
		value := get(ref)
		if !httpguts.ValidHeaderFieldValue(value) || strings.TrimSpace(value) == "" {
			return nil, errors.New("model profile headers_env references an unset or invalid HTTP header value")
		}
		headers[name] = strings.Trim(value, " \t")
	}
	return headers, nil
}

func hasCapability(capabilities []string, required string) bool {
	for _, capability := range capabilities {
		if capability == required {
			return true
		}
	}
	return false
}
