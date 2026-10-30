package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

func settingValue(s string) *string { return &s }

func TestGroupSettingsPersistInheritAndResetWithoutChangingMemory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	group := domain.GroupSettingsTarget{GroupID: -20}
	topic := domain.GroupSettingsTarget{GroupID: -20, ConversationID: "group:20:topic:3"}
	otherTopic := domain.GroupSettingsTarget{GroupID: -20, ConversationID: "group:20:topic:4"}
	update := func(target domain.GroupSettingsTarget, patch domain.GroupSettingsPatch) {
		t.Helper()
		if _, err := s.UpdateGroupSettings(ctx, target, patch); err != nil {
			t.Fatal(err)
		}
	}
	update(domain.GroupSettingsTarget{}, domain.GroupSettingsPatch{ReplyMode: settingValue("contextual"), Tone: settingValue("friendly")})
	update(group, domain.GroupSettingsPatch{ReplyMode: settingValue("direct_only"), Verbosity: settingValue("brief")})
	update(topic, domain.GroupSettingsPatch{ReplyMode: settingValue("all")})
	// A partial update preserves the mode and does not affect sibling topics.
	update(topic, domain.GroupSettingsPatch{Tone: settingValue("neutral")})
	s, err = New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		target domain.GroupSettingsTarget
		want   domain.GroupSettings
	}{
		{topic, domain.GroupSettings{ReplyMode: "all", Tone: "neutral", Verbosity: "brief"}},
		{otherTopic, domain.GroupSettings{ReplyMode: "direct_only", Tone: "friendly", Verbosity: "brief"}},
		{domain.GroupSettingsTarget{GroupID: -30}, domain.GroupSettings{ReplyMode: "contextual", Tone: "friendly", Verbosity: "normal"}},
	} {
		v, err := s.GroupSettings(ctx, tc.target)
		if err != nil || v.Effective != tc.want {
			t.Fatalf("settings after reopen: %+v %v; want %+v", v, err, tc.want)
		}
	}
	update(topic, domain.GroupSettingsPatch{ReplyMode: settingValue("inherit")})
	v, err := s.GroupSettings(ctx, topic)
	if err != nil || v.Effective.ReplyMode != "direct_only" || v.Effective.Tone != "neutral" {
		t.Fatalf("field reset lost other overrides: %+v %v", v, err)
	}
	update(topic, domain.GroupSettingsPatch{Reset: true})
	update(group, domain.GroupSettingsPatch{Reset: true})
	v, err = s.GroupSettings(ctx, topic)
	if err != nil || v.Effective != (domain.GroupSettings{ReplyMode: "contextual", Tone: "friendly", Verbosity: "normal"}) {
		t.Fatalf("scope reset did not restore defaults: %+v %v", v, err)
	}
	profile, err := s.Profile(ctx, "personal:7")
	if err != nil || profile != "" {
		t.Fatalf("group settings modified private memory: %q %v", profile, err)
	}
}

func TestGroupSettingsRejectPrivateProseInvalidTargetsAndCorruptFiles(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, patch := range []domain.GroupSettingsPatch{
		{},
		{Tone: settingValue("my private address")},
		{Verbosity: settingValue("invalid")},
		{ReplyMode: settingValue("invalid")},
		{Reset: true, Tone: settingValue("neutral")},
	} {
		if _, err := s.UpdateGroupSettings(ctx, domain.GroupSettingsTarget{}, patch); err == nil {
			t.Fatal("invalid settings accepted")
		}
	}
	for _, target := range []domain.GroupSettingsTarget{{GroupID: 7}, {ConversationID: "personal:7"}} {
		if _, err := s.UpdateGroupSettings(ctx, target, domain.GroupSettingsPatch{Reset: true}); err == nil {
			t.Fatal("invalid settings target accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(s.root, groupSettingsFile), []byte(`{"all":{"tone":"private prose"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GroupSettings(ctx, domain.GroupSettingsTarget{}); err == nil {
		t.Fatal("corrupt settings silently entered context")
	}
}
