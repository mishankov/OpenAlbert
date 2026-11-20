package siwc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"openalbert/apps/backend/internal/domain"
)

// An allowlisted request shape prevents accidentally carrying Chat Completions
// parameters (or unsupported Responses fields) into the subscription route.
type responseRequest struct {
	Include []string `json:"include"`
	Model   string   `json:"model"`
	Input   []any    `json:"input"`
	Store   bool     `json:"store"`
	Stream  bool     `json:"stream"`
	Tools   []any    `json:"tools,omitempty"`
}

func buildRequest(model string, messages []domain.Message, tools []domain.Tool) (responseRequest, error) {
	r := responseRequest{Model: model, Input: []any{}, Stream: true, Include: []string{"reasoning.encrypted_content"}}
	for _, m := range messages {
		if m.Role == "assistant" && len(m.ResponsesItems) > 0 {
			for _, item := range m.ResponsesItems {
				r.Input = append(r.Input, item)
			}
			continue
		}
		if m.Role == "tool" {
			r.Input = append(r.Input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		role := m.Role
		if role == "system" {
			role = "developer"
		}
		if role != "developer" && role != "user" && role != "assistant" {
			return r, errors.New("unsupported ChatGPT message role")
		}
		if m.Content != "" || len(m.Images) > 0 {
			var content any = m.Content
			if len(m.Images) > 0 {
				parts := []any{map[string]any{"type": "input_text", "text": m.Content}}
				for _, image := range m.Images {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": image})
				}
				content = parts
			}
			r.Input = append(r.Input, map[string]any{"role": role, "content": content})
		}
		for _, call := range m.ToolCalls {
			r.Input = append(r.Input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "namespace": "openalbert", "arguments": string(call.Arguments)})
		}
	}
	if len(tools) > 0 {
		defs := []any{}
		for _, t := range tools {
			if !json.Valid(t.Parameters) {
				return r, errors.New("invalid tool schema")
			}
			defs = append(defs, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters, "strict": false})
		}
		r.Tools = []any{map[string]any{"type": "namespace", "name": "openalbert", "description": "OpenAlbert local tools", "tools": defs}}
	}
	return r, nil
}
func (c *Client) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	c.mu.Lock()
	token, err := c.accessToken(ctx)
	model := c.creds.Model
	c.mu.Unlock()
	if err != nil {
		return domain.Completion{}, err
	}
	body, err := buildRequest(model, messages, tools)
	if err != nil {
		return domain.Completion{}, err
	}
	data, err := json.Marshal(body)
	if err != nil {
		return domain.Completion{}, errors.New("invalid ChatGPT request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/responses", bytes.NewReader(data))
	if err != nil {
		return domain.Completion{}, errors.New("invalid ChatGPT request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "OpenAlbert/0.1")
	resp, err := c.do(req)
	if err != nil {
		return domain.Completion{}, err
	}
	defer resp.Body.Close()
	result, err := readStream(resp.Body)
	var api *APIError
	if errors.As(err, &api) {
		api.RequestID = resp.Header.Get("x-request-id")
		api.Status = resp.StatusCode
	}
	if err == nil {
		result.Model = body.Model
	}
	return result, err
}

type responseOutput struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}
type responseEvent struct {
	Type     string `json:"type"`
	Code     string `json:"code"`
	Param    string `json:"param"`
	Response struct {
		Status string `json:"status"`
		Error  *struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

// Partial deltas are never executed or returned. The authoritative completed
// response contains final text/calls and usage; EOF alone cannot be success.
func readStream(reader io.Reader) (domain.Completion, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, 32<<20))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var data []string
	dispatch := func() (domain.Completion, bool, error) {
		if len(data) == 0 {
			return domain.Completion{}, false, nil
		}
		raw := []byte(strings.Join(data, "\n"))
		data = nil
		if string(raw) == "[DONE]" {
			return domain.Completion{}, false, nil
		}
		var event responseEvent
		if json.Unmarshal(raw, &event) != nil {
			return domain.Completion{}, false, errors.New("invalid ChatGPT stream event")
		}
		switch event.Type {
		case "response.failed", "error":
			e := &APIError{Body: append([]byte{}, raw...), Code: event.Code, Param: event.Param}
			if event.Response.Error != nil {
				e.Code = event.Response.Error.Code
				e.Param = event.Response.Error.Param
			}
			return domain.Completion{}, false, e
		case "response.incomplete":
			return domain.Completion{}, false, errors.New("ChatGPT response incomplete")
		case "response.completed":
			if event.Response.Status != "completed" {
				return domain.Completion{}, false, errors.New("invalid ChatGPT completion status")
			}
			result := domain.Completion{Message: domain.Message{Role: "assistant"}, InputTokens: event.Response.Usage.Input, OutputTokens: event.Response.Usage.Output}
			for _, rawItem := range event.Response.Output {
				var item responseOutput
				if json.Unmarshal(rawItem, &item) != nil {
					return domain.Completion{}, false, errors.New("invalid ChatGPT output item")
				}
				result.Message.ResponsesItems = append(result.Message.ResponsesItems, rawItem)
				switch item.Type {
				case "message":
					for _, part := range item.Content {
						if part.Type == "output_text" {
							result.Message.Content += part.Text
						}
						if part.Type == "refusal" {
							result.Message.Content += part.Refusal
						}
					}
				case "function_call":
					if item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) || (item.Namespace != "" && item.Namespace != "openalbert") {
						return domain.Completion{}, false, errors.New("ChatGPT returned an invalid tool call")
					}
					result.Message.ToolCalls = append(result.Message.ToolCalls, domain.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)})
				}
			}
			if result.Message.Content == "" && len(result.Message.ToolCalls) == 0 {
				return domain.Completion{}, false, errors.New("ChatGPT returned an empty response")
			}
			return result, true, nil
		}
		return domain.Completion{}, false, nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			result, done, err := dispatch()
			if err != nil || done {
				return result, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return domain.Completion{}, errors.New("ChatGPT stream interrupted")
	}
	if result, done, err := dispatch(); err != nil || done {
		return result, err
	}
	return domain.Completion{}, errors.New("ChatGPT stream ended without response.completed")
}

var _ domain.Model = (*Client)(nil)
