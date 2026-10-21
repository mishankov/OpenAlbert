package knowledge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jarvis/internal/domain"
)

func TestKnowledgePersistenceAndIsolation(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	soul, err := s.Soul(ctx)
	if err != nil || soul == "" {
		t.Fatalf("soul %q %v", soul, err)
	}
	entry := domain.MemoryEntry{ID: "morning-meetings", Scope: "telegram:private:123", Kind: "preference", Content: "Предпочитает утренние встречи", Source: "session:s1", UpdatedAt: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	if err = s.Update(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(ctx, entry); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Read(ctx, entry.Scope, entry.ID)
	if err != nil || got != entry {
		t.Fatalf("reopened %#v %v", got, err)
	}
	if _, err = reopened.Read(ctx, "telegram:group:456", entry.ID); !os.IsNotExist(err) {
		t.Fatalf("read across scope: %v", err)
	}
	result, err := reopened.Search(ctx, entry.Scope, "УТРЕННИЕ встречи")
	if err != nil || len(result) != 1 {
		t.Fatalf("search %#v %v", result, err)
	}
	group, err := reopened.Search(ctx, "telegram:group:456", "")
	if err != nil || len(group) != 0 {
		t.Fatalf("group memory %#v %v", group, err)
	}
	profile, err := reopened.Profile(ctx, entry.Scope)
	if err != nil || !strings.Contains(profile, entry.Content) {
		t.Fatalf("profile %q %v", profile, err)
	}
	entry.Content = "Теперь предпочитает вечерние встречи"
	entry.UpdatedAt = entry.UpdatedAt.Add(time.Hour)
	if err = reopened.Update(ctx, entry); err != nil {
		t.Fatal(err)
	}
	profile, _ = reopened.Profile(ctx, entry.Scope)
	if strings.Contains(profile, "утренние") || !strings.Contains(profile, "вечерние") {
		t.Fatal("profile stale after correction")
	}
	if err = reopened.Delete(ctx, entry.Scope, entry.ID); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Delete(ctx, entry.Scope, entry.ID); err != nil {
		t.Fatal(err)
	}
	profile, _ = reopened.Profile(ctx, entry.Scope)
	if strings.Contains(profile, "вечерние") {
		t.Fatal("deleted preference retained")
	}
	// Existing owner-edited files must survive process restarts.
	custom := "# My Jarvis\nCalm and concise.\n"
	if err = os.WriteFile(filepath.Join(root, "SOUL.md"), []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(root); err != nil {
		t.Fatal(err)
	}
	soul, _ = reopened.Soul(ctx)
	if soul != custom {
		t.Fatal("constructor overwrote owner personality")
	}
}
func TestKnowledgeConcurrentUpdatesDoNotLoseProfileFacts(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Update(ctx, domain.MemoryEntry{ID: fmt.Sprintf("pref-%02d", i), Scope: "private", Kind: "preference", Content: fmt.Sprintf("Preference %02d", i)})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	entries, err := s.Search(ctx, "private", "")
	if err != nil || len(entries) != 30 {
		t.Fatalf("memory facts %d %v", len(entries), err)
	}
	profile, err := s.Profile(ctx, "private")
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(profile, "- Preference"); count != 30 {
		t.Fatalf("profile lost updates: %d", count)
	}
}
func TestSkillProposalsNeverActivateThemselves(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	skills, err := s.Skills(ctx)
	if err != nil || len(skills) != 1 || skills[0].Name != "web-research" || skills[0].Content != "" {
		t.Fatalf("catalog %#v %v", skills, err)
	}
	old, err := s.ReadSkill(ctx, "web-research")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ProposeSkill(ctx, "web-research", "Changed research", "# New content"); err != nil {
		t.Fatal(err)
	}
	if err = s.ProposeSkill(ctx, "web-research", "Changed research", "# New content"); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ReadSkill(ctx, "web-research")
	if active != old {
		t.Fatal("proposal changed active skill")
	}
	files, err := os.ReadDir(filepath.Join(root, "skills/proposals/web-research"))
	if err != nil || len(files) != 1 {
		t.Fatalf("proposal retry not idempotent %d %v", len(files), err)
	}
	if err = s.ProposeSkill(ctx, "email", "Send email", "# Email skill"); err != nil {
		t.Fatal(err)
	}
	skills, _ = s.Skills(ctx)
	if len(skills) != 1 {
		t.Fatal("proposal leaked into active catalog")
	}
	for _, name := range []string{"../SOUL", "/tmp/evil", "..", "x/y", "x\\y"} {
		if err = s.ProposeSkill(ctx, name, "bad", "bad"); err == nil {
			t.Errorf("unsafe skill name accepted %q", name)
		}
		if _, err = s.ReadSkill(ctx, name); err == nil {
			t.Errorf("unsafe skill read accepted %q", name)
		}
	}
}
func TestMemoryIdentifiersCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	entry := domain.MemoryEntry{ID: "../../SOUL.md", Scope: "../../outside", Content: "A safely hashed ID"}
	if err = s.Update(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(context.Background(), entry.Scope, entry.ID)
	if err != nil || got.Content != entry.Content {
		t.Fatalf("hashed ID %#v %v", got, err)
	}
	soul, _ := s.Soul(context.Background())
	if soul != defaultSoul {
		t.Fatal("memory traversal overwrote SOUL.md")
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(root, "scopes", hash("another-scope"))); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(context.Background(), domain.MemoryEntry{ID: "fact", Scope: "another-scope", Content: "secret"}); err == nil {
		t.Fatal("symlinked scope accepted")
	}
	files, _ := os.ReadDir(outside)
	if len(files) > 0 {
		t.Fatal("symlink wrote outside root")
	}
	if _, err = s.safePath("../escape"); err == nil {
		t.Fatal("relative traversal accepted")
	}
	if _, err = s.safePath("/tmp/escape"); err == nil {
		t.Fatal("absolute traversal accepted")
	}
}
func TestCorruptedMemoryIsReported(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ensureDir(filepath.Join(scopeDir("scope"), "memories")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, memoryPath("scope", "x")), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Search(context.Background(), "scope", ""); err == nil {
		t.Fatal("corrupted memory silently ignored")
	}
}

func TestExternalSoulFileSurvivesMemoryWritesAndReloadsAtomicOwnerEdits(t *testing.T) {
	root := t.TempDir()
	configDir := t.TempDir()
	soulFile := filepath.Join(configDir, "SOUL.md")
	first := "# Owner character\nCalm and direct.\n"
	if err := os.WriteFile(soulFile, []byte(first), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewWithSoulFile(root, soulFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := store.Soul(ctx)
	if err != nil || got != first {
		t.Fatalf("external personality not loaded: %q %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(root, "SOUL.md")); !os.IsNotExist(err) {
		t.Fatal("unexpected competing default personality")
	}
	if err = store.Update(ctx, domain.MemoryEntry{ID: "pref", Scope: "owner", Kind: "preference", Content: "Morning meetings"}); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Soul(ctx)
	if got != first {
		t.Fatal("memory update rewrote character")
	}
	replacement := filepath.Join(configDir, "SOUL.tmp")
	second := "# Owner character\nWarm and concise.\n"
	if err = os.WriteFile(replacement, []byte(second), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, soulFile); err != nil {
		t.Fatal(err)
	}
	got, err = store.Soul(ctx)
	if err != nil || got != second {
		t.Fatalf("owner edit needs a restart: %q %v", got, err)
	}
	if _, err = NewWithSoulFile(root, soulFile); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Soul(ctx)
	if got != second {
		t.Fatal("restart replaced owner character")
	}
}
func TestExternalSoulRejectsMissingFileOrDirectory(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.md"), t.TempDir()} {
		if _, err := NewWithSoulFile(t.TempDir(), path); err == nil {
			t.Fatal("invalid external SOUL_FILE accepted")
		}
	}
}
