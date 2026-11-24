package domain

import (
	"context"
	"sort"
	"time"
)

// MaxModelTimeout leaves 30 seconds inside the three-minute model Activity for
// inspection and durable checkpoints.
const MaxModelTimeout = 150 * time.Second

// ModelRole identifies an application operation, independently of profile names.
type ModelRole string

const (
	RoleChat       ModelRole = "chat"
	RoleBackground ModelRole = "background"
	RoleAnalysis   ModelRole = "analysis"
	RoleCoding     ModelRole = "coding"
)

// ModelProvider defines a configured upstream LLM provider and its defaults.
// Credentials must never be serialized.
type ModelProvider struct {
	Name                       string
	Adapter                    string
	BaseURL                    string
	Headers                    map[string]string `json:"-"`
	APIKeyEnv                  string
	APIKey                     string `json:"-"`
	DiscoverModels             bool
	IncludeModels              []string
	ExcludeModels              []string
	DefaultCapabilities        []string
	DefaultContextWindowTokens int
	DefaultMaxOutputTokens     int
	DefaultTimeout             time.Duration
	DefaultOptions             ModelOptions
	DefaultPricing             *ModelPricing
}

// ModelProfiles is immutable after startup. Credentials must never be serialized.
type ModelProfiles struct {
	Routes    map[ModelRole]string
	Providers map[string]ModelProvider
	Models    map[string]ModelProfile
	Profiles  map[string]ModelProfile
}

// HasSubscription reports whether the configuration needs the shared ChatGPT
// connection. Account credentials stay outside the profile TOML.
func (m ModelProfiles) HasSubscription() bool {
	for _, p := range m.Providers {
		if p.Adapter == "openai_chatgpt" {
			return true
		}
	}
	for _, p := range m.Profiles {
		if p.Adapter == "openai_chatgpt" {
			return true
		}
	}
	return false
}

// SubscriptionCatalogProfile selects the transport for the shared account catalog.
func (m ModelProfiles) SubscriptionCatalogProfile() ModelProfile {
	for _, role := range []ModelRole{RoleChat, RoleCoding, RoleBackground, RoleAnalysis} {
		if p := m.Profiles[m.Routes[role]]; p.Adapter == "openai_chatgpt" {
			return p
		}
	}
	names := make([]string, 0, len(m.Providers))
	for name, p := range m.Providers {
		if p.Adapter == "openai_chatgpt" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		p := m.Providers[names[0]]
		return ModelProfile{
			Provider:            p.Name,
			Adapter:             p.Adapter,
			BaseURL:             p.BaseURL,
			Headers:             p.Headers,
			Capabilities:        p.DefaultCapabilities,
			ContextWindowTokens: p.DefaultContextWindowTokens,
			MaxOutputTokens:     p.DefaultMaxOutputTokens,
			Timeout:             p.DefaultTimeout,
		}
	}
	return ModelProfile{}
}

type ModelProfile struct {
	Provider                  string
	Adapter                   string
	BaseURL                   string
	Headers                   map[string]string `json:"-"`
	Model                     string
	DisplayName               string
	APIKeyEnv                 string
	APIKey                    string `json:"-"`
	Capabilities              []string
	ContextWindowTokens       int
	MaxOutputTokens           int
	Timeout                   time.Duration
	Options                   ModelOptions
	Pricing                   *ModelPricing
	SupportedReasoningEfforts []string
}

type ModelOptions struct {
	ReasoningEffort         string `json:"reasoning_effort,omitempty"`
	Thinking                string `json:"thinking,omitempty"`
	IncludeReasoningContent bool   `json:"include_reasoning_content,omitempty"`
	SessionHeader           string `json:"session_header,omitempty"`
	OutputTokenField        string `json:"output_token_field,omitempty"`
}

// Prices are optional estimates in USD per million tokens, not billing records.
type ModelPricing struct {
	InputPerMillion       float64
	CachedInputPerMillion float64
	OutputPerMillion      float64
}

type ModelUsage struct {
	Role                 ModelRole `json:"role,omitempty"`
	Profile              string    `json:"profile,omitempty"`
	Model                string    `json:"model,omitempty"`
	TokenUsageKnown      bool      `json:"token_usage_known,omitempty"`
	CachedInputTokens    int       `json:"cached_input_tokens,omitempty"`
	EstimatedInputTokens int       `json:"estimated_input_tokens,omitempty"`
	DroppedMessages      int       `json:"dropped_messages,omitempty"`
	EstimatedCostUSD     *float64  `json:"estimated_cost_usd,omitempty"`
}

// StreamEvent represents one incremental event emitted by a streaming completion.
type StreamEvent struct {
	Type       string      `json:"type"` // text_delta, reasoning_delta, tool_call_delta, usage, done, error
	Delta      string      `json:"delta,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolName   string      `json:"tool_name,omitempty"`
	ToolArgs   string      `json:"tool_args,omitempty"`
	Completion *Completion `json:"completion,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// StreamFunc receives incremental completion events during a streaming LLM call.
type StreamFunc func(StreamEvent) error

// CatalogModel describes a selectable model exposed to ACP / coding clients.
type CatalogModel struct {
	ID                        string   `json:"id"`
	Provider                  string   `json:"provider"`
	Model                     string   `json:"model"`
	DisplayName               string   `json:"display_name"`
	Capabilities              []string `json:"capabilities"`
	ContextWindowTokens       int      `json:"context_window_tokens"`
	MaxOutputTokens           int      `json:"max_output_tokens"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	Default                   bool     `json:"default,omitempty"`
}

// ModelContext checks the configured input budget without making a provider call.
type ModelContext interface {
	FitsContext(context.Context, []Message, []Tool) (bool, error)
}
