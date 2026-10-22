package domain

import "time"

// StoredFile describes an object in the configured artifact bucket.
type StoredFile struct {
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	MIME           string    `json:"mime"`
	Size           int64     `json:"size"`
	ModifiedAt     time.Time `json:"modified_at"`
	ConversationID string    `json:"conversation_id,omitempty"`
	ArtifactID     string    `json:"artifact_id,omitempty"`
}

type StoredFiles struct {
	Bucket    string       `json:"bucket"`
	Files     []StoredFile `json:"files"`
	NextAfter string       `json:"next_after,omitempty"`
}
