package knowledge

import (
	"context"
	"openalbert/internal/domain"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParticipantProfilesPersistRenameAndSeparatePeopleAndScopes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"group-a", "group-b"} {
		for _, id := range []int64{7, 8} {
			if err = store.ObserveParticipant(ctx, domain.Participant{Scope: scope, UserID: id, DisplayName: "Саша", Username: "sasha", LastSeenAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			entry := domain.MemoryEntry{ID: scope + string(rune(id)), Scope: scope, SubjectUserID: id, Content: "Любит пончики", Kind: "preference", Source: "session", SourceMessageID: "message", Evidence: "Я люблю пончики"}
			if err = store.Update(ctx, entry); err != nil {
				t.Fatal(err)
			}
		}
	}
	next := time.Now().Add(time.Hour)
	if err = store.ObserveParticipant(ctx, domain.Participant{Scope: "group-a", UserID: 7, DisplayName: "Александр", Username: "new_name", LastSeenAt: next}); err != nil {
		t.Fatal(err)
	}
	if err = store.ObserveParticipant(ctx, domain.Participant{Scope: "group-a", UserID: 7, DisplayName: "Старое имя", LastSeenAt: next.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	store, err = New(root)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.Participant(ctx, "group-a", 7)
	if err != nil || profile.Participant.DisplayName != "Александр" || len(profile.Facts) != 1 || profile.Facts[0].SubjectUserID != 7 {
		t.Fatalf("profile lost: %v %v", profile, err)
	}
	names, err := store.Participants(ctx, "group-a", "пончики")
	if err != nil || len(names) != 2 {
		t.Fatalf("fact lookup failed: %v %v", names, err)
	}
	shared, err := store.Profile(ctx, "group-a")
	if err != nil || strings.Contains(shared, "пончики") {
		t.Fatal("participant preference leaked into shared USER.md")
	}
	if _, err = store.Participant(ctx, "private", 7); !os.IsNotExist(err) {
		t.Fatal("profile crossed scopes")
	}
	data, err := os.ReadFile(filepath.Join(root, participantDir("group-a", 7), "PROFILE.md"))
	if err != nil || !strings.Contains(string(data), "Александр") || !strings.Contains(string(data), "Я люблю пончики") {
		t.Fatalf("host profile missing attribution: %v", err)
	}
	fact := profile.Facts[0]
	fact.SubjectUserID = 8
	if err = store.Update(ctx, fact); err == nil {
		t.Fatal("moved a fact to a different person")
	}
	if err = store.Delete(ctx, "group-a", profile.Facts[0].ID); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Participant(ctx, "group-a", 7)
	other, _ := store.Participant(ctx, "group-a", 8)
	if len(after.Facts) != 0 || len(other.Facts) != 1 {
		t.Fatal("deletion changed another person")
	}
}
