package config

import (
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
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
	Routes    map[string]string              `toml:"routes"`
	Providers map[string]modelProviderConfig `toml:"providers"`
	Models    map[string]modelOverrideConfig `toml:"models"`
}

type modelProviderConfig struct {
	Adapter                    string             `toml:"adapter"`
	BaseURL                    string             `toml:"base_url"`
	HeadersEnv                 map[string]string  `toml:"headers_env"`
	APIKeyEnv                  string             `toml:"api_key_env"`
	DiscoverModels             *bool              `toml:"discover_models"`
	IncludeModels              []string           `toml:"include_models"`
	ExcludeModels              []string           `toml:"exclude_models"`
	DefaultCapabilities        []string           `toml:"default_capabilities"`
	DefaultContextWindowTokens int                `toml:"default_context_window_tokens"`
	DefaultMaxOutputTokens     int                `toml:"default_max_output_tokens"`
	DefaultTimeout             string             `toml:"default_timeout"`
	DefaultOptions             modelOptionsConfig `toml:"default_options"`
	DefaultPricing             *modelPriceConfig  `toml:"default_pricing"`
}

type modelOverrideConfig struct {
	DisplayName               string              `toml:"display_name"`
	Capabilities              []string            `toml:"capabilities"`
	ContextWindowTokens       *int                `toml:"context_window_tokens"`
	MaxOutputTokens           *int                `toml:"max_output_tokens"`
	Timeout                   string              `toml:"timeout"`
	Options                   *modelOptionsConfig `toml:"options"`
	Pricing                   *modelPriceConfig   `toml:"pricing"`
	SupportedReasoningEfforts []string            `toml:"supported_reasoning_efforts"`
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

// LoadModelProfiles loads a complete provider/model routing configuration with
// credentials from the process environment. Parser source text and configured
// secrets are never included in returned errors.
func LoadModelProfiles(path string) (domain.ModelProfiles, error) {
	return loadModelProfiles(path, os.Getenv)
}

func loadModelProfiles(path string, get func(string) string) (domain.ModelProfiles, error) {
	return loadModelProfilesForAuth(path, get, "profiles")
}

func loadModelProfilesForAuth(filePath string, get func(string) string, authMode string) (domain.ModelProfiles, error) {
	var empty domain.ModelProfiles
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		return empty, errors.New("LLM_CONFIG_FILE must reference a readable regular file")
	}
	if info.Size() > maxModelConfigBytes {
		return empty, errors.New("LLM_CONFIG_FILE exceeds 256 KiB")
	}
	f, err := os.Open(filePath)
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
	out := domain.ModelProfiles{
		Routes:    map[domain.ModelRole]string{},
		Providers: map[string]domain.ModelProvider{},
		Models:    map[string]domain.ModelProfile{},
		Profiles:  map[string]domain.ModelProfile{},
	}
	if len(raw.Providers) == 0 {
		return empty, errors.New("model configuration requires at least one provider")
	}
	for name, provider := range raw.Providers {
		if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "/\r\n\x00") {
			return empty, errors.New("model provider name must be nonempty without slashes, surrounding whitespace or control characters")
		}
		if provider.Adapter == "openai_chatgpt" && authMode != "profiles" {
			return empty, errors.New("openai_chatgpt providers require OPENAI_AUTH_MODE=profiles")
		}
		if provider.Adapter == "openai_chatgpt" {
			for _, field := range []string{"api_key_env", "default_pricing"} {
				if meta.IsDefined("providers", name, field) {
					return empty, errors.New("ChatGPT subscription providers do not accept API keys or per-token pricing")
				}
			}
			if meta.IsDefined("providers", name, "default_options") {
				if !meta.IsDefined("providers", name, "default_options", "reasoning_effort") {
					return empty, errors.New("ChatGPT subscription providers only support reasoning_effort in default_options")
				}
				for _, field := range []string{"thinking", "include_reasoning_content", "session_header", "output_token_field"} {
					if meta.IsDefined("providers", name, "default_options", field) {
						return empty, errors.New("ChatGPT subscription providers only support reasoning_effort in default_options")
					}
				}
			}
		}
		validated, err := validateModelProvider(name, provider, get, authMode == "siwc")
		if err != nil {
			return empty, err
		}
		out.Providers[name] = validated
	}
	for key, override := range raw.Models {
		providerName, modelID, ok := splitModelRef(key)
		if !ok {
			return empty, errors.New("model entry keys must use the provider/model format")
		}
		provider, exists := out.Providers[providerName]
		if !exists {
			return empty, errors.New("model entry references an undefined provider")
		}
		if provider.Adapter == "openai_chatgpt" {
			if meta.IsDefined("models", key, "pricing") {
				return empty, errors.New("ChatGPT subscription models do not accept per-token pricing")
			}
			if meta.IsDefined("models", key, "options") {
				if !meta.IsDefined("models", key, "options", "reasoning_effort") {
					return empty, errors.New("ChatGPT subscription models only support reasoning_effort in options")
				}
				for _, field := range []string{"thinking", "include_reasoning_content", "session_header", "output_token_field"} {
					if meta.IsDefined("models", key, "options", field) {
						return empty, errors.New("ChatGPT subscription models only support reasoning_effort in options")
					}
				}
			}
		}
		resolved, err := resolveModelOverride(provider, modelID, override, meta, key)
		if err != nil {
			return empty, err
		}
		out.Models[key] = resolved
		out.Profiles[key] = resolved
	}
	for role, ref := range raw.Routes {
		r := domain.ModelRole(role)
		if r != domain.RoleChat && r != domain.RoleBackground && r != domain.RoleAnalysis && r != domain.RoleCoding {
			return empty, errors.New("model routes contain an unsupported role")
		}
		providerName, modelID, ok := splitModelRef(ref)
		if !ok {
			return empty, errors.New("model route must reference provider/model")
		}
		provider, exists := out.Providers[providerName]
		if !exists {
			return empty, errors.New("model route must reference a defined provider")
		}
		profile, hasOverride := out.Models[ref]
		if !hasOverride {
			profile = ProfileFromProvider(provider, modelID)
		}
		required := "tools"
		if r == domain.RoleAnalysis {
			required = "structured_output"
		}
		if !hasCapability(profile.Capabilities, required) {
			return empty, errors.New("model route lacks a required capability")
		}
		out.Routes[r] = ref
		out.Profiles[ref] = profile
	}
	for _, required := range []domain.ModelRole{domain.RoleChat, domain.RoleBackground, domain.RoleAnalysis} {
		if out.Routes[required] == "" {
			return empty, errors.New("model routes must explicitly define chat, background and analysis")
		}
	}
	return out, nil
}

