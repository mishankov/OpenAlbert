package knowledge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
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
	want := entry
	want.CreatedAt = entry.UpdatedAt
	if err != nil || !reflect.DeepEqual(got, want) {
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
	custom := "# My OpenAlbert\nCalm and concise.\n"
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

func TestRebuildProfileOrdersProfilesBeforePreferencesByRecency(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	// Deliberately choose lexicographically inverse IDs so ID sorting would invert the expected order.
	entries := []domain.MemoryEntry{
		{ID: "000-pref-newest", Scope: "personal:7", Kind: "preference", Content: "Pref newest", UpdatedAt: base.Add(4 * time.Hour)},
		{ID: "111-pref-older", Scope: "personal:7", Kind: "preference", Content: "Pref older", UpdatedAt: base.Add(1 * time.Hour)},
		{ID: "zzz-profile-older", Scope: "personal:7", Kind: "profile", Content: "Profile older", UpdatedAt: base},
		{ID: "yyy-profile-newer", Scope: "personal:7", Kind: "profile", Content: "Profile newer", UpdatedAt: base.Add(2 * time.Hour)},
		{ID: "aaa-fact", Scope: "personal:7", Kind: "fact", Content: "Fact excluded from USER.md", UpdatedAt: base.Add(5 * time.Hour)},
	}
	for _, entry := range entries {
		if err := s.Update(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := s.Profile(ctx, "personal:7")
	if err != nil {
		t.Fatal(err)
	}
	want := "# User profile\n\n- Profile newer\n- Profile older\n- Pref newest\n- Pref older\n"
	if profile != want {
		t.Fatalf("USER.md ordering mismatch:\ngot:\n%s\nwant:\n%s", profile, want)
	}
}

func TestArchiveMemoryAndPruneArchive(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oldEntry := domain.MemoryEntry{
		ID:        "old-pref",
		Scope:     "personal:7",
		Kind:      "preference",
		Content:   "Original preference content",
		Source:    "session-1",
		CreatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
	}
	recentEntry := domain.MemoryEntry{
		ID:        "recent-pref",
		Scope:     "personal:7",
		Kind:      "preference",
		Content:   "Recent preference content",
		Source:    "session-2",
		CreatedAt: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
	}
	for _, entry := range []domain.MemoryEntry{oldEntry, recentEntry} {
		if err := s.Update(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	oldDay := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	recentDay := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if applied, reason, err := s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     oldEntry.Scope,
		RunID:     "run-old",
		Now:       oldDay,
		Expected:  []domain.MemoryEntry{oldEntry},
		DeleteIDs: []string{oldEntry.ID},
	}); err != nil || !applied || reason != "" {
		t.Fatalf("archive old failed: applied=%v reason=%q err=%v", applied, reason, err)
	}
	if applied, reason, err := s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     recentEntry.Scope,
		RunID:     "run-recent",
		Now:       recentDay,
		Expected:  []domain.MemoryEntry{recentEntry},
		DeleteIDs: []string{recentEntry.ID},
	}); err != nil || !applied || reason != "" {
		t.Fatalf("archive recent failed: applied=%v reason=%q err=%v", applied, reason, err)
	}
	recentArchive := filepath.Join(root, archivePath(recentEntry.Scope, "run-recent", recentEntry, recentDay))
	raw, err := os.ReadFile(recentArchive)
	if err != nil {
		t.Fatalf("missing archived file: %v", err)
	}
	parsed, err := parseMemory(raw)
	if err != nil || !reflect.DeepEqual(parsed, recentEntry) {
		t.Fatalf("archived memory mismatch: %+v %v", parsed, err)
	}
	if err := s.PruneArchive(ctx, oldEntry.Scope, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(root, scopeDir(oldEntry.Scope), "archive", "2026-10-01")
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("expired archive directory not pruned: %v", err)
	}
	if _, err := os.Stat(recentArchive); err != nil {
		t.Fatalf("recent archive unexpectedly pruned: %v", err)
	}
}

func TestApplyMemoryMutationPreservesOriginalArchiveAcrossTwoMergesInSameRun(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	orig := domain.MemoryEntry{ID: "surv", Scope: "personal:7", Kind: "preference", Content: "Original v1", Source: "s1", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)}
	dup1 := domain.MemoryEntry{ID: "dup1", Scope: "personal:7", Kind: "preference", Content: "Dup 1", Source: "s2", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	dup2 := domain.MemoryEntry{ID: "dup2", Scope: "personal:7", Kind: "preference", Content: "Dup 2", Source: "s3", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	for _, e := range []domain.MemoryEntry{orig, dup1, dup2} {
		if err := s.Update(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// Merge 1: orig + dup1 -> surv v2.
	v2 := orig
	v2.Content = "Merged v2"
	v2.Source = "s1; s2"
	v2.UpdatedAt = now
	if applied, reason, err := s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     orig.Scope,
		RunID:     "same-run",
		Now:       now,
		Expected:  []domain.MemoryEntry{orig, dup1},
		Update:    &v2,
		DeleteIDs: []string{dup1.ID},
	}); err != nil || !applied || reason != "" {
		t.Fatalf("first merge failed: applied=%v reason=%q err=%v", applied, reason, err)
	}

	// Merge 2 in the SAME run: v2 + dup2 -> surv v3.
	v3 := v2
	v3.Content = "Merged v3"
	v3.Source = "s1; s2; s3"
	v3.UpdatedAt = now.Add(time.Second)
	if applied, reason, err := s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     orig.Scope,
		RunID:     "same-run",
		Now:       now,
		Expected:  []domain.MemoryEntry{v2, dup2},
		Update:    &v3,
		DeleteIDs: []string{dup2.ID},
	}); err != nil || !applied || reason != "" {
		t.Fatalf("second merge failed: applied=%v reason=%q err=%v", applied, reason, err)
	}

	// Both v1 (original) and v2 (intermediate) archive files must exist intact.
	origRaw, err := os.ReadFile(filepath.Join(root, archivePath(orig.Scope, "same-run", orig, now)))
	if err != nil {
		t.Fatalf("original v1 archive overwritten or missing: %v", err)
	}
	parsedOrig, err := parseMemory(origRaw)
	if err != nil || !reflect.DeepEqual(parsedOrig, orig) {
		t.Fatalf("original v1 archive corrupted: %+v %v", parsedOrig, err)
	}
	v2Raw, err := os.ReadFile(filepath.Join(root, archivePath(orig.Scope, "same-run", v2, now)))
	if err != nil {
		t.Fatalf("intermediate v2 archive missing: %v", err)
	}
	parsedV2, err := parseMemory(v2Raw)
	if err != nil || !reflect.DeepEqual(parsedV2, v2) {
		t.Fatalf("intermediate v2 archive corrupted: %+v %v", parsedV2, err)
	}
}

