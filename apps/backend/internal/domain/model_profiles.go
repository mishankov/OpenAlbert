package domain

import (
	"context"
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
)

// ModelProfiles is immutable after startup. Credentials must never be serialized.
type ModelProfiles struct {
	Routes   map[ModelRole]string
	Profiles map[string]ModelProfile
}

type ModelProfile struct {
	Adapter             string
	BaseURL             string
	Model               string
	APIKeyEnv           string
	APIKey              string `json:"-"`
	Capabilities        []string
	ContextWindowTokens int
	MaxOutputTokens     int
	Timeout             time.Duration
	Options             ModelOptions
	Pricing             *ModelPricing
}

type ModelOptions struct {
	ReasoningEffort         string
	Thinking                string
	IncludeReasoningContent bool
	SessionHeader           string
	OutputTokenField        string
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

// ModelContext checks the configured input budget without making a provider call.
type ModelContext interface {
	FitsContext(context.Context, []Message, []Tool) (bool, error)
}
