package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"openalbert/apps/backend/internal/domain"
)

const groupSettingsFile = "group-settings.json"

func groupSettingsKey(target domain.GroupSettingsTarget) (string, error) {
	if target.GroupID == 0 && target.ConversationID == "" {
		return "all", nil
	}
	if target.GroupID >= 0 {
		return "", errors.New("group settings require a negative Telegram group ID")
	}
	if target.ConversationID != "" {
		return fmt.Sprintf("group:%d/scope:%s", target.GroupID, hash(target.ConversationID)), nil
	}
	return fmt.Sprintf("group:%d", target.GroupID), nil
}

func validateGroupSettings(s domain.GroupSettings) error {
	for _, field := range []struct {
		name, value string
		allowed     []string
	}{
		{"reply_mode", s.ReplyMode, []string{"", "contextual", "direct_only", "all"}},
		{"tone", s.Tone, []string{"", "personality", "neutral", "friendly"}},
		{"verbosity", s.Verbosity, []string{"", "normal", "brief", "detailed"}},
	} {
		valid := false
		for _, allowed := range field.allowed {
			valid = valid || field.value == allowed
		}
		if !valid {
			return fmt.Errorf("invalid group setting: %s", field.name)
		}
	}
	return nil
}

func mergeGroupSettings(base, specific domain.GroupSettings) domain.GroupSettings {
	if specific.ReplyMode != "" {
		base.ReplyMode = specific.ReplyMode
	}
	if specific.Tone != "" {
		base.Tone = specific.Tone
	}
	if specific.Verbosity != "" {
		base.Verbosity = specific.Verbosity
	}
	return base
}

// Call with s.mu held so reads, partial updates and inheritance are consistent.
func (s *Store) readGroupSettings() (map[string]domain.GroupSettings, error) {
	entries := map[string]domain.GroupSettings{}
	b, err := s.read(groupSettingsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, errors.New("group settings must be an object")
	}
	for _, entry := range entries {
		if err = validateGroupSettings(entry); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func groupSettingsView(entries map[string]domain.GroupSettings, target domain.GroupSettingsTarget) domain.GroupSettingsView {
	v := domain.GroupSettingsView{Defaults: entries["all"]}
	v.Effective = mergeGroupSettings(domain.GroupSettings{ReplyMode: "contextual", Tone: "personality", Verbosity: "normal"}, v.Defaults)
	if target.GroupID < 0 {
		groupKey, _ := groupSettingsKey(domain.GroupSettingsTarget{GroupID: target.GroupID})
		v.Group = entries[groupKey]
		v.Effective = mergeGroupSettings(v.Effective, v.Group)
	}
	if target.ConversationID != "" {
		topicKey, _ := groupSettingsKey(target)
		v.Topic = entries[topicKey]
		v.Effective = mergeGroupSettings(v.Effective, v.Topic)
	}
	return v
}

func (s *Store) GroupSettings(ctx context.Context, target domain.GroupSettingsTarget) (domain.GroupSettingsView, error) {
	if err := ctx.Err(); err != nil {
		return domain.GroupSettingsView{}, err
	}
	if _, err := groupSettingsKey(target); err != nil {
		return domain.GroupSettingsView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readGroupSettings()
	return groupSettingsView(entries, target), err
}

func (s *Store) UpdateGroupSettings(ctx context.Context, target domain.GroupSettingsTarget, patch domain.GroupSettingsPatch) (domain.GroupSettingsView, error) {
	if err := ctx.Err(); err != nil {
		return domain.GroupSettingsView{}, err
	}
	key, err := groupSettingsKey(target)
	if err != nil {
		return domain.GroupSettingsView{}, err
	}
	if patch.Reset && (patch.ReplyMode != nil || patch.Tone != nil || patch.Verbosity != nil) {
		return domain.GroupSettingsView{}, errors.New("reset cannot be combined with setting fields")
	}
	if !patch.Reset && patch.ReplyMode == nil && patch.Tone == nil && patch.Verbosity == nil {
		return domain.GroupSettingsView{}, errors.New("at least one group setting or reset is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readGroupSettings()
	if err != nil {
		return domain.GroupSettingsView{}, err
	}
	entry := entries[key]
	for _, field := range []struct{ value, destination *string }{
		{patch.ReplyMode, &entry.ReplyMode}, {patch.Tone, &entry.Tone}, {patch.Verbosity, &entry.Verbosity},
	} {
		if field.value != nil {
			*field.destination = *field.value
			if *field.destination == "inherit" {
				*field.destination = ""
			}
		}
	}
	if err = validateGroupSettings(entry); err != nil {
		return domain.GroupSettingsView{}, err
	}
	if patch.Reset || entry == (domain.GroupSettings{}) {
		delete(entries, key)
	} else {
		entries[key] = entry
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return domain.GroupSettingsView{}, err
	}
	if err = s.write(groupSettingsFile, append(b, '\n')); err != nil {
		return domain.GroupSettingsView{}, err
	}
	return groupSettingsView(entries, target), nil
}
