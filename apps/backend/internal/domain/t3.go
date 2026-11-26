package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrT3EnvironmentMismatch = errors.New("command belongs to another environment")

const (
	T3CommandStatusPending    = "pending"
	T3CommandStatusDispatched = "dispatched"
	T3CommandStatusSucceeded  = "succeeded"
	T3CommandStatusFailed     = "failed"

	T3ActionSyncCapabilities = "sync_capabilities"
	T3ActionThreadLaunch     = "thread_launch"
	T3ActionThreadSend       = "thread_send"
	T3ActionThreadRead       = "thread_read"
	T3ActionThreadRespond    = "thread_respond"
	T3ActionThreadControl    = "thread_control"
	T3ActionArtifactFetch    = "artifact_fetch"

	T3EventThreadLaunched     = "thread_launched"
	T3EventThreadProgress     = "thread_progress"
	T3EventThreadWaitingInput = "thread_waiting_input"
	T3EventThreadCompleted    = "thread_completed"
	T3EventThreadFailed       = "thread_failed"
)

func ValidT3EventKind(kind string) bool {
	switch kind {
	case T3EventThreadLaunched, T3EventThreadProgress, T3EventThreadWaitingInput, T3EventThreadCompleted, T3EventThreadFailed:
		return true
	default:
		return false
	}
}

func ValidT3CommandAction(action string) bool {
	switch action {
	case T3ActionSyncCapabilities, T3ActionThreadLaunch, T3ActionThreadSend, T3ActionThreadRead, T3ActionThreadRespond, T3ActionThreadControl, T3ActionArtifactFetch:
		return true
	default:
		return false
	}
}

type T3Project struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	WorkspaceRoot string `json:"workspace_root"`
}

type T3ModelOption struct {
	ID      string `json:"id"`
	Values  []any  `json:"values"`
	Default any    `json:"default"`
}

type T3Model struct {
	ID      string          `json:"id"`
	Label   string          `json:"label"`
	Options []T3ModelOption `json:"options,omitempty"`
}

type T3Provider struct {
	ProviderInstanceID string    `json:"provider_instance_id"`
	DriverKind         string    `json:"driver_kind"`
	DisplayName        string    `json:"display_name"`
	Models             []T3Model `json:"models"`
}

type T3Host struct {
	HostID          string       `json:"host_id"`
	EnvironmentID   string       `json:"environment_id"`
	Label           string       `json:"label"`
	Online          bool         `json:"online"`
	Projects        []T3Project  `json:"projects"`
	Providers       []T3Provider `json:"providers"`
	ActiveThreadIDs []string     `json:"active_thread_ids,omitempty"`
	LastSeenAt      time.Time    `json:"last_seen_at"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

type T3Command struct {
	Seq           int64           `json:"-"`
	ID            string          `json:"command_id"`
	EnvironmentID string          `json:"environment_id"`
	Action        string          `json:"action"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	ErrorText     string          `json:"error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type T3PendingRequest struct {
	RequestID string            `json:"request_id"`
	Questions []json.RawMessage `json:"questions"`
}

// UnmarshalJSON accepts both the frozen snake_case shape (request_id, questions)
// and bridge/MCP variants (requestId, singular question), normalizing into the
// canonical wire representation.
func (p *T3PendingRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		RequestID      string            `json:"request_id"`
		CamelRequestID string            `json:"requestId"`
		Questions      []json.RawMessage `json:"questions"`
		Question       json.RawMessage   `json:"question"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.RequestID = strings.TrimSpace(raw.RequestID)
	if p.RequestID == "" {
		p.RequestID = strings.TrimSpace(raw.CamelRequestID)
	}
	if len(raw.Questions) > 0 {
		p.Questions = raw.Questions
	} else if trimmed := bytes.TrimSpace(raw.Question); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		p.Questions = []json.RawMessage{append(json.RawMessage(nil), trimmed...)}
	} else {
		p.Questions = []json.RawMessage{}
	}
	return nil
}

type T3Thread struct {
	ThreadID           string            `json:"thread_id"`
	EnvironmentID      string            `json:"environment_id"`
	HostID             string            `json:"host_id,omitempty"`
	ConversationID     string            `json:"conversation_id,omitempty"`
	TaskID             string            `json:"task_id,omitempty"`
	ProjectID          string            `json:"project_id,omitempty"`
	Title              string            `json:"title,omitempty"`
	Link               string            `json:"link,omitempty"`
	ProviderInstanceID string            `json:"provider_instance_id,omitempty"`
	Model              string            `json:"model,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	Status             string            `json:"status,omitempty"`
	PRURL              string            `json:"pr_url,omitempty"`
	PendingRequest     *T3PendingRequest `json:"pending_request,omitempty"`
	LastOutput         json.RawMessage   `json:"last_output,omitempty"`
	LastSummary        string            `json:"last_summary,omitempty"`
	LastError          string            `json:"last_error,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
}