// ProfileFromProvider materializes a ModelProfile from a provider's defaults.
func ProfileFromProvider(provider domain.ModelProvider, modelID string) domain.ModelProfile {
	var pricing *domain.ModelPricing
	if provider.DefaultPricing != nil {
		p := *provider.DefaultPricing
		pricing = &p
	}
	return domain.ModelProfile{
		Provider:            provider.Name,
		Adapter:             provider.Adapter,
		BaseURL:             provider.BaseURL,
		Headers:             maps.Clone(provider.Headers),
		Model:               modelID,
		DisplayName:         modelID,
		APIKeyEnv:           provider.APIKeyEnv,
		APIKey:              provider.APIKey,
		Capabilities:        append([]string(nil), provider.DefaultCapabilities...),
		ContextWindowTokens: provider.DefaultContextWindowTokens,
		MaxOutputTokens:     provider.DefaultMaxOutputTokens,
		Timeout:             provider.DefaultTimeout,
		Options:             provider.DefaultOptions,
		Pricing:             pricing,
	}
}

func splitModelRef(ref string) (provider, model string, ok bool) {
	provider, model, ok = strings.Cut(ref, "/")
	if !ok || provider == "" || model == "" || provider != strings.TrimSpace(provider) || model != strings.TrimSpace(model) || strings.ContainsAny(ref, "\r\n\x00") {
		return "", "", false
	}
	return provider, model, true
}

