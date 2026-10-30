package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGroupRulesCreateEditReloadAndClear(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GroupRules(ctx, -20); err != nil || got != "" {
		t.Fatalf("missing file: %q %v", got, err)
	}
	const initial = "# Правила\nОтвечай по обращению.\nОтвечай кратко.\n"
	if err := s.UpdateGroupRules(ctx, -20, "", initial); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGroupRules(ctx, -20, "Отвечай кратко.", "Давай подробный ответ, когда его просят."); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(initial, "Отвечай кратко.", "Давай подробный ответ, когда его просят.", 1)
	if got, err := reopened.GroupRules(ctx, -20); err != nil || got != want {
		t.Fatalf("edit lost after reopen: %q %v", got, err)
	}
	if got, err := s.GroupRules(ctx, -30); err != nil || got != "" {
		t.Fatalf("rules leaked to another group: %q %v", got, err)
	}
	// Direct owner edits are picked up without reopening the store.
	path := filepath.Join(root, "groups", "-20", "GROUP.md")
	if err := os.WriteFile(path, []byte("Новые правила"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GroupRules(ctx, -20); err != nil || got != "Новые правила" {
		t.Fatalf("owner edit not reloaded: %q %v", got, err)
	}
	if err := s.UpdateGroupRules(ctx, -20, "Новые правила", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GroupRules(ctx, -20); err != nil || got != "" {
		t.Fatalf("clear failed: %q %v", got, err)
	}
}

func TestGroupRulesRejectStaleAmbiguousInvalidAndOversizedEdits(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const initial = "Кратко. Кратко. По делу."
	if err := s.UpdateGroupRules(ctx, -20, "", initial); err != nil {
		t.Fatal(err)
	}
	for _, edit := range [][2]string{
		{"Кратко.", "Подробно."},
		{"missing", "new"},
		{"", "replace without reading"},
		{initial, initial},
		{initial, strings.Repeat("я", 4001)},
		{initial, string([]byte{0xff})},
	} {
		if err := s.UpdateGroupRules(ctx, -20, edit[0], edit[1]); err == nil {
			t.Fatal("invalid edit accepted")
		}
		if got, err := s.GroupRules(ctx, -20); err != nil || got != initial {
			t.Fatalf("rejected edit changed rules: %q %v", got, err)
		}
	}
	for _, id := range []int64{0, 7} {
		if _, err := s.GroupRules(ctx, id); err == nil {
			t.Fatal("private/non-group ID accepted for reading")
		}
		if err := s.UpdateGroupRules(ctx, id, "", "rules"); err == nil {
			t.Fatal("private/non-group ID accepted for writing")
		}
	}
}

func TestGroupRulesCannotFollowSymlinkToPrivateFile(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "USER.md")
	if err := os.WriteFile(private, []byte("private profile"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "groups", "-20")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(dir, "GROUP.md")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GroupRules(context.Background(), -20); err == nil || got != "" {
		t.Fatalf("symlink leaked private data: %q %v", got, err)
	}
	if err := s.UpdateGroupRules(context.Background(), -20, "private profile", "changed"); err == nil {
		t.Fatal("symlink accepted for writing")
	}
	if got, err := os.ReadFile(private); err != nil || string(got) != "private profile" {
		t.Fatalf("symlink changed private file: %q %v", got, err)
	}
}
