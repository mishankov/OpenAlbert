package knowledge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func groupRulesPath(groupID int64) (string, error) {
	if groupID >= 0 {
		return "", errors.New("GROUP.md requires a negative Telegram group ID")
	}
	return filepath.Join("groups", fmt.Sprint(groupID), "GROUP.md"), nil
}

func (s *Store) readGroupRules(path string) (string, error) {
	b, err := s.read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) || utf8.RuneCount(b) > 4000 {
		return "", errors.New("GROUP.md must be valid UTF-8 and at most 4000 characters")
	}
	return string(b), nil
}

// GroupRules belongs to a Telegram group, shared by all its conversations/topics.
// Authorization belongs to the agent boundary; this store never reads USER.md.
func (s *Store) GroupRules(ctx context.Context, groupID int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := groupRulesPath(groupID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readGroupRules(path)
}

// UpdateGroupRules creates an empty/missing GROUP.md with oldText="", or replaces
// one exact fragment. Replacing the whole text with "" clears the group rules.
func (s *Store) UpdateGroupRules(ctx context.Context, groupID int64, oldText, newText string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := groupRulesPath(groupID)
	if err != nil {
		return err
	}
	if oldText == newText || !utf8.ValidString(oldText) || !utf8.ValidString(newText) {
		return errors.New("old_text and new_text must be different valid UTF-8 strings")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.readGroupRules(path)
	if err != nil {
		return err
	}
	if (oldText == "" && current != "") || strings.Count(current, oldText) != 1 {
		return errors.New("old_text must match exactly once in current GROUP.md; read it again and include more surrounding text; empty old_text is only for an empty file")
	}
	updated := strings.Replace(current, oldText, newText, 1)
	if utf8.RuneCountInString(updated) > 4000 {
		return errors.New("GROUP.md must contain at most 4000 characters")
	}
	return s.write(path, []byte(updated))
}
