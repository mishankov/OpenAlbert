package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSoulEditsPersistAndRejectAmbiguousOrEmptyReplacements(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "knowledge"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := ""
			if external {
				path = filepath.Join(t.TempDir(), "SOUL.md")
				if err := os.WriteFile(path, []byte(defaultSoul), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := NewWithSoulFile(root, path)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			before, _ := store.Soul(ctx)
			for _, edit := range [][2]string{
				{"OpenAlbert", "Атлас"}, // occurs in both heading and introduction
				{"missing text", "new"},
				{before, " "},
				{before, strings.Repeat("я", 12001)},
			} {
				if err := store.UpdateSoul(ctx, edit[0], edit[1]); err == nil {
					t.Fatal("invalid edit accepted")
				}
				after, _ := store.Soul(ctx)
				if before != after {
					t.Fatal("invalid edit modified file")
				}
			}
			if err := store.UpdateSoul(ctx, "Ты — OpenAlbert,", "Ты — Атлас,"); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewWithSoulFile(root, path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Soul(ctx)
			if err != nil || got != strings.Replace(before, "Ты — OpenAlbert,", "Ты — Атлас,", 1) {
				t.Fatalf("edit did not survive reopening: %q %v", got, err)
			}
		})
	}
}
