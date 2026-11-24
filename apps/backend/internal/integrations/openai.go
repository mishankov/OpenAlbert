package integrations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
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

// DiscoveredModel holds optional metadata parsed from GET /models responses.
type DiscoveredModel struct {
	ID                  string
	DisplayName         string
	ContextWindowTokens int
	MaxOutputTokens     int
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

func (o *OpenAI) requestBody(messages []domain.Message, tools []domain.Tool, stream bool) (map[string]any, error) {
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
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
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
	return body, nil
}

func (o *OpenAI) requestData(messages []domain.Message, tools []domain.Tool) ([]byte, error) {
	body, err := o.requestBody(messages, tools, false)
	if err != nil {
		return nil, err
	}
	return json.Marshal(body)
}

func (o *OpenAI) sanitizeModel(model string) string {
	if key := strings.TrimSpace(o.config.APIKey); key != "" && strings.Contains(model, key) {
		return ""
	}
	for _, value := range o.config.Headers {
		if value != "" && strings.Contains(model, value) {
			return ""
		}
	}
	return model
}

func (o *OpenAI) prepareRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, o.config.BaseURL+endpoint, body)
	if err != nil {
		return nil, errors.New("invalid model request")
	}
	for name, value := range o.config.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("User-Agent", "OpenAlbert/0.1")
	if o.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.config.APIKey)
	}
	return req, nil
}

// ListModels fetches the OpenAI-compatible GET /models catalog from this provider.
func (o *OpenAI) ListModels(ctx context.Context) ([]DiscoveredModel, error) {
	req, err := o.prepareRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	var response struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			DisplayName   string `json:"display_name"`
			ContextLength int    `json:"context_length"`
			ContextWindow int    `json:"context_window"`
			MaxOutput     int    `json:"max_output_tokens"`
			TopProvider   *struct {
				ContextLength       int `json:"context_length"`
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
		} `json:"data"`
	}
	if err := request(ctx, o.client, req, &response, 4<<20); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]DiscoveredModel, 0, len(response.Data))
	for _, item := range response.Data {
		id := strings.TrimSpace(o.sanitizeModel(item.ID))
		if id == "" || seen[id] || strings.ContainsAny(id, "\r\n\x00") {
			continue
		}
		seen[id] = true
		display := strings.TrimSpace(o.sanitizeModel(item.DisplayName))
		if display == "" {
			display = strings.TrimSpace(o.sanitizeModel(item.Name))
		}
		if display == "" {
			display = id
		}
		ctxLen := item.ContextLength
		if ctxLen <= 0 {
			ctxLen = item.ContextWindow
		}
		maxOut := item.MaxOutput
		if item.TopProvider != nil {
			if ctxLen <= 0 {
				ctxLen = item.TopProvider.ContextLength
			}
			if maxOut <= 0 {
				maxOut = item.TopProvider.MaxCompletionTokens
			}
		}
		out = append(out, DiscoveredModel{
			ID:                  id,
			DisplayName:         display,
			ContextWindowTokens: ctxLen,
			MaxOutputTokens:     maxOut,
		})
	}
	return out, nil
}