type T3ThreadPatch struct {
	ThreadID               string
	EnvironmentID          string
	ConversationID         string
	TaskID                 string
	ProjectID              string
	Title                  string
	Link                   string
	ProviderInstanceID     string
	Model                  string
	Branch                 string
	Status                 string
	PRURL                  string
	PreserveExistingStatus bool // Fill status only if empty (e.g. late launch result after terminal event).
	RequireCurrent         bool // Apply mutable state fields only when At >= existing updated_at.
	SetPendingRequest      bool
	PendingRequest         *T3PendingRequest
	SetLastOutput          bool
	LastOutput             json.RawMessage
	LastSummary            string
	SetLastError           bool
	LastError              string
	At                     time.Time
}

// T3CommandThreadEffect describes the atomic thread state update to commit
// alongside a command's first terminal transition.
type T3CommandThreadEffect struct {
	Patch          *T3ThreadPatch
	ClearThreadID  string
	ClearRequestID string
}

type T3ThreadFilter struct {
	EnvironmentID  string
	HostID         string
	ConversationID string
	ProjectID      string
	Status         string
	Limit          int
}

type T3Event struct {
	EventID            string            `json:"event_id"`
	EnvironmentID      string            `json:"environment_id"`
	ThreadID           string            `json:"thread_id"`
	Kind               string            `json:"kind"`
	Status             string            `json:"status,omitempty"`
	Title              string            `json:"title,omitempty"`
	Link               string            `json:"link,omitempty"`
	ProjectID          string            `json:"project_id,omitempty"`
	ProviderInstanceID string            `json:"provider_instance_id,omitempty"`
	Model              string            `json:"model,omitempty"`
	Branch             string            `json:"branch,omitempty"`
	PRURL              string            `json:"pr_url,omitempty"`
	PendingRequest     *T3PendingRequest `json:"pending_request,omitempty"`
	Output             json.RawMessage   `json:"output,omitempty"`
	Summary            string            `json:"summary,omitempty"`
	Error              string            `json:"error,omitempty"`
	OccurredAt         time.Time         `json:"occurred_at,omitempty"`
	// Resolved from openalbert_t3_threads before notifying observers.
	ConversationID string `json:"conversation_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
}

type T3StoredEvent struct {
	Event   T3Event
	WokenAt *time.Time
}

// T3Store persists T3 Control Plane hosts, commands, threads, and deduplicated events.
type T3Store interface {
	UpsertT3Host(ctx context.Context, host T3Host) (T3Host, error)
	TouchT3Host(ctx context.Context, environmentID string, at time.Time) (T3Host, bool, error)
	T3Host(ctx context.Context, hostOrEnvID string) (T3Host, bool, error)
	ListT3Hosts(ctx context.Context) ([]T3Host, error)

	CreateT3Command(ctx context.Context, cmd T3Command) (T3Command, bool, error)
	T3Command(ctx context.Context, id string) (T3Command, bool, error)
	ClaimPendingT3Commands(ctx context.Context, environmentID string, limit int, at, redeliveryBefore time.Time) ([]T3Command, error)
	RequeueT3Command(ctx context.Context, id string, at time.Time) error
	RequeueDispatchedT3Commands(ctx context.Context, environmentID string, at time.Time) error
	CompleteT3Command(
		ctx context.Context,
		id, environmentID string,
		ok bool,
		result json.RawMessage,
		errText string,
		at time.Time,
		buildEffect func(cmd T3Command) *T3CommandThreadEffect,
	) (cmd T3Command, thread *T3Thread, found bool, transitioned bool, err error)

	UpsertT3Thread(ctx context.Context, patch T3ThreadPatch) (T3Thread, error)
	ClearT3ThreadPendingRequest(ctx context.Context, threadID, requestID string, at time.Time) error
	T3Thread(ctx context.Context, threadID string) (T3Thread, bool, error)
	ListT3Threads(ctx context.Context, filter T3ThreadFilter) ([]T3Thread, error)

	RecordT3EventAndUpsertThread(ctx context.Context, event T3Event, rawPayload json.RawMessage, patch T3ThreadPatch, receivedAt time.Time) (thread T3Thread, deduplicated bool, stored T3StoredEvent, err error)
	PendingWakeT3Events(ctx context.Context, threadID string) ([]T3Event, error)
	MarkT3EventWoken(ctx context.Context, eventID string, at time.Time) error
}
