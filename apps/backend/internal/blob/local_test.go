package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

// Compile-time check that *Store satisfies inspector.ArtifactStore.
var _ interface {
	Artifact(context.Context, string, string) (domain.Artifact, bool, error)
	ListFiles(context.Context, string, string) (domain.StoredFiles, error)
	OpenFile(context.Context, string) (domain.StoredFile, io.ReadSeekCloser, error)
} = (*Store)(nil)

func setupLocal(t *testing.T, repo domain.Repository) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestNewValidation(t *testing.T) {
	repo := storage.NewMemory()
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := New(repo, bad); err == nil {
			t.Fatalf("expected error for dir %q", bad)
		}
	}

	base := filepath.Join(t.TempDir(), "nested", "artifacts")
	s, err := New(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("artifacts dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("artifacts path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("expected private permissions (0700), got %o", info.Mode().Perm())
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("ping failed: %v", err)
	}

	// Existing regular file at artifacts dir path must fail.
	conflictPath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(conflictPath, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(repo, conflictPath); err == nil {
		t.Fatal("expected error when artifacts path is a regular file")
	}
}

func TestArtifactsImmutableScopedAndConcurrent(t *testing.T) {
	ctx := context.Background()
	s, dir := setupLocal(t, storage.NewMemory())
	original := domain.Artifact{
		ID:             "notes",
		ConversationID: "a/b",
		Name:           "Заметки.txt",
		MIME:           "text/plain",
		Data:           []byte("first bytes"),
	}
	if err := s.SaveArtifact(ctx, original); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			retry := original
			retry.Name = "replacement"
			retry.Data = []byte(fmt.Sprint(i))
			if err := s.SaveArtifact(ctx, retry); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	got, ok, err := s.Artifact(ctx, original.ConversationID, original.ID)
	if err != nil || !ok || got.Name != original.Name || got.MIME != original.MIME || got.SizeBytes != int64(len(original.Data)) || string(got.Data) != "first bytes" {
		t.Fatalf("immutable artifact %#v %v %v", got, ok, err)
	}

	// Empty payload should also round-trip cleanly.
	emptyArt := domain.Artifact{ID: "empty", ConversationID: "a/b", Name: "empty.bin", MIME: "application/octet-stream", Data: []byte{}}
	if err := s.SaveArtifact(ctx, emptyArt); err != nil {
		t.Fatal(err)
	}
	gotEmpty, ok, err := s.Artifact(ctx, emptyArt.ConversationID, emptyArt.ID)
	if err != nil || !ok || len(gotEmpty.Data) != 0 || gotEmpty.SizeBytes != 0 {
		t.Fatalf("empty artifact %#v %v %v", gotEmpty, ok, err)
	}

	different := original
	different.ConversationID = "a_b"
	different.Data = []byte("different scope")
	if err = s.SaveArtifact(ctx, different); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.Artifact(ctx, different.ConversationID, different.ID)
	if err != nil || !ok || string(got.Data) != "different scope" {
		t.Fatalf("scope collision %#v %v %v", got, ok, err)
	}
	if _, ok, err = s.Artifact(ctx, "another", original.ID); err != nil || ok {
		t.Fatal("artifact leaked across conversations")
	}

	// Verify no leftover .tmp files exist and no nested artifacts/artifacts directory was created.
	if _, err := os.Stat(filepath.Join(dir, "artifacts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected nested artifacts directory: %v", err)
	}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), tmpSuffix) {
			t.Errorf("leftover temp file: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type failingRepository struct {
	domain.Repository
	mu       sync.Mutex
	failNext bool
	pingErr  error
}

func (r *failingRepository) SaveArtifact(ctx context.Context, a domain.Artifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return errors.New("simulated database commit failure")
	}
	return r.Repository.SaveArtifact(ctx, a)
}

func (r *failingRepository) Ping(ctx context.Context) error {
	r.mu.Lock()
	err := r.pingErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return r.Repository.Ping(ctx)
}

