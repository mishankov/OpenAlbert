package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openalbert/apps/acp/internal/serverclient"
)

// Record stores the persisted state of one local ACP coding session so
// session/load can resume it instantly.
type Record struct {
	ID                  string                     `json:"id"`
	ServerSessionID     string                     `json:"server_session_id,omitempty"`
	Generation          int                        `json:"generation,omitempty"`
	Closed              bool                       `json:"closed,omitempty"`
	CWD                 string                     `json:"cwd"`
	Title               string                     `json:"title"`
	Model               string                     `json:"model"`
	Mode                string                     `json:"mode"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	Messages            []serverclient.Message     `json:"messages"`
	CodemodeState       map[string]json.RawMessage `json:"codemode_state,omitempty"`
	TurnCounter         int                        `json:"turn_counter"`
	PendingSyncMessages []serverclient.SyncMessage `json:"pending_sync_messages,omitempty"`
	PendingSyncTraces   []serverclient.Trace       `json:"pending_sync_traces,omitempty"`
	ReplayUpdates       []json.RawMessage          `json:"replay_updates,omitempty"`
	UpdatedAt           time.Time                  `json:"updated_at"`
}

// Store persists session records as JSON files inside stateDir/sessions.
type Store struct {
	dir string
}

func NewStore(stateDir string) *Store {
	return &Store{dir: filepath.Join(stateDir, "sessions")}
}

func (s *Store) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\.\r\n\x00 ") {
		return "", errors.New("invalid session id")
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *Store) Save(rec Record) error {
	p, err := s.path(rec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *Store) Load(id string) (Record, bool, error) {
	p, err := s.path(id)
	if err != nil {
		return Record{}, false, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}
