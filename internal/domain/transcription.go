package domain

import (
	"context"
	"time"
)

// SpeechRecognizer handles completed audio files independently of the chat model.
// Profile must change when weights, language or normalization settings change.
type SpeechRecognizer interface {
	Profile() string
	Transcribe(context.Context, string, []byte) (SpeechText, error)
}

type SpeechText struct {
	Text            string  `json:"text"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	DecodeMS        int64   `json:"decode_ms,omitempty"`
	InferenceMS     int64   `json:"inference_ms,omitempty"`
}

// AudioResult is the durable projection for one incoming attachment. The original
// author and conversation still belong to Incoming; STT never supplies identity.
type AudioResult struct {
	SpeechText
	ConversationID string    `json:"conversation_id"`
	AuthorID       int64     `json:"author_id"`
	AuthorName     string    `json:"author_name,omitempty"`
	SessionID      string    `json:"session_id"`
	IncomingID     string    `json:"incoming_id"`
	ArtifactID     string    `json:"artifact_id"`
	Model          string    `json:"model"`
	Status         string    `json:"status"`
	ErrorCode      string    `json:"error_code,omitempty"`
	Cached         bool      `json:"cached"`
	QueuedAt       time.Time `json:"queued_at"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
}