func (o *OpenAI) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	data, err := o.requestData(messages, tools)
	if err != nil {
		return domain.Completion{}, err
	}
	req, err := o.prepareRequest(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(data))
	if err != nil {
		return domain.Completion{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.config.Options.SessionHeader != "" {
		session := domain.ModelSession(ctx)
		if session == "" {
			return domain.Completion{}, errors.New("model requires a conversation session ID")
		}
		req.Header.Set(o.config.Options.SessionHeader, session)
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
	model := o.sanitizeModel(response.Model)
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

// CompleteStream executes a streaming Chat Completions request, invoking onEvent
// for incremental text, reasoning and tool-call argument deltas while returning
// the assembled final Completion.
func (o *OpenAI) CompleteStream(ctx context.Context, messages []domain.Message, tools []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
	body, err := o.requestBody(messages, tools, true)
	if err != nil {
		return domain.Completion{}, err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return domain.Completion{}, err
	}
	req, err := o.prepareRequest(ctx, http.MethodPost, "/chat/completions", bytes.NewReader(data))
	if err != nil {
		return domain.Completion{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if o.config.Options.SessionHeader != "" {
		session := domain.ModelSession(ctx)
		if session == "" {
			return domain.Completion{}, errors.New("model requires a conversation session ID")
		}
		req.Header.Set(o.config.Options.SessionHeader, session)
	}
	streamClient := *o.client
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return domain.Completion{}, ctx.Err()
		}
		return domain.Completion{}, &RequestError{Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return domain.Completion{}, &HTTPError{Status: resp.StatusCode, Source: "upstream"}
	}

	type partialCall struct {
		id, typ, name string
		args          strings.Builder
	}
	calls := map[int]*partialCall{}
	var textBuf, reasonBuf strings.Builder
	var finishReason, responseModel string
	var promptTokens, completionTokens *int
	var cachedTokens int

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 32<<20))
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var dataLines []string
	processEvent := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		raw := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if raw == "" {
			return false, nil
		}
		if raw == "[DONE]" {
			return true, nil
		}
		var chunk struct {
			Model   string `json:"model"`
			Error   any    `json:"error"`
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content          string `json:"content"`
					Refusal          string `json:"refusal"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt         *int `json:"prompt_tokens"`
				Completion     *int `json:"completion_tokens"`
				PromptCacheHit int  `json:"prompt_cache_hit_tokens"`
				PromptDetails  struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			return false, errors.New("invalid model stream event")
		}
		if chunk.Error != nil {
			return false, errors.New("model stream returned an error")
		}
		if chunk.Model != "" {
			responseModel = chunk.Model
		}
		if chunk.Usage != nil {
			if chunk.Usage.Prompt != nil {
				v := *chunk.Usage.Prompt
				promptTokens = &v
			}
			if chunk.Usage.Completion != nil {
				v := *chunk.Usage.Completion
				completionTokens = &v
			}
			if chunk.Usage.PromptDetails.Cached > cachedTokens {
				cachedTokens = chunk.Usage.PromptDetails.Cached
			}
			if chunk.Usage.PromptCacheHit > cachedTokens {
				cachedTokens = chunk.Usage.PromptCacheHit
			}
		}
		if len(chunk.Choices) > 0 {
			ch := chunk.Choices[0]
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
			reason := ch.Delta.ReasoningContent
			if reason == "" {
				reason = ch.Delta.Reasoning
			}
			if reason != "" {
				reasonBuf.WriteString(reason)
				if onEvent != nil {
					if err := onEvent(domain.StreamEvent{Type: "reasoning_delta", Delta: reason}); err != nil {
						return false, err
					}
				}
			}
			content := ch.Delta.Content
			if content == "" && ch.Delta.Refusal != "" {
				content = ch.Delta.Refusal
			}
			if content != "" {
				textBuf.WriteString(content)
				if onEvent != nil {
					if err := onEvent(domain.StreamEvent{Type: "text_delta", Delta: content}); err != nil {
						return false, err
					}
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				pc := calls[tc.Index]
				if pc == nil {
					pc = &partialCall{}
					calls[tc.Index] = pc
				}
				if tc.ID != "" {
					pc.id = tc.ID
				}
				if tc.Type != "" {
					pc.typ = tc.Type
				}
				if tc.Function.Name != "" {
					pc.name += tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					pc.args.WriteString(tc.Function.Arguments)
				}
				if onEvent != nil && (tc.Function.Name != "" || tc.Function.Arguments != "") {
					if err := onEvent(domain.StreamEvent{
						Type:       "tool_call_delta",
						ToolCallID: pc.id,
						ToolName:   tc.Function.Name,
						ToolArgs:   tc.Function.Arguments,
					}); err != nil {
						return false, err
					}
				}
			}
		}
		return false, nil
	}

	done := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			var err error
			done, err = processEvent()
			if err != nil {
				return domain.Completion{}, err
			}
			if done {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return domain.Completion{}, ctx.Err()
		}
		return domain.Completion{}, errors.New("model stream interrupted")
	}
	if !done {
		var err error
		done, err = processEvent()
		if err != nil {
			return domain.Completion{}, err
		}
	}
	if !done && finishReason == "" {
		return domain.Completion{}, errors.New("model stream ended unexpectedly without completion")
	}

	model := o.sanitizeModel(responseModel)
	if model == "" {
		model = o.config.Model
	}
	out := domain.Completion{Model: o.config.Model, Usage: domain.ModelUsage{Model: model}}
	if promptTokens != nil {
		out.InputTokens = *promptTokens
	}
	if completionTokens != nil {
		out.OutputTokens = *completionTokens
	}
	if out.InputTokens < 0 || out.OutputTokens < 0 {
		out.InputTokens, out.OutputTokens = 0, 0
		return out, errors.New("model returned invalid token usage")
	}
	out.Usage.TokenUsageKnown = promptTokens != nil && completionTokens != nil
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > out.InputTokens {
		cachedTokens = out.InputTokens
	}
	out.Usage.CachedInputTokens = cachedTokens

	if finishReason == "length" {
		return out, errors.New("model response truncated by token limit")
	}
	msg := domain.Message{Role: "assistant", Content: textBuf.String(), Reasoning: reasonBuf.String()}
	if len(calls) > 0 {
		indices := make([]int, 0, len(calls))
		for idx := range calls {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			pc := calls[idx]
			if pc.typ != "" && pc.typ != "function" {
				return out, errors.New("model returned invalid tool call")
			}
			args := strings.TrimSpace(pc.args.String())
			if args == "" {
				args = "{}"
			}
			if pc.id == "" {
				pc.id = fmt.Sprintf("call_%d", idx)
			}
			if pc.name == "" || !json.Valid([]byte(args)) {
				return out, errors.New("model returned invalid tool call")
			}
			msg.ToolCalls = append(msg.ToolCalls, domain.ToolCall{ID: pc.id, Name: pc.name, Arguments: json.RawMessage(args)})
		}
	}
	if msg.Content == "" && len(msg.ToolCalls) == 0 && finishReason != "stop" {
		return out, errors.New("model returned empty response")
	}
	out.Message = msg
	return out, nil
}

var _ domain.Model = (*OpenAI)(nil)
