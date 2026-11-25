package serverclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type T3Project struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspace_root"`
}

type T3Model struct {
	ID      string            `json:"id"`
	Label   string            `json:"label,omitempty"`
	Options []json.RawMessage `json:"options,omitempty"`
}

type T3Provider struct {
	ProviderInstanceID string    `json:"provider_instance_id"`
	DriverKind         string    `json:"driver_kind"`
	DisplayName        string    `json:"display_name"`
	Models             []T3Model `json:"models"`
}

type T3HostRegisterRequest struct {
	EnvironmentID string       `json:"environment_id"`
	Label         string       `json:"label"`
	Projects      []T3Project  `json:"projects"`
	Providers     []T3Provider `json:"providers"`
}

type T3HostRegisterResponse struct {
	HostID        string `json:"host_id"`
	EnvironmentID string `json:"environment_id"`
	RegisteredAt  string `json:"registered_at"`
}

type T3HeartbeatRequest struct {
	EnvironmentID   string   `json:"environment_id"`
	ActiveThreadIDs []string `json:"active_thread_ids"`
}

type T3HeartbeatResponse struct {
	OK         bool   `json:"ok"`
	ServerTime string `json:"server_time"`
}

type T3Command struct {
	CommandID string          `json:"command_id"`
	Action    string          `json:"action"`
	Payload   json.RawMessage `json:"payload"`
}

type T3CommandsDrainResponse struct {
	Commands []T3Command `json:"commands"`
}

type T3CommandResultRequest struct {
	EnvironmentID string          `json:"environment_id"`
	OK            bool            `json:"ok"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type T3ThreadEvent struct {
	EventID            string          `json:"event_id"`
	EnvironmentID      string          `json:"environment_id"`
	ThreadID           string          `json:"thread_id"`
	Kind               string          `json:"kind"`
	Status             string          `json:"status,omitempty"`
	Title              string          `json:"title,omitempty"`
	Link               string          `json:"link,omitempty"`
	ProjectID          string          `json:"project_id,omitempty"`
	ProviderInstanceID string          `json:"provider_instance_id,omitempty"`
	Model              string          `json:"model,omitempty"`
	Branch             string          `json:"branch,omitempty"`
	PRURL              string          `json:"pr_url,omitempty"`
	PendingRequest     json.RawMessage `json:"pending_request,omitempty"`
	Output             json.RawMessage `json:"output,omitempty"`
	Summary            string          `json:"summary,omitempty"`
	Error              string          `json:"error,omitempty"`
	OccurredAt         string          `json:"occurred_at"`
}

func (c *Client) RegisterT3Host(ctx context.Context, req T3HostRegisterRequest) (T3HostRegisterResponse, error) {
	if req.Projects == nil {
		req.Projects = []T3Project{}
	}
	if req.Providers == nil {
		req.Providers = []T3Provider{}
	}
	var out T3HostRegisterResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/t3/hosts/register", req, &out)
	return out, err
}

func (c *Client) HeartbeatT3Host(ctx context.Context, req T3HeartbeatRequest) (T3HeartbeatResponse, error) {
	if req.ActiveThreadIDs == nil {
		req.ActiveThreadIDs = []string{}
	}
	var out T3HeartbeatResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/t3/heartbeat", req, &out)
	return out, err
}

func (c *Client) DrainT3Commands(ctx context.Context, environmentID string) ([]T3Command, error) {
	q := url.Values{}
	q.Set("environment_id", environmentID)
	q.Set("once", "true")
	var out T3CommandsDrainResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/t3/commands/stream?"+q.Encode(), nil, &out)
	return out.Commands, err
}

func (c *Client) StreamT3Commands(ctx context.Context, environmentID string, onCommand func(T3Command) error) error {
	q := url.Values{}
	q.Set("environment_id", environmentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/t3/commands/stream?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "openalbert/0.1")

	resp, err := c.streamClient.Do(req)
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

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var (
		eventName string
		eventID   string
		dataLines []string
	)
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			eventID = ""
			return nil
		}
		joined := strings.TrimSpace(strings.Join(dataLines, "\n"))
		ev := eventName
		id := eventID
		eventName = ""
		eventID = ""
		dataLines = nil
		if joined == "" || (ev != "" && ev != "command") {
			return nil
		}
		var cmd T3Command
		if err := json.Unmarshal([]byte(joined), &cmd); err != nil {
			return fmt.Errorf("invalid T3 SSE command: %w", err)
		}
		if cmd.CommandID == "" {
			cmd.CommandID = id
		}
		if cmd.CommandID == "" || cmd.Action == "" {
			return nil
		}
		if onCommand != nil {
			return onCommand(cmd)
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "id:") {
			eventID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		} else if strings.HasPrefix(line, "data:") {
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			dataLines = append(dataLines, val)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("T3 command stream interrupted: %w", err)
	}
	return flush()
}

func (c *Client) PostT3CommandResult(ctx context.Context, commandID string, req T3CommandResultRequest) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/t3/commands/"+url.PathEscape(commandID)+"/result", req, nil)
}

func (c *Client) PostT3Event(ctx context.Context, ev T3ThreadEvent) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/t3/events", ev, nil)
}