func TestApplyMemoryMutationAtomicGuardsAndInterruptedMergeRecovery(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	e1 := domain.MemoryEntry{ID: "e1", Scope: "personal:7", Kind: "preference", Content: "First", Source: "s1", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	e2 := domain.MemoryEntry{ID: "e2", Scope: "personal:7", Kind: "preference", Content: "Second", Source: "s2", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	for _, e := range []domain.MemoryEntry{e1, e2} {
		if err := s.Update(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	// Concurrent deletion of survivor must NOT be resurrected by ApplyMemoryMutation.
	if err := s.Delete(ctx, e1.Scope, e1.ID); err != nil {
		t.Fatal(err)
	}
	merged := e1
	merged.Content = "Merged"
	merged.Source = "s1; s2"
	merged.OriginSource = "s1"
	merged.UpdatedAt = now
	applied, reason, err := s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     e1.Scope,
		RunID:     "run-deleted-survivor",
		Now:       now,
		Expected:  []domain.MemoryEntry{e1, e2},
		Update:    &merged,
		DeleteIDs: []string{e2.ID},
	})
	if err != nil || applied || reason != "missing" {
		t.Fatalf("deleted survivor not rejected: applied=%v reason=%q err=%v", applied, reason, err)
	}
	if _, err := s.Read(ctx, e1.Scope, e1.ID); !os.IsNotExist(err) {
		t.Fatalf("deleted survivor was resurrected: %v", err)
	}

	// Simulate an interrupted merge: survivor already equals *mut.Update on disk,
	// while absorbed e2 is still live and matches its snapshot.
	if err := s.Update(ctx, merged); err != nil {
		t.Fatal(err)
	}
	applied, reason, err = s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     e1.Scope,
		RunID:     "run-recover",
		Now:       now,
		Expected:  []domain.MemoryEntry{e1, e2},
		Update:    &merged,
		DeleteIDs: []string{e2.ID},
	})
	if err != nil || !applied || reason != "" {
		t.Fatalf("interrupted merge did not reconcile: applied=%v reason=%q err=%v", applied, reason, err)
	}
	if _, err := s.Read(ctx, e1.Scope, e2.ID); !os.IsNotExist(err) {
		t.Fatalf("absorbed e2 remained live after merge retry: %v", err)
	}

	// Interrupted merge with a concurrently modified absorbed entry must NOT
	// delete other untouched absorbed entries and must report concurrent_modification.
	c1 := domain.MemoryEntry{ID: "c1", Scope: "personal:7", Kind: "preference", Content: "C1", Source: "s1", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)}
	c2 := domain.MemoryEntry{ID: "c2", Scope: "personal:7", Kind: "preference", Content: "C2", Source: "s2", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	c3 := domain.MemoryEntry{ID: "c3", Scope: "personal:7", Kind: "preference", Content: "C3", Source: "s3", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	for _, e := range []domain.MemoryEntry{c1, c2, c3} {
		if err := s.Update(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	cMerged := c1
	cMerged.Content = "C1+C2+C3"
	cMerged.Source = "s1; s2; s3"
	cMerged.OriginSource = "s1"
	cMerged.UpdatedAt = now
	// Survivor already updated, c2 concurrently edited, c3 untouched.
	if err := s.Update(ctx, cMerged); err != nil {
		t.Fatal(err)
	}
	c2Edited := c2
	c2Edited.Content = "Live edit on C2"
	c2Edited.UpdatedAt = now.Add(time.Minute)
	if err := s.Update(ctx, c2Edited); err != nil {
		t.Fatal(err)
	}
	applied, reason, err = s.ApplyMemoryMutation(ctx, domain.MemoryMutation{
		Scope:     c1.Scope,
		RunID:     "run-recover-conflict",
		Now:       now,
		Expected:  []domain.MemoryEntry{c1, c2, c3},
		Update:    &cMerged,
		DeleteIDs: []string{c3.ID, c2.ID},
	})
	if err != nil || applied || reason != "concurrent_modification" {
		t.Fatalf("interrupted merge ignored concurrent absorbed edit: applied=%v reason=%q err=%v", applied, reason, err)
	}
	gotC2, err := s.Read(ctx, c1.Scope, c2.ID)
	if err != nil || gotC2.Content != "Live edit on C2" {
		t.Fatalf("concurrent edit on c2 lost: %+v %v", gotC2, err)
	}
	if _, err := s.Read(ctx, c1.Scope, c3.ID); err != nil {
		t.Fatalf("untouched c3 was deleted despite conflict on c2: %v", err)
	}
}