// TOML struct decoding accepts case-insensitive aliases. Require exact schema
// tags so distinct TOML keys cannot compete to populate the same Go field.
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

func validateModelProvider(name string, raw modelProviderConfig, get func(string) string, allowMissingKey bool) (domain.ModelProvider, error) {
	var empty domain.ModelProvider
	if raw.Adapter != "openai_chat_completions" && raw.Adapter != "openai_chatgpt" {
		return empty, errors.New("model provider adapter is unsupported")
	}
	if raw.BaseURL != "" || raw.Adapter == "openai_chat_completions" {
		u, err := url.Parse(raw.BaseURL)
		if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw.BaseURL, "#") {
			return empty, errors.New("model provider base_url must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	if len(raw.HeadersEnv) > 0 && raw.BaseURL == "" {
		return empty, errors.New("model provider headers_env requires an explicit base_url")
	}
	opts, err := validateModelOptions(raw.Adapter, raw.DefaultOptions)
	if err != nil {
		return empty, err
	}
	headers, err := resolveModelHeaders(raw.HeadersEnv, opts.SessionHeader, get)
	if err != nil {
		return empty, err
	}
	key := ""
	if raw.Adapter == "openai_chat_completions" {
		if !environmentName.MatchString(raw.APIKeyEnv) {
			return empty, errors.New("model provider api_key_env must name an environment variable")
		}
		key = get(raw.APIKeyEnv)
		if strings.TrimSpace(key) == "" && !allowMissingKey {
			return empty, errors.New("model provider credential environment variable is unset")
		}
	}
	if err := validatePatterns(raw.IncludeModels); err != nil {
		return empty, err
	}
	if err := validatePatterns(raw.ExcludeModels); err != nil {
		return empty, err
	}
	caps, err := validateCapabilities(raw.DefaultCapabilities)
	if err != nil {
		return empty, err
	}
	if raw.DefaultContextWindowTokens <= 0 || raw.DefaultMaxOutputTokens <= 0 || raw.DefaultMaxOutputTokens >= raw.DefaultContextWindowTokens {
		return empty, errors.New("model provider requires positive context/output token limits with output smaller than context")
	}
	timeout, err := time.ParseDuration(raw.DefaultTimeout)
	if err != nil || timeout <= 0 || timeout > domain.MaxModelTimeout {
		return empty, errors.New("model provider default_timeout must be a positive duration no greater than 150s")
	}
	pricing, err := validatePricing(raw.DefaultPricing)
	if err != nil {
		return empty, err
	}
	discoverModels := true
	if raw.DiscoverModels != nil {
		discoverModels = *raw.DiscoverModels
	}
	return domain.ModelProvider{
		Name:                       name,
		Adapter:                    raw.Adapter,
		BaseURL:                    strings.TrimRight(raw.BaseURL, "/"),
		Headers:                    headers,
		APIKeyEnv:                  raw.APIKeyEnv,
		APIKey:                     key,
		DiscoverModels:             discoverModels,
		IncludeModels:              append([]string(nil), raw.IncludeModels...),
		ExcludeModels:              append([]string(nil), raw.ExcludeModels...),
		DefaultCapabilities:        caps,
		DefaultContextWindowTokens: raw.DefaultContextWindowTokens,
		DefaultMaxOutputTokens:     raw.DefaultMaxOutputTokens,
		DefaultTimeout:             timeout,
		DefaultOptions:             opts,
		DefaultPricing:             pricing,
	}, nil
}

func resolveModelOverride(provider domain.ModelProvider, modelID string, raw modelOverrideConfig, meta toml.MetaData, key string) (domain.ModelProfile, error) {
	p := ProfileFromProvider(provider, modelID)
	if strings.TrimSpace(raw.DisplayName) != "" {
		if strings.ContainsAny(raw.DisplayName, "\r\n\x00") {
			return domain.ModelProfile{}, errors.New("model display_name must not contain control characters")
		}
		p.DisplayName = strings.TrimSpace(raw.DisplayName)
	}
	if meta.IsDefined("models", key, "capabilities") {
		caps, err := validateCapabilities(raw.Capabilities)
		if err != nil {
			return domain.ModelProfile{}, err
		}
		p.Capabilities = caps
	}
	if raw.ContextWindowTokens != nil {
		p.ContextWindowTokens = *raw.ContextWindowTokens
	}
	if raw.MaxOutputTokens != nil {
		p.MaxOutputTokens = *raw.MaxOutputTokens
	}
	if p.ContextWindowTokens <= 0 || p.MaxOutputTokens <= 0 || p.MaxOutputTokens >= p.ContextWindowTokens {
		return domain.ModelProfile{}, errors.New("model requires positive context/output token limits with output smaller than context")
	}
	if raw.Timeout != "" {
		timeout, err := time.ParseDuration(raw.Timeout)
		if err != nil || timeout <= 0 || timeout > domain.MaxModelTimeout {
			return domain.ModelProfile{}, errors.New("model timeout must be a positive duration no greater than 150s")
		}
		p.Timeout = timeout
	}
	if raw.Options != nil {
		merged := modelOptionsConfig{
			ReasoningEffort:         p.Options.ReasoningEffort,
			Thinking:                p.Options.Thinking,
			IncludeReasoningContent: p.Options.IncludeReasoningContent,
			SessionHeader:           p.Options.SessionHeader,
			OutputTokenField:        p.Options.OutputTokenField,
		}
		if meta.IsDefined("models", key, "options", "reasoning_effort") {
			merged.ReasoningEffort = raw.Options.ReasoningEffort
		}
		if meta.IsDefined("models", key, "options", "thinking") {
			merged.Thinking = raw.Options.Thinking
		}
		if meta.IsDefined("models", key, "options", "include_reasoning_content") {
			merged.IncludeReasoningContent = raw.Options.IncludeReasoningContent
		}
		if meta.IsDefined("models", key, "options", "session_header") {
			merged.SessionHeader = raw.Options.SessionHeader
		}
		if meta.IsDefined("models", key, "options", "output_token_field") {
			merged.OutputTokenField = raw.Options.OutputTokenField
		}
		opts, err := validateModelOptions(provider.Adapter, merged)
		if err != nil {
			return domain.ModelProfile{}, err
		}
		if opts.SessionHeader != "" {
			for headerName := range provider.Headers {
				if strings.EqualFold(headerName, opts.SessionHeader) {
					return domain.ModelProfile{}, errors.New("model options.session_header conflicts with provider headers_env")
				}
			}
		}
		p.Options = opts
	}
	if raw.Pricing != nil {
		pricing, err := validatePricing(raw.Pricing)
		if err != nil {
			return domain.ModelProfile{}, err
		}
		p.Pricing = pricing
	}
	if len(raw.SupportedReasoningEfforts) > 0 {
		seen := map[string]bool{}
		for _, effort := range raw.SupportedReasoningEfforts {
			if !validReasoningEffort(effort) || effort == "" || seen[effort] {
				return domain.ModelProfile{}, errors.New("model supported_reasoning_efforts contains invalid or duplicate values")
			}
			seen[effort] = true
		}
		p.SupportedReasoningEfforts = append([]string(nil), raw.SupportedReasoningEfforts...)
	}
	return p, nil
}

func validatePatterns(patterns []string) error {
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" || strings.ContainsAny(pattern, "\r\n\x00") {
			return errors.New("model filter pattern must be nonempty")
		}
		if _, err := path.Match(pattern, "sample"); err != nil {
			return errors.New("model filter pattern has invalid glob syntax")
		}
	}
	return nil
}