func TestReplayAfterObjectCommittedBeforeMetadata(t *testing.T) {
	ctx := context.Background()
	repo := &failingRepository{Repository: storage.NewMemory(), failNext: true}
	s, dir := setupLocal(t, repo)
	original := domain.Artifact{
		ID:             "same",
		ConversationID: "scope",
		Name:           "first.txt",
		MIME:           "text/plain",
		Data:           []byte("committed object"),
		CreatedAt:      time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	}
	if err := s.SaveArtifact(ctx, original); err == nil {
		t.Fatal("database failure was hidden")
	}
	if _, ok, _ := repo.Artifact(ctx, "scope", "same"); ok {
		t.Fatal("failed database committed metadata")
	}

	// Simulate process restart with a new Store pointing at the same directory and DB.
	restarted, err := New(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	retry := original
	retry.Name = "second.txt"
	retry.MIME = "application/json"
	retry.Data = []byte("replacement")
	retry.CreatedAt = time.Now().UTC()
	if err := restarted.SaveArtifact(ctx, retry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := restarted.Artifact(ctx, "scope", "same")
	if err != nil || !ok || got.Name != original.Name || got.MIME != original.MIME || !got.CreatedAt.Equal(original.CreatedAt) || string(got.Data) != string(original.Data) {
		t.Fatalf("replay lost original %#v %v %v", got, ok, err)
	}
}

func TestObjectIntegrity(t *testing.T) {
	ctx := context.Background()
	s, _ := setupLocal(t, storage.NewMemory())
	a := domain.Artifact{ID: "one", ConversationID: "scope", Name: "one.txt", MIME: "text/plain", Data: []byte("original")}
	if err := s.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}

	bodyPath := s.objectPath(key(a.ConversationID, a.ID))
	if err := os.WriteFile(bodyPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Artifact(ctx, a.ConversationID, a.ID); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("changed local object accepted: %v", err)
	}

	// Restore body but clear sha256 in sidecar.
	if err := os.WriteFile(bodyPath, a.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sidecarMetadata{Artifact: domain.Artifact{ID: a.ID, ConversationID: a.ConversationID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath(bodyPath), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Artifact(ctx, a.ConversationID, a.ID); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("missing digest accepted: %v", err)
	}
}

func TestScopeMismatchDetection(t *testing.T) {
	ctx := context.Background()
	s, _ := setupLocal(t, storage.NewMemory())
	a := domain.Artifact{ID: "one", ConversationID: "scope-a", Name: "one.txt", MIME: "text/plain", Data: []byte("secret")}
	if err := s.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}

	bodyPath := s.objectPath(key(a.ConversationID, a.ID))
	tampered, err := json.Marshal(sidecarMetadata{
		Artifact: domain.Artifact{ID: a.ID, ConversationID: "scope-b", Name: a.Name, MIME: a.MIME},
		SHA256:   digest(a.Data),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath(bodyPath), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Artifact(ctx, a.ConversationID, a.ID); err == nil || !strings.Contains(err.Error(), "scope mismatch") {
		t.Fatalf("scope mismatch accepted on read: %v", err)
	}

	// Crash-recovery path must also reject a mismatched sidecar.
	repo := storage.NewMemory()
	s2, _ := setupLocal(t, repo)
	k := key("scope-x", "art-1")
	p := s2.objectPath(k)
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("disk-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	badMeta, err := json.Marshal(sidecarMetadata{
		Artifact: domain.Artifact{ID: "art-1", ConversationID: "scope-y"},
		SHA256:   digest([]byte("disk-bytes")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath(p), badMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s2.SaveArtifact(ctx, domain.Artifact{ID: "art-1", ConversationID: "scope-x", Data: []byte("new")}); err == nil || !strings.Contains(err.Error(), "scope mismatch") {
		t.Fatalf("scope mismatch accepted on recovery: %v", err)
	}
}

func TestObjectKeysNeverCollideBySanitization(t *testing.T) {
	for _, pair := range [][2]string{{"a/b", "a_b"}, {"a\\b", "a_b"}, {"../a", "__a"}, {"a/b", "a\\b"}} {
		if key(pair[0], "id") == key(pair[1], "id") {
			t.Fatalf("scope keys collide: %v", pair)
		}
		if key("scope", pair[0]) == key("scope", pair[1]) {
			t.Fatalf("ID keys collide: %v", pair)
		}
	}
}

func TestRejectInvalidAndOversizedArtifacts(t *testing.T) {
	ctx := context.Background()
	s, dir := setupLocal(t, storage.NewMemory())
	invalid := []domain.Artifact{
		{ID: "one"},
		{ID: "  ", ConversationID: "scope"},
		{ConversationID: "scope"},
		{ID: "one", ConversationID: "  "},
		{ID: "one", ConversationID: "scope", Data: make([]byte, maxArtifactSize+1)},
	}
	for _, a := range invalid {
		if err := s.SaveArtifact(ctx, a); err == nil {
			t.Fatalf("invalid artifact accepted: id=%q conv=%q size=%d", a.ID, a.ConversationID, len(a.Data))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid artifact wrote files: %d", len(entries))
	}

	// Exact 20 MiB boundary succeeds and round-trips.
	exact := bytes.Repeat([]byte("a"), maxArtifactSize)
	if err := s.SaveArtifact(ctx, domain.Artifact{ID: "exact", ConversationID: "scope", Name: "exact.bin", Data: exact}); err != nil {
		t.Fatalf("exact 20 MiB artifact rejected: %v", err)
	}
	got, ok, err := s.Artifact(ctx, "scope", "exact")
	if err != nil || !ok || len(got.Data) != maxArtifactSize {
		t.Fatalf("exact 20 MiB read failed: ok=%v len=%d err=%v", ok, len(got.Data), err)
	}

	// Disk file grown beyond 20 MiB is rejected on read.
	bodyPath := s.objectPath(key("scope", "exact"))
	f, err := os.OpenFile(bodyPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	if _, _, err := s.Artifact(ctx, "scope", "exact"); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized disk file accepted: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.SaveArtifact(cancelled, domain.Artifact{ID: "c", ConversationID: "s", Data: []byte("x")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveArtifact canceled: %v", err)
	}
	if _, _, err := s.Artifact(cancelled, "scope", "exact"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Artifact canceled: %v", err)
	}
}

func TestPing(t *testing.T) {
	ctx := context.Background()
	repo := &failingRepository{Repository: storage.NewMemory()}
	s, dir := setupLocal(t, repo)

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("healthy ping failed: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Ping(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ping: %v", err)
	}

	repoErr := errors.New("db down")
	repo.pingErr = repoErr
	if err := s.Ping(ctx); !errors.Is(err, repoErr) {
		t.Fatalf("expected repo error, got: %v", err)
	}
	repo.pingErr = nil

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err == nil {
		t.Fatal("ping succeeded after artifacts directory was removed")
	}
}
