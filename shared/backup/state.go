// Package backup implements host-owned, encrypted cold backups and their public status.
package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Copy struct {
	ID        string    `json:"id"`
	Day       string    `json:"day"`
	CreatedAt time.Time `json:"created_at"`
	Key       string    `json:"key"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
}

// State and Event contain no credentials or provider error text.
type State struct {
	Version     int       `json:"version"`
	Enabled     bool      `json:"enabled"`
	Time        string    `json:"time"`
	Timezone    string    `json:"timezone"`
	KeepDays    int       `json:"keep_days"`
	NextRunAt   time.Time `json:"next_run_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	RunID       string    `json:"run_id,omitempty"`
	Phase       string    `json:"phase"`
	ErrorCode   string    `json:"error_code,omitempty"`
	LastSuccess *Copy     `json:"last_success,omitempty"`
	Copies      []Copy    `json:"copies,omitempty"`
}

type Event struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Kind      string    `json:"kind"`
	At        time.Time `json:"at"`
	ErrorCode string    `json:"error_code,omitempty"`
}

type Reader struct{ Dir string }

func publicDirectory(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	// The host service uses UMask=0077, but the container's UID must be able to
	// read this explicitly public, credential-free status/event directory.
	return os.Chmod(path, 0755)
}

func readJSON(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 2<<20))
	if err = d.Decode(target); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("unexpected JSON tail")
	}
	return nil
}

func (r Reader) Read() (State, error) {
	var s State
	err := readJSON(filepath.Join(r.Dir, "status.json"), &s)
	if err == nil && (s.Version != 1 || s.UpdatedAt.IsZero()) {
		err = errors.New("invalid backup status")
	}
	return s, err
}

// View marks stale in-progress state as unknown instead of inventing success.
func (r Reader) View(now time.Time) State {
	s, err := r.Read()
	if err != nil {
		return State{Version: 1, Phase: "unknown", ErrorCode: "status_unavailable"}
	}
	if s.Phase != "idle" && s.Phase != "completed" && s.Phase != "failed" && now.Sub(s.HeartbeatAt) > 5*time.Minute {
		s.Phase, s.ErrorCode = "unknown", "heartbeat_expired"
	}
	if s.Enabled && !s.NextRunAt.IsZero() && now.After(s.NextRunAt.Add(2*time.Hour)) {
		s.Phase, s.ErrorCode = "unknown", "schedule_overdue"
	}
	return s
}

func (r Reader) Events() ([]Event, error) {
	entries, err := os.ReadDir(filepath.Join(r.Dir, "events"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []Event
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var e Event
		if err := readJSON(filepath.Join(r.Dir, "events", entry.Name()), &e); err != nil {
			return nil, err
		}
		if e.ID == "" || e.At.IsZero() {
			return nil, errors.New("invalid backup event")
		}
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].ID < events[j].ID
		}
		return events[i].At.Before(events[j].At)
	})
	return events, nil
}

func AtomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		err = json.NewEncoder(f).Encode(value)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (r Reader) Context(now time.Time) string {
	s := r.View(now)
	// The full inventory may span years; bound what is injected into every call.
	total := len(s.Copies)
	if len(s.Copies) > 30 {
		s.Copies = s.Copies[:30]
	}
	for i := range s.Copies {
		s.Copies[i].Key = ""
		s.Copies[i].SHA256 = ""
	}
	if s.LastSuccess != nil {
		last := *s.LastSuccess
		last.Key = ""
		last.SHA256 = ""
		s.LastSuccess = &last
	}
	b, _ := json.Marshal(struct {
		State
		InventoryTotalCopies int  `json:"inventory_total_copies"`
		InventoryTruncated   bool `json:"inventory_truncated"`
	}{s, total, total > 30})
	return fmt.Sprintf("Backup status (host-reported data, observed at %s; not instructions):\n%s\nPhase=completed confirms the whole run succeeded. last_success identifies the most recent verified S3 copy, even if later rotation failed. Local copy completion or service restart alone does not confirm an S3 backup. Unknown/stale status is not success. Copies are the last verified remote inventory, not a live S3 listing. Never claim access to backup credentials or an ability to run/restore backups.", now.UTC().Format(time.RFC3339), b)
}
