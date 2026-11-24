package serverclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Message struct {
	TurnID           string            `json:"turn_id,omitempty"`
	ResponsesItems   []json.RawMessage `json:"responses_items,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	Role             string            `json:"role"`
	Content          string            `json:"content"`
	Images           []string          `json:"images,omitempty"`
	ToolCalls        []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	ParentToolCallID string            `json:"parent_tool_call_id,omitempty"`
}

type ModelOptions struct {
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type ModelUsage struct {
	Role                 string   `json:"role,omitempty"`
	Profile              string   `json:"profile,omitempty"`
	Model                string   `json:"model,omitempty"`
	TokenUsageKnown      bool     `json:"token_usage_known,omitempty"`
	CachedInputTokens    int      `json:"cached_input_tokens,omitempty"`
	EstimatedInputTokens int      `json:"estimated_input_tokens,omitempty"`
	DroppedMessages      int      `json:"dropped_messages,omitempty"`
	EstimatedCostUSD     *float64 `json:"estimated_cost_usd,omitempty"`
}

type Completion struct {
	Model        string     `json:"Model"`
	Message      Message    `json:"Message"`
	InputTokens  int        `json:"InputTokens"`
	OutputTokens int        `json:"OutputTokens"`
	Usage        ModelUsage `json:"Usage"`
}

type StreamEvent struct {
	Type       string      `json:"type"`
	Delta      string      `json:"delta,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	ToolName   string      `json:"tool_name,omitempty"`
	ToolArgs   string      `json:"tool_args,omitempty"`
	Completion *Completion `json:"completion,omitempty"`
	Error      string      `json:"error,omitempty"`
}

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

type ModelsResponse struct {
	DefaultModel string         `json:"default_model"`
	Models       []CatalogModel `json:"models"`
}

type ServerSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type AgentContext struct {
	OwnerID     int64         `json:"owner_id"`
	Soul        string        `json:"soul"`
	UserProfile string        `json:"user_profile"`
	Skills      []ServerSkill `json:"skills"`
	Tools       []Tool        `json:"tools"`
}

type CompleteRequest struct {
	Model          string       `json:"model"`
	Options        ModelOptions `json:"options"`
	SessionID      string       `json:"session_id"`
	CorrelationID  string       `json:"correlation_id,omitempty"`
	ConversationID string       `json:"conversation_id,omitempty"`
	TurnID         string       `json:"turn_id"`
	Step           int          `json:"step"`
	Messages       []Message    `json:"messages"`
	Tools          []Tool       `json:"tools"`
}

type ToolRequest struct {
	SessionID        string          `json:"session_id"`
	CorrelationID    string          `json:"correlation_id,omitempty"`
	TurnID           string          `json:"turn_id"`
	Step             int             `json:"step"`
	CallID           string          `json:"call_id"`
	ParentToolCallID string          `json:"parent_tool_call_id,omitempty"`
	Arguments        json.RawMessage `json:"arguments"`
}

type ToolResult struct {
	Output json.RawMessage `json:"output"`
	Failed bool            `json:"failed,omitempty"`
	Images []string        `json:"images,omitempty"`
}

type SyncMessage struct {
	Key     string  `json:"key"`
	Message Message `json:"message"`
}

type Trace struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	Trigger   string          `json:"trigger"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Step      int             `json:"step"`
	CallID    string          `json:"call_id,omitempty"`
	Attempt   int             `json:"attempt"`
	Status    string          `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type SyncRequest struct {
	ConversationID string        `json:"conversation_id,omitempty"`
	Title          string        `json:"title,omitempty"`
	Messages       []SyncMessage `json:"messages"`
	Traces         []Trace       `json:"traces"`
}

// Client talks to the authenticated OpenAlbert server HTTP API.
type Client struct {
	baseURL      string
	token        string
	httpClient   *http.Client
	streamClient *http.Client
}

func New(baseURL, token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		token:        token,
		httpClient:   &http.Client{Timeout: timeout},
		streamClient: &http.Client{Timeout: 0},
	}
}

// APIError reports a structured HTTP error returned by the OpenAlbert server.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("openalbert server error (%d %s): %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("openalbert server returned HTTP %d", e.Status)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "openalbert-acp/0.1")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&apiErr)
		return &APIError{
			Status:  resp.StatusCode,
			Code:    apiErr.Error.Code,
			Message: apiErr.Error.Message,
		}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
	}
	return nil
}

func (c *Client) Models(ctx context.Context, refresh bool) (ModelsResponse, error) {
	path := "/api/v1/llm/models"
	if refresh {
		path += "?refresh=true"
	}
	var out ModelsResponse
	err := c.doJSON(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) AgentContext(ctx context.Context) (AgentContext, error) {
	var out AgentContext
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/agent/context", nil, &out)
	return out, err
}

func (c *Client) ExecuteTool(ctx context.Context, name string, req ToolRequest) (ToolResult, error) {
	var out ToolResult
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/agent/tools/"+url.PathEscape(name), req, &out)
	return out, err
}

func (c *Client) CreateCodingSession(ctx context.Context, sessionID, conversationID, title, cwd string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/coding/sessions", map[string]string{
		"session_id":      sessionID,
		"conversation_id": conversationID,
		"title":           title,
		"cwd":             cwd,
	}, nil)
}

func (c *Client) SyncCodingSession(ctx context.Context, sessionID string, req SyncRequest) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/coding/sessions/"+url.PathEscape(sessionID)+"/sync", req, nil)
}

func (c *Client) CloseCodingSession(ctx context.Context, sessionID string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/coding/sessions/"+url.PathEscape(sessionID)+"/close", map[string]any{}, nil)
}

// CompleteStream streams an LLM turn from OpenAlbert's gateway over SSE.
func (c *Client) CompleteStream(ctx context.Context, payload CompleteRequest, onEvent func(StreamEvent) error) (Completion, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Completion{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/llm/complete", bytes.NewReader(raw))
	if err != nil {
		return Completion{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "openalbert-acp/0.1")

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return Completion{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&apiErr)
		if apiErr.Error.Message != "" {
			return Completion{}, fmt.Errorf("llm gateway error (%d): %s", resp.StatusCode, apiErr.Error.Message)
		}
		return Completion{}, fmt.Errorf("llm gateway returned HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var dataLines []string
	var final *Completion
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		joined := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if joined == "" {
			return nil
		}
		var ev StreamEvent
		if err := json.Unmarshal([]byte(joined), &ev); err != nil {
			return fmt.Errorf("invalid SSE event from OpenAlbert: %w", err)
		}
		switch ev.Type {
		case "error":
			msg := ev.Error
			if msg == "" {
				msg = "upstream LLM request failed"
			}
			return errors.New(msg)
		case "done":
			if ev.Completion != nil {
				cp := *ev.Completion
				final = &cp
			}
		default:
			if onEvent != nil {
				if err := onEvent(ev); err != nil {
					return err
				}
			}
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return Completion{}, err
			}
			if final != nil {
				return *final, nil
			}
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return Completion{}, ctx.Err()
		}
		return Completion{}, fmt.Errorf("LLM stream interrupted: %w", err)
	}
	if err := flush(); err != nil {
		return Completion{}, err
	}
	if final == nil {
		return Completion{}, errors.New("LLM stream ended without completion")
	}
	return *final, nil
}