func validateCapabilities(raw []string) ([]string, error) {
	seen := map[string]bool{}
	for _, capability := range raw {
		if (capability != "tools" && capability != "vision" && capability != "structured_output") || seen[capability] {
			return nil, errors.New("model capabilities must contain unique supported names")
		}
		seen[capability] = true
	}
	if len(seen) == 0 {
		return nil, errors.New("model requires capabilities")
	}
	return append([]string(nil), raw...), nil
}

func validReasoningEffort(effort string) bool {
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}

func validateModelOptions(adapter string, o modelOptionsConfig) (domain.ModelOptions, error) {
	if !validReasoningEffort(o.ReasoningEffort) {
		return domain.ModelOptions{}, errors.New("model reasoning_effort is unsupported")
	}
	if adapter == "openai_chatgpt" {
		if o.Thinking != "" || o.IncludeReasoningContent || o.SessionHeader != "" || o.OutputTokenField != "" {
			return domain.ModelOptions{}, errors.New("ChatGPT subscription models only support reasoning_effort option")
		}
		return domain.ModelOptions{ReasoningEffort: o.ReasoningEffort}, nil
	}
	if o.Thinking != "" && o.Thinking != "enabled" && o.Thinking != "disabled" {
		return domain.ModelOptions{}, errors.New("model thinking must be enabled or disabled")
	}
	if o.SessionHeader != "" && o.SessionHeader != "x-opencode-session" {
		return domain.ModelOptions{}, errors.New("model session_header is unsupported")
	}
	if adapter == "openai_chat_completions" && o.OutputTokenField == "" {
		o.OutputTokenField = "max_tokens"
	}
	if o.OutputTokenField != "" && o.OutputTokenField != "max_tokens" && o.OutputTokenField != "max_completion_tokens" {
		return domain.ModelOptions{}, errors.New("model output_token_field is unsupported")
	}
	return domain.ModelOptions{
		ReasoningEffort:         o.ReasoningEffort,
		Thinking:                o.Thinking,
		IncludeReasoningContent: o.IncludeReasoningContent,
		SessionHeader:           o.SessionHeader,
		OutputTokenField:        o.OutputTokenField,
	}, nil
}

