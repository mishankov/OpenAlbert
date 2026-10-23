package domain

import (
	"context"
	"encoding/json"
	"time"
)

type Conversation struct {
	Title    string `json:"title,omitempty"`
	ID       string `json:"id"`
	ChatID   int64  `json:"chat_id"`
	ThreadID int    `json:"thread_id,omitempty"`
	OwnerID  int64  `json:"owner_id"`
	Private  bool   `json:"private"`
}
type Attachment struct {
	SizeBytes       int64  `json:"size_bytes,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	FileID          string `json:"file_id,omitempty"`
	Name            string `json:"name"`
	MIME            string `json:"mime"`
	Kind            string `json:"kind"`
	Data            []byte `json:"-"`
	ArtifactID      string `json:"artifact_id,omitempty"`
}
type Incoming struct {
	AuthorName     string       `json:"author_name,omitempty"`
	AuthorUsername string       `json:"author_username,omitempty"`
	ID             string       `json:"id"`
	Conversation   Conversation `json:"conversation"`
	Text           string       `json:"text"`
	Attachments    []Attachment `json:"attachments,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
}
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type Message struct {
	Reasoning       string     `json:"reasoning,omitempty"`
	SourceMessageID string     `json:"source_message_id,omitempty"`
	AuthorName      string     `json:"author_name,omitempty"`
	AuthorUsername  string     `json:"author_username,omitempty"`
	AuthorID        int64      `json:"author_id,omitempty"`
	Role            string     `json:"role"`
	Content         string     `json:"content"`
	Images          []string   `json:"images,omitempty"`
	ToolCalls       []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID      string     `json:"tool_call_id,omitempty"`
}
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}
type Completion struct {
	Message      Message
	InputTokens  int
	OutputTokens int
}
type Model interface {
	Complete(context.Context, []Message, []Tool) (Completion, error)
}
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}
type Search interface {
	Search(context.Context, string) ([]SearchResult, error)
	Read(context.Context, string) (string, error)
}
type Messenger interface {
	Send(context.Context, Conversation, string) (string, error)
	SendFile(context.Context, Conversation, string, []byte) (string, error)
	Download(context.Context, string) ([]byte, error)
}
type Task struct {
	ID              string    `json:"id"`
	ConversationID  string    `json:"conversation_id"`
	Title           string    `json:"title"`
	Goal            string    `json:"goal"`
	Reason          string    `json:"reason,omitempty"`
	Status          string    `json:"status"`
	Result          string    `json:"result,omitempty"`
	ScheduleSeconds int       `json:"schedule_seconds,omitempty"`
	NextRunAt       time.Time `json:"next_run_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Version         int64     `json:"version"`
}
type Outgoing struct {
	RecordInHistory bool         `json:"record_in_history,omitempty"`
	ID              string       `json:"id"`
	Conversation    Conversation `json:"conversation"`
	Text            string       `json:"text"`
	Entities        []TextEntity `json:"entities,omitempty"`
	Formatted       bool         `json:"formatted,omitempty"`
	ArtifactID      string       `json:"artifact_id,omitempty"`
	State           string       `json:"state"`
	Attempts        int          `json:"attempts"`
	TurnID          string       `json:"turn_id,omitempty"`
	SessionID       string       `json:"session_id,omitempty"`
	TaskID          string       `json:"task_id,omitempty"`
	Trigger         string       `json:"trigger,omitempty"`
	StartedAt       time.Time    `json:"started_at,omitempty"`
	ReceivedAt      time.Time    `json:"received_at,omitempty"`
	EnqueuedAt      time.Time    `json:"enqueued_at,omitempty"`
	PartIndex       int          `json:"part_index,omitempty"`
	PartCount       int          `json:"part_count,omitempty"`
}

// TextEntity uses Telegram's UTF-16 code unit offsets, including for emoji.
type TextEntity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
}
type Artifact struct {
	SessionID      string    `json:"session_id,omitempty"`
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Name           string    `json:"name"`
	MIME           string    `json:"mime"`
	Data           []byte    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
}
type Session struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Status         string    `json:"status"`
	Summary        string    `json:"summary,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// HistoryMessage is a saved user/assistant message with its original scope.
// SessionStartedAt is not the individual message's timestamp.
type HistoryMessage struct {
	AuthorID         int64             `json:"author_id,omitempty"`
	ToolCalls        []HistoryToolCall `json:"tool_calls,omitempty"`
	Truncated        bool              `json:"truncated,omitempty"`
	Seq              int64             `json:"seq"`
	ConversationID   string            `json:"conversation_id"`
	SessionID        string            `json:"session_id"`
	SessionStartedAt time.Time         `json:"session_started_at"`
	Role             string            `json:"role"`
	Content          string            `json:"content"`
}

// HistoryToolCall identifies an actual send_message call. Its text is already
// included in HistoryMessage.Content; full arguments remain in stored messages.
type HistoryToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Repository stores operational state. All mutation keys supplied by callers must be idempotent.
type Repository interface {
	Close()
	Ping(context.Context) error
	LoadCheckpoint(context.Context, string) (json.RawMessage, bool, error)
	SaveCheckpoint(context.Context, string, json.RawMessage) error
	ClaimTranscription(context.Context, string, string) (SpeechText, bool, bool, error)
	CompleteTranscription(context.Context, string, string, SpeechText) (bool, error)
	ReleaseTranscription(context.Context, string, string) error
	SaveTrace(context.Context, Trace) error
	AcceptIncoming(context.Context, Incoming, int64) (bool, error)
	PollOffset(context.Context) (int64, error)
	SetPollOffset(context.Context, int64) error
	PendingIncoming(context.Context, int) ([]Incoming, error)
	Incoming(context.Context, string) (Incoming, bool, error)
	MarkIncomingHandled(context.Context, string) error
	Conversation(context.Context, string) (Conversation, bool, error)
	ListPrivateConversations(context.Context) ([]Conversation, error)
	ListConversations(context.Context) ([]Conversation, error)
	ConversationHistory(context.Context, string, string, int64, int) ([]HistoryMessage, error)
	StartSession(context.Context, Session) error
	EndSession(context.Context, string, string) error
	PendingAnalysis(context.Context, int) ([]Session, error)
	ActiveSessions(context.Context) ([]Session, error)
	MarkAnalyzed(context.Context, string) error
	AppendMessage(context.Context, string, string, Message) error
	Messages(context.Context, string) ([]Message, error)
	CreateTask(context.Context, Task) (Task, error)
	UpdateTask(context.Context, Task) (Task, error)
	FindTasks(context.Context, string, string) ([]Task, error)
	Task(context.Context, string, string) (Task, bool, error)
	RunnableTasks(context.Context) ([]Task, error)
	EnqueueOutgoing(context.Context, Outgoing) error
	PendingOutgoing(context.Context, int) ([]Outgoing, error)
	SetOutgoingState(context.Context, string, string) error
	StartProgress(context.Context, Progress) error
	ReportProgress(context.Context, string, int, string) error
	EndProgress(context.Context, string, string) error
	DueProgress(context.Context, time.Time, int) ([]Progress, error)
	SetProgressDelivery(context.Context, string, int64, ProgressDelivery) (Progress, bool, error)
	SaveArtifact(context.Context, Artifact) error
	Artifact(context.Context, string, string) (Artifact, bool, error)
}

type MemoryEntry struct {
	SubjectUserID   int64     `json:"subject_user_id,omitempty"`
	SourceMessageID string    `json:"source_message_id,omitempty"`
	Evidence        string    `json:"evidence,omitempty"`
	ID              string    `json:"id"`
	Scope           string    `json:"scope"`
	Content         string    `json:"content"`
	Source          string    `json:"source"`
	Kind            string    `json:"kind"`
	UpdatedAt       time.Time `json:"updated_at"`
}
type Participant struct {
	Scope       string    `json:"scope"`
	UserID      int64     `json:"user_id"`
	DisplayName string    `json:"display_name,omitempty"`
	Username    string    `json:"username,omitempty"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}
type ParticipantProfile struct {
	Participant Participant   `json:"participant"`
	Facts       []MemoryEntry `json:"facts"`
}
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content,omitempty"`
}
type Knowledge interface {
	ObserveParticipant(context.Context, Participant) error
	Participants(context.Context, string, string) ([]Participant, error)
	Participant(context.Context, string, int64) (ParticipantProfile, error)
	Profile(context.Context, string) (string, error)
	Soul(context.Context) (string, error)
	UpdateSoul(context.Context, string, string) error
	Search(context.Context, string, string) ([]MemoryEntry, error)
	Read(context.Context, string, string) (MemoryEntry, error)
	Update(context.Context, MemoryEntry) error
	Delete(context.Context, string, string) error
	Skills(context.Context) ([]Skill, error)
	ReadSkill(context.Context, string) (Skill, error)
	ProposeSkill(context.Context, string, string, string) error
}
