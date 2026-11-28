package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func TestEndToEndOnlineBackupRetentionAndRestore(t *testing.T) {
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "installation")
	workDir := t.TempDir()
	createTestSQLiteDB(t, filepath.Join(root, "data", "openalbert.db"))
	files := map[string]string{
		".env":                    "BOT_TOKEN=secret-must-not-leak\n",
		"compose.yaml":            "services: {}\n",
		"models.toml":             "# model configuration\n",
		"models.env":              "OPENAI_API_KEY=secret-model-key\n",
		"data/knowledge/SOUL.md":  "Albert remembers everything\n",
		"data/artifacts/doc.txt":  "local blob artifact\n",
		"data/chatgpt/token.json": `{"refresh":"synthetic"}`,
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	configPath := filepath.Join(t.TempDir(), "backup.json")
	rawConfig := map[string]any{
		"root":          root,
		"compose_file":  "compose.yaml",
		"work_dir":      workDir,
		"enabled":       true,
		"time":          "03:00",
		"timezone":      "Europe/Moscow",
		"keep_days":     2,
		"s3_endpoint":   "s3.example.test:9000",
		"s3_region":     "us-east-1",
		"s3_secure":     false,
		"s3_bucket":     "backups",
		"s3_prefix":     "e2e/",
		"age_recipient": key.Recipient().String(),
	}
	if err := AtomicJSON(configPath, rawConfig); err != nil {
		t.Fatal(err)
	}

	c, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.NoticeSeconds != 0 {
		t.Fatalf("expected default notice_seconds=0, got %d", c.NoticeSeconds)
	}
	if c.StateDir != filepath.Join(root, "backup-state") {
		t.Fatalf("unexpected default state_dir: %s", c.StateDir)
	}

	r, err := NewRunner(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	remote := &memoryRemote{}
	r.Remote = remote

	if err := (&Runner{Config: c}).Request(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	serveDone := make(chan error, 1)
	go func() { serveDone <- r.Serve(ctx, io.Discard) }()
	joined := false
	joinServe := func() error {
		if joined {
			return nil
		}
		joined = true
		cancel()
		return <-serveDone
	}
	t.Cleanup(func() { _ = joinServe() })

	reader := Reader{Dir: c.StateDir}
	waitCompleted := func(prevRunID string) State {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			if s, e := reader.Read(); e == nil && s.RunID != prevRunID {
				if s.Phase == "completed" {
					return s
				}
				if s.Phase == "failed" {
					t.Fatalf("backup failed: %+v", s)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("timed out waiting for backup completion")
		return State{}
	}

	first := waitCompleted("")
	if first.LastSuccess == nil {
		t.Fatal("missing first LastSuccess")
	}

	// Verify public context never leaks credentials or secrets.
	ctxText := reader.Context(time.Now().UTC())
	if strings.Contains(ctxText, "secret-must-not-leak") || strings.Contains(ctxText, "secret-model-key") {
		t.Fatal("secret leaked into Reader.Context")
	}

	// Seed two earlier dates, then trigger a second backup today:
	// keep_days=2 must drop 2020-01-01 while preserving 2020-01-02 and both copies from today.
	for i, day := range []string{"2020-01-01", "2020-01-02"} {
		at, _ := time.Parse(time.DateOnly, day)
		old := *first.LastSuccess
		old.ID = fmt.Sprintf("2020010%dT000000Z-0000000000000000", i+1)
		old.Day = day
		old.CreatedAt = at
		old.Key = c.Prefix + "copies/" + old.ID + "/archive.tar.gz.age"
		remote.seed(old)
	}
	// The first run publishes "completed" just before releasing the flock, so
	// retry ErrBusy within the test deadline when queueing the second request.
	var reqErr error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		reqErr = (&Runner{Config: c}).Request()
		if reqErr == nil || !errors.Is(reqErr, ErrBusy) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if reqErr != nil {
		t.Fatal(reqErr)
	}
	second := waitCompleted(first.RunID)
	if err := joinServe(); err != nil {
		t.Fatal(err)
	}
	assertWorkDirEmpty(t, workDir)

	copies, err := remote.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 3 {
		t.Fatalf("expected 3 copies (2 today + 2020-01-02), got %+v", copies)
	}
	for _, cp := range copies {
		if cp.Day == "2020-01-01" {
			t.Fatal("expired date 2020-01-01 survived rotation")
		}
	}

	events, err := reader.Events()
	if err != nil || len(events) < 4 {
		t.Fatalf("expected starting/completed events for both runs: %+v %v", events, err)
	}

	archivePath := filepath.Join(t.TempDir(), "latest.age")
	if err := os.WriteFile(archivePath, remote.archiveBytes(second.LastSuccess.ID), 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := Restore(context.Background(), archivePath, identity, dest); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := readJSON(filepath.Join(dest, "backup-manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != ManifestVersion || m.ComposeFile != "compose.yaml" || m.ModelConfigFile != "models.toml" || m.ModelCredentialsFile != "models.env" {
		t.Fatalf("unexpected restored manifest: %+v", m)
	}
	for rel, want := range files {
		if strings.HasPrefix(rel, "data/chatgpt/") {
			if _, err := os.Lstat(filepath.Join(dest, rel)); !os.IsNotExist(err) {
				t.Fatalf("excluded %s was restored: %v", rel, err)
			}
			continue
		}
		got, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil || string(got) != want {
			t.Fatalf("restored %s mismatch: %q %v", rel, got, err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dest, "data", "openalbert.db"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("restored DB integrity_check=%q err=%v", check, err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM notes").Scan(&count); err != nil || count != 2 {
		t.Fatalf("restored DB rows: count=%d err=%v", count, err)
	}
}

// Optional live S3 verification when OPENALBERT_TEST_BACKUP_S3_ENDPOINT is set
// (along with BACKUP_S3_ACCESS_KEY, BACKUP_S3_SECRET_KEY, OPENALBERT_TEST_BACKUP_S3_BUCKET).
func TestS3RemoteBackupRestore(t *testing.T) {
	endpoint := os.Getenv("OPENALBERT_TEST_BACKUP_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OPENALBERT_TEST_BACKUP_S3_ENDPOINT to run live S3 backup integration test")
	}
	bucket := os.Getenv("OPENALBERT_TEST_BACKUP_S3_BUCKET")
	if bucket == "" {
		bucket = "backups"
	}
	c, key := fixture(t)
	c.Endpoint = endpoint
	c.Bucket = bucket
	c.Secure = os.Getenv("OPENALBERT_TEST_BACKUP_S3_SECURE") == "true"
	c.Prefix = fmt.Sprintf("test-%d/", time.Now().UnixNano())
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := NewRunner(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "completed" || s.LastSuccess == nil {
		t.Fatalf("live S3 backup failed: %+v %v", s, err)
	}
	remote, err := NewS3(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Delete(context.Background(), *s.LastSuccess) })
	downloaded := filepath.Join(t.TempDir(), "download.age")
	if err := remote.Download(ctx, *s.LastSuccess, downloaded); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, downloaded, identity, filepath.Join(t.TempDir(), "restored")); err != nil {
		t.Fatal(err)
	}
}