func validatePricing(raw *modelPriceConfig) (*domain.ModelPricing, error) {
	if raw == nil {
		return nil, nil
	}
	for _, price := range []*float64{raw.InputPerMillion, raw.CachedInputPerMillion, raw.OutputPerMillion} {
		if price == nil || math.IsNaN(*price) || math.IsInf(*price, 0) || *price < 0 {
			return nil, errors.New("model pricing requires finite nonnegative input, cached-input and output USD prices")
		}
	}
	return &domain.ModelPricing{
		InputPerMillion:       *raw.InputPerMillion,
		CachedInputPerMillion: *raw.CachedInputPerMillion,
		OutputPerMillion:      *raw.OutputPerMillion,
	}, nil
}

func resolveModelHeaders(refs map[string]string, sessionHeader string, get func(string) string) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(refs))
	for name, ref := range refs {
		if !httpguts.ValidHeaderFieldName(name) || !environmentName.MatchString(ref) {
			return nil, errors.New("model provider headers_env must map HTTP header names to environment variable names")
		}
		name = http.CanonicalHeaderKey(name)
		switch name {
		case "Authorization", "Host", "Content-Length", "Content-Type", "Accept", "Accept-Encoding", "User-Agent",
			"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
			"Te", "Trailer", "Transfer-Encoding", "Upgrade":
			return nil, errors.New("model provider headers_env cannot set managed HTTP headers")
		}
		if strings.EqualFold(name, sessionHeader) {
			return nil, errors.New("model provider headers_env cannot override default_options.session_header")
		}
		if _, exists := headers[name]; exists {
			return nil, errors.New("model provider headers_env cannot contain duplicate HTTP headers")
		}
		value := get(ref)
		if !httpguts.ValidHeaderFieldValue(value) || strings.TrimSpace(value) == "" {
			return nil, errors.New("model provider headers_env references an unset or invalid HTTP header value")
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
