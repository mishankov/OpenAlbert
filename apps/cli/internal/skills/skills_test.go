package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

func TestDiscoverPrecedenceAndInvoke(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	sub := filepath.Join(repo, "pkg", "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	writeSkill := func(base, name, body string) {
		t.Helper()
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(filepath.Join(home, ".agents", "skills"), "shared", "---\nname: shared\ndescription: User version\n---\nuser body")
	writeSkill(filepath.Join(home, ".claude", "skills"), "user-only", "---\nname: user-only\ndescription: Only in home\n---\nhome body")
	writeSkill(filepath.Join(repo, ".agents", "skills"), "shared", "---\nname: shared\ndescription: Workspace version\n---\nworkspace body")

	reg := Discover(sub, home, []serverclient.ServerSkill{
		{Name: "shared", Description: "Server version"},
		{Name: "server-only", Description: "From OpenAlbert"},
	}, func(ctx context.Context, name string) (string, error) {
		return "remote body for " + name, nil
	})

	list := reg.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 skills, got %+v", list)
	}
	inv, err := reg.Invoke(context.Background(), "shared", "--strict")
	if err != nil || inv.Source != "workspace" || !strings.Contains(inv.Content, "workspace body") || inv.Args != "--strict" {
		t.Fatalf("workspace precedence failed: %+v %v", inv, err)
	}
	inv2, err := reg.Invoke(context.Background(), "server-only", "")
	if err != nil || inv2.Source != "server" || inv2.Content != "remote body for server-only" {
		t.Fatalf("server skill invoke failed: %+v %v", inv2, err)
	}
	inv3, err := reg.Invoke(context.Background(), "/user-only", "")
	if err != nil || inv3.Source != "user" || !strings.Contains(inv3.Content, "home body") {
		t.Fatalf("user skill invoke failed: %+v %v", inv3, err)
	}
}
