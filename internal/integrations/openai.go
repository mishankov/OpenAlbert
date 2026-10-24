package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openalbert/internal/domain"
)

type OpenAIConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}
type OpenAI struct {
	config     OpenAIConfig
	client     *http.Client
	opencodeGo bool
	deepSeek   bool
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
	endpoint, _ := url.Parse(base)
	isOpenCodeGo := strings.EqualFold(endpoint.Hostname(), "opencode.ai") && (strings.HasPrefix(endpoint.Path, "/inference/go/") || strings.HasPrefix(endpoint.Path, "/zen/go/"))
	modelID := strings.ToLower(c.Model)
	modelID = modelID[strings.LastIndex(modelID, "/")+1:]
	return &OpenAI{config: c, client: httpClient(c.HTTPClient, 120*time.Second), opencodeGo: isOpenCodeGo, deepSeek: strings.HasPrefix(modelID, "deepseek-")}, nil
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

func (o *OpenAI) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	converted := make([]apiMessage, 0, len(messages))
	for _, m := range messages {
		a := apiMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if o.deepSeek && m.Role == "assistant" {
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
	if len(tools) > 0 {
		defs := make([]any, 0, len(tools))
		for _, t := range tools {
			if !json.Valid(t.Parameters) {
				return domain.Completion{}, errors.New("invalid tool schema")
			}
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
		}
		body["tools"] = defs
	}
	data, err := json.Marshal(body)
	if err != nil {
		return domain.Completion{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.config.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return domain.Completion{}, errors.New("invalid model request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenAlbert/0.1")
	if o.opencodeGo {
		session := domain.ModelSession(ctx)
		if session == "" {
			return domain.Completion{}, errors.New("OpenCode Go requires a conversation session ID")
		}
		req.Header.Set("x-opencode-session", session)
	}
	if o.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.config.APIKey)
	}
	var response struct {
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
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := request(ctx, o.client, req, &response, 8<<20); err != nil {
		return domain.Completion{}, err
	}
	if len(response.Choices) == 0 {
		return domain.Completion{}, errors.New("model returned no choices")
	}
	choice := response.Choices[0]
	if choice.FinishReason == "length" {
		return domain.Completion{}, errors.New("model response truncated by token limit")
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
				return domain.Completion{}, errors.New("invalid model content")
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
			return domain.Completion{}, errors.New("model returned invalid tool call")
		}
		m.ToolCalls = append(m.ToolCalls, domain.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(c.Function.Arguments)})
	}
	if m.Content == "" && len(m.ToolCalls) == 0 && choice.FinishReason != "stop" {
		return domain.Completion{}, errors.New("model returned empty response")
	}
	return domain.Completion{Message: m, InputTokens: response.Usage.Prompt, OutputTokens: response.Usage.Completion}, nil
}

var _ domain.Model = (*OpenAI)(nil)
