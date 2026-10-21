package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"jarvis/internal/domain"
)

func participantDir(scope string, id int64) string {
	return filepath.Join(scopeDir(scope), "participants", strconv.FormatInt(id, 10))
}
func (s *Store) participant(scope string, id int64) (domain.Participant, error) {
	var person domain.Participant
	b, err := s.read(filepath.Join(participantDir(scope, id), "IDENTITY.md"))
	if err != nil {
		return person, err
	}
	parts := strings.SplitN(string(b), "\n---\n", 2)
	if len(parts) != 2 {
		return person, errors.New("invalid participant identity")
	}
	if err = json.Unmarshal([]byte(strings.TrimPrefix(parts[0], "---\n")), &person); err != nil {
		return person, err
	}
	if person.Scope != scope || person.UserID != id {
		return person, errors.New("participant scope mismatch")
	}
	return person, nil
}
func (s *Store) ObserveParticipant(ctx context.Context, p domain.Participant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := nonempty(p.Scope); err != nil {
		return err
	}
	if p.UserID <= 0 {
		return errors.New("positive participant user_id required")
	}
	p.DisplayName = string([]rune(p.DisplayName)[:min(len([]rune(p.DisplayName)), 200)])
	p.Username = string([]rune(p.Username)[:min(len([]rune(p.Username)), 100)])
	if p.LastSeenAt.IsZero() {
		p.LastSeenAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.participant(p.Scope, p.UserID)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if !p.LastSeenAt.After(old.LastSeenAt) {
			return s.rebuildParticipant(p.Scope, p.UserID)
		}
		if p.DisplayName == "" {
			p.DisplayName = old.DisplayName
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	body := append([]byte("---\n"), b...)
	body = append(body, []byte("\n---\n# Telegram participant\n")...)
	if err = s.write(filepath.Join(participantDir(p.Scope, p.UserID), "IDENTITY.md"), body); err != nil {
		return err
	}
	return s.rebuildParticipant(p.Scope, p.UserID)
}
func (s *Store) participantProfile(scope string, id int64) (domain.ParticipantProfile, error) {
	p, err := s.participant(scope, id)
	if err != nil {
		return domain.ParticipantProfile{}, err
	}
	entries, err := s.all(scope)
	if err != nil {
		return domain.ParticipantProfile{}, err
	}
	facts := []domain.MemoryEntry{}
	for _, e := range entries {
		if e.SubjectUserID == id {
			facts = append(facts, e)
		}
	}
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].UpdatedAt.Equal(facts[j].UpdatedAt) {
			return facts[i].ID < facts[j].ID
		}
		return facts[i].UpdatedAt.After(facts[j].UpdatedAt)
	})
	return domain.ParticipantProfile{Participant: p, Facts: facts}, nil
}
func (s *Store) rebuildParticipant(scope string, id int64) error {
	profile, err := s.participantProfile(scope, id)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Participant %d\n\n- Display name: %s\n- Username: %s\n- Conversation: %s\n\n", id, profile.Participant.DisplayName, profile.Participant.Username, scope)
	for _, fact := range profile.Facts {
		fmt.Fprintf(&b, "- %s\n  - Kind: %s; memory ID: %s\n  - Source: %s; message: %s\n  - Evidence: %q\n", fact.Content, fact.Kind, fact.ID, fact.Source, fact.SourceMessageID, fact.Evidence)
	}
	return s.write(filepath.Join(participantDir(scope, id), "PROFILE.md"), []byte(b.String()))
}
func (s *Store) Participant(ctx context.Context, scope string, id int64) (domain.ParticipantProfile, error) {
	if err := ctx.Err(); err != nil {
		return domain.ParticipantProfile{}, err
	}
	if id <= 0 {
		return domain.ParticipantProfile{}, errors.New("positive participant user_id required")
	}
	if err := nonempty(scope); err != nil {
		return domain.ParticipantProfile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.participantProfile(scope, id)
}
func (s *Store) Participants(ctx context.Context, scope, q string) ([]domain.Participant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := nonempty(scope); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.safePath(filepath.Join(scopeDir(scope), "participants"))
	if err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []domain.Participant{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := s.all(scope)
	if err != nil {
		return nil, err
	}
	out := []domain.Participant{}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(dir.Name(), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		person, err := s.participant(scope, id)
		if err != nil {
			return nil, err
		}
		searchable := fmt.Sprint(id) + " " + person.DisplayName + " " + person.Username
		for _, e := range entries {
			if e.SubjectUserID == id {
				searchable += " " + e.Content
			}
		}
		match := true
		for _, term := range strings.Fields(strings.ToLower(q)) {
			if !strings.Contains(strings.ToLower(searchable), term) {
				match = false
				break
			}
		}
		if match {
			out = append(out, person)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}
