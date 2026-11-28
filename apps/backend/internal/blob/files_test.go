package blob

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

func writeDiskFile(t *testing.T, baseDir, objectKey string, data []byte, modTime time.Time) {
	t.Helper()
	rel := strings.TrimPrefix(objectKey, "artifacts/")
	full := filepath.Join(baseDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(full, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAndOpenLocalFiles(t *testing.T) {
	ctx := context.Background()
	s, dir := setupLocal(t, storage.NewMemory())

	art := domain.Artifact{
		ID:             "notes",
		ConversationID: "conv-1",
		Name:           "Заметки.txt",
		MIME:           "text/plain",
		Data:           []byte("hello world"),
	}
	if err := s.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	art2 := domain.Artifact{
		ID:             "img",
		ConversationID: "conv-2",
		Name:           "photo.png",
		MIME:           "image/png",
		Data:           []byte{0x89, 'P', 'N', 'G'},
	}
	if err := s.SaveArtifact(ctx, art2); err != nil {
		t.Fatal(err)
	}

	// Internal .tmp file must be ignored by ListFiles.
	writeDiskFile(t, dir, "artifacts/inprogress.tmp", []byte("partial"), time.Now().UTC())

	page, err := s.ListFiles(ctx, "artifacts/", "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Bucket != "local" {
		t.Fatalf("expected bucket local, got %q", page.Bucket)
	}
	if len(page.Files) != 2 || page.NextAfter != "" {
		t.Fatalf("unexpected files: %#v", page)
	}

	// Prefix filter by scope hash returns only that conversation's artifact.
	scopePrefix := "artifacts/" + digest([]byte(art.ConversationID)) + "/"
	page, err = s.ListFiles(ctx, scopePrefix, "")
	if err != nil || len(page.Files) != 1 {
		t.Fatalf("scope prefix: %#v %v", page, err)
	}
	got := page.Files[0]
	if got.Key != key(art.ConversationID, art.ID) || got.Name != art.Name || got.MIME != art.MIME || got.Size != int64(len(art.Data)) || got.ConversationID != art.ConversationID || got.ArtifactID != art.ID || got.ModifiedAt.IsZero() {
		t.Fatalf("artifact metadata mismatch: %#v", got)
	}

	// OpenFile returns metadata and a working io.ReadSeekCloser.
	info, reader, err := s.OpenFile(ctx, key(art.ConversationID, art.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if info.Name != art.Name || info.MIME != art.MIME || info.ArtifactID != art.ID || info.ConversationID != art.ConversationID || info.Size != int64(len(art.Data)) {
		t.Fatalf("open stat mismatch: %#v", info)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "hello world" {
		t.Fatalf("read content: %q %v", data, err)
	}
	if pos, err := reader.Seek(6, io.SeekStart); err != nil || pos != 6 {
		t.Fatalf("seek failed: pos=%d err=%v", pos, err)
	}
	tail, err := io.ReadAll(reader)
	if err != nil || string(tail) != "world" {
		t.Fatalf("read after seek: %q %v", tail, err)
	}

	// Non-matching prefix yields empty non-nil slice.
	page, err = s.ListFiles(ctx, "nothing/", "")
	if err != nil || page.Files == nil || len(page.Files) != 0 || page.NextAfter != "" {
		t.Fatalf("empty prefix: %#v %v", page, err)
	}

	// Canceled context is respected.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListFiles(cancelled, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListFiles canceled: %v", err)
	}
	if _, _, err := s.OpenFile(cancelled, key(art.ConversationID, art.ID)); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenFile canceled: %v", err)
	}
}

func TestListFilesNewestFirstPaginationAndPrefix(t *testing.T) {
	ctx := context.Background()
	s, dir := setupLocal(t, storage.NewMemory())

	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for i := range 250 {
		writeDiskFile(t, dir, fmt.Sprintf("artifacts/batch/%04d", i), []byte("raw"), base.Add(time.Duration(i)*time.Minute))
	}
	writeDiskFile(t, dir, "artifacts/other/item", []byte("other"), base.Add(1000*time.Minute))

	page1, err := s.ListFiles(ctx, "artifacts/batch/", "")
	if err != nil || page1.Bucket != "local" || len(page1.Files) != 100 || page1.NextAfter == "" {
		t.Fatalf("page1: len=%d next=%q err=%v", len(page1.Files), page1.NextAfter, err)
	}
	if page1.Files[0].Key != "artifacts/batch/0249" || page1.Files[99].Key != "artifacts/batch/0150" {
		t.Fatalf("page1 order: first=%s last=%s", page1.Files[0].Key, page1.Files[99].Key)
	}

	page2, err := s.ListFiles(ctx, "artifacts/batch/", page1.NextAfter)
	if err != nil || len(page2.Files) != 100 || page2.NextAfter == "" {
		t.Fatalf("page2: len=%d next=%q err=%v", len(page2.Files), page2.NextAfter, err)
	}
	if page2.Files[0].Key != "artifacts/batch/0149" || page2.Files[99].Key != "artifacts/batch/0050" {
		t.Fatalf("page2 order: first=%s last=%s", page2.Files[0].Key, page2.Files[99].Key)
	}

	page3, err := s.ListFiles(ctx, "artifacts/batch/", page2.NextAfter)
	if err != nil || len(page3.Files) != 50 || page3.NextAfter != "" {
		t.Fatalf("page3: len=%d next=%q err=%v", len(page3.Files), page3.NextAfter, err)
	}
	if page3.Files[0].Key != "artifacts/batch/0049" || page3.Files[49].Key != "artifacts/batch/0000" {
		t.Fatalf("page3 order: first=%s last=%s", page3.Files[0].Key, page3.Files[49].Key)
	}

	// Narrow prefix filter matches only 0200..0249.
	sub, err := s.ListFiles(ctx, "artifacts/batch/02", "")
	if err != nil || len(sub.Files) != 50 || sub.NextAfter != "" || sub.Files[0].Key != "artifacts/batch/0249" || sub.Files[49].Key != "artifacts/batch/0200" {
		t.Fatalf("narrow prefix: len=%d next=%q err=%v", len(sub.Files), sub.NextAfter, err)
	}
}

func TestFileDatePaginationTiesAndDeletedCursor(t *testing.T) {
	ctx := context.Background()
	s, dir := setupLocal(t, storage.NewMemory())

	sameTime := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for i := range 205 {
		writeDiskFile(t, dir, fmt.Sprintf("artifacts/ties/%03d", i), []byte("x"), sameTime)
	}

	var after string
	for pageNo, count := range []int{100, 100, 5} {
		page, err := s.ListFiles(ctx, "artifacts/ties/", after)
		if err != nil || len(page.Files) != count {
			t.Fatalf("page %d: %d %v", pageNo, len(page.Files), err)
		}
		for i, file := range page.Files {
			expectedKey := fmt.Sprintf("artifacts/ties/%03d", pageNo*100+i)
			if file.Key != expectedKey {
				t.Fatalf("tie order at page %d index %d: got %s want %s", pageNo, i, file.Key, expectedKey)
			}
		}
		if pageNo < 2 && page.NextAfter == "" {
			t.Fatal("missing continuation")
		}
		if pageNo == 2 && page.NextAfter != "" {
			t.Fatal("unexpected continuation")
		}
		after = page.NextAfter
		// The following page must not require the previous cursor object to exist.
		lastPath := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(page.Files[len(page.Files)-1].Key, "artifacts/")))
		if err := os.Remove(lastPath); err != nil {
			t.Fatal(err)
		}
	}

	missingTime, _ := json.Marshal(filePosition{Key: "artifacts/ties/000"})
	missingKey, _ := json.Marshal(filePosition{ModifiedAt: sameTime})
	invalidCursors := []string{
		"old-key",
		strings.Repeat("x", 4097),
		"e30",
		base64.RawURLEncoding.EncodeToString(missingTime),
		base64.RawURLEncoding.EncodeToString(missingKey),
	}
	for _, invalid := range invalidCursors {
		if _, err := s.ListFiles(ctx, "", invalid); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("invalid cursor %q accepted: %v", invalid, err)
		}
	}
}

func TestOpenFileSecurityAndTraversalRejection(t *testing.T) {
	ctx := context.Background()
	s, dir := setupLocal(t, storage.NewMemory())

	art := domain.Artifact{ID: "doc", ConversationID: "scope", Name: "doc.txt", MIME: "text/plain", Data: []byte("safe")}
	if err := s.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	artKey := key(art.ConversationID, art.ID)

	// Files outside ARTIFACTS_DIR and internal files inside ARTIFACTS_DIR.
	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDiskFile(t, dir, "artifacts/stray.tmp", []byte("temp"), time.Now().UTC())

	// Symlink file and symlinked directory inside ARTIFACTS_DIR pointing outside.
	if err := os.Symlink(secretPath, filepath.Join(dir, "link-escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(dir, "dir-escape")); err != nil {
		t.Fatal(err)
	}

	rejected := []string{
		"",
		"missing",
		"secret.txt",
		"/etc/passwd",
		"/" + artKey,
		"artifacts",
		"artifacts/",
		"artifacts/.",
		"artifacts/..",
		"artifacts/../secret.txt",
		"artifacts/sub/../../secret.txt",
		"artifacts/a/../b",
		"artifacts/a//b",
		"artifacts/a/./b",
		`artifacts\a\b`,
		`artifacts/a\..\secret.txt`,
		"artifacts/a\x00b",
		artKey + ".meta.json",
		artKey + ".META.JSON",
		"artifacts/stray.tmp",
		"artifacts/stray.TMP",
		"artifacts/" + digest([]byte(art.ConversationID)), // directory, not regular file
		"artifacts/link-escape",
		"artifacts/dir-escape/secret.txt",
	}
	for _, badKey := range rejected {
		if _, reader, err := s.OpenFile(ctx, badKey); !errors.Is(err, fs.ErrNotExist) {
			if reader != nil {
				_ = reader.Close()
			}
			t.Fatalf("OpenFile(%q) expected fs.ErrNotExist, got %v", badKey, err)
		}
	}

	// ListFiles must also skip symlinks and internal files.
	page, err := s.ListFiles(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Files) != 1 || page.Files[0].Key != artKey {
		t.Fatalf("ListFiles leaked unexpected entries: %#v", page.Files)
	}
}
