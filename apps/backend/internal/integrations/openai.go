package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
)

type OpenAIConfig struct {
	BaseURL         string
	APIKey          string
	Headers         map[string]string `json:"-"`
	Model           string
	HTTPClient      *http.Client
	Timeout         time.Duration
	MaxOutputTokens int
	Options         domain.ModelOptions
	JSONOutput      bool
}
type OpenAI struct {
	config OpenAIConfig
	client *http.Client
}

func NewOpenAI(c OpenAIConfig) (*OpenAI, error) {
	if c.BaseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
	}
	base, err := providerURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	c.BaseURL = base
	if strings.TrimSpace(c.Model) == "" {
		return nil, errors.New("LLM model is required")
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
	if c.Options.OutputTokenField == "" {
		c.Options.OutputTokenField = "max_tokens"
	}
	if c.Options.OutputTokenField != "max_tokens" && c.Options.OutputTokenField != "max_completion_tokens" {
		return nil, errors.New("invalid output token field")
	}
	if c.Options.SessionHeader != "" && c.Options.SessionHeader != "x-opencode-session" {
		return nil, errors.New("unsupported model session header")
	}
	c.Headers = maps.Clone(c.Headers)
	return &OpenAI{config: c, client: httpClient(c.HTTPClient, c.Timeout)}, nil
}

type apiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type apiMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"`
	ToolCalls        []apiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

func (o *OpenAI) requestData(messages []domain.Message, tools []domain.Tool) ([]byte, error) {
	converted := make([]apiMessage, 0, len(messages))
	for _, m := range messages {
		a := apiMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if o.config.Options.IncludeReasoningContent && m.Role == "assistant" {
			// DeepSeek thinking with tools requires this field on every assistant
			// message, including text-only replies and empty reasoning. Keep it
			// separate from user-facing content and omit it for other models.
			a.ReasoningContent = &m.Reasoning
		}
		if len(m.Images) > 0 {
			parts := []any{map[string]any{"type": "text", "text": m.Content}}
			for _, u := range m.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}})
			}
			a.Content = parts
		}
		for _, call := range m.ToolCalls {
			c := apiToolCall{ID: call.ID, Type: "function"}
			c.Function.Name = call.Name
			c.Function.Arguments = string(call.Arguments)
			a.ToolCalls = append(a.ToolCalls, c)
		}
		converted = append(converted, a)
	}
	body := map[string]any{"model": o.config.Model, "messages": converted}
	if o.config.MaxOutputTokens > 0 {
		body[o.config.Options.OutputTokenField] = o.config.MaxOutputTokens
	}
	if o.config.Options.ReasoningEffort != "" {
		body["reasoning_effort"] = o.config.Options.ReasoningEffort
	}
	if o.config.Options.Thinking != "" {
		body["thinking"] = map[string]string{"type": o.config.Options.Thinking}
	}
	if o.config.JSONOutput {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if len(tools) > 0 {
		defs := make([]any, 0, len(tools))
		for _, t := range tools {
			if !json.Valid(t.Parameters) {
				return nil, errors.New("invalid tool schema")
			}
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
		}
		body["tools"] = defs
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (o *OpenAI) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	data, err := o.requestData(messages, tools)
	if err != nil {
		return domain.Completion{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.config.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return domain.Completion{}, errors.New("invalid model request")
	}
	for name, value := range o.config.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenAlbert/0.1")
	if o.config.Options.SessionHeader != "" {
		session := domain.ModelSession(ctx)
		if session == "" {
			return domain.Completion{}, errors.New("model requires a conversation session ID")
		}
		req.Header.Set(o.config.Options.SessionHeader, session)
	}
	if o.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.config.APIKey)
	}
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string          `json:"role"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
				Content          json.RawMessage `json:"content"`
				ToolCalls        []apiToolCall   `json:"tool_calls"`
				Refusal          string          `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt         *int `json:"prompt_tokens"`
			Completion     *int `json:"completion_tokens"`
			PromptCacheHit int  `json:"prompt_cache_hit_tokens"`
			PromptDetails  struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := request(ctx, o.client, req, &response, 8<<20); err != nil {
		return domain.Completion{}, err
	}
	model := response.Model
	// The provider model is logged as usage metadata. A proxy can echo credentials.
	if key := strings.TrimSpace(o.config.APIKey); key != "" && strings.Contains(model, key) {
		model = ""
	}
	for _, value := range o.config.Headers {
		if value != "" && strings.Contains(model, value) {
			model = ""
			break
		}
	}
	if model == "" {
		model = o.config.Model
	}
	out := domain.Completion{Usage: domain.ModelUsage{Model: model}}
	if response.Usage.Prompt != nil {
		out.InputTokens = *response.Usage.Prompt
	}
	if response.Usage.Completion != nil {
		out.OutputTokens = *response.Usage.Completion
	}
	if out.InputTokens < 0 || out.OutputTokens < 0 {
		out.InputTokens, out.OutputTokens = 0, 0
		return out, errors.New("model returned invalid token usage")
	}
	out.Usage.TokenUsageKnown = response.Usage.Prompt != nil && response.Usage.Completion != nil
	cached := response.Usage.PromptDetails.Cached
	if response.Usage.PromptCacheHit > cached {
		cached = response.Usage.PromptCacheHit
	}
	if cached < 0 {
		cached = 0
	}
	if cached > out.InputTokens {
		cached = out.InputTokens
	}
	out.Usage.CachedInputTokens = cached
	if len(response.Choices) == 0 {
		return out, errors.New("model returned no choices")
	}
	choice := response.Choices[0]
	if choice.FinishReason == "length" {
		return out, errors.New("model response truncated by token limit")
	}
	m := domain.Message{Role: "assistant", Reasoning: choice.Message.ReasoningContent}
	if m.Reasoning == "" {
		m.Reasoning = choice.Message.Reasoning
	}
	if len(choice.Message.Content) > 0 && string(choice.Message.Content) != "null" {
		if json.Unmarshal(choice.Message.Content, &m.Content) != nil {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(choice.Message.Content, &parts) != nil {
				return out, errors.New("invalid model content")
			}
			for _, p := range parts {
				if p.Type == "text" {
					m.Content += p.Text
				}
			}
		}
	}
	if m.Content == "" && choice.Message.Refusal != "" {
		m.Content = choice.Message.Refusal
	}
	for _, c := range choice.Message.ToolCalls {
		if c.Type != "function" || c.ID == "" || c.Function.Name == "" || !json.Valid([]byte(c.Function.Arguments)) {
			return out, errors.New("model returned invalid tool call")
		}
		m.ToolCalls = append(m.ToolCalls, domain.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(c.Function.Arguments)})
	}
	if m.Content == "" && len(m.ToolCalls) == 0 && choice.FinishReason != "stop" {
		return out, errors.New("model returned empty response")
	}
	out.Model = o.config.Model
	out.Message = m
	return out, nil
}

var _ domain.Model = (*OpenAI)(nil)
