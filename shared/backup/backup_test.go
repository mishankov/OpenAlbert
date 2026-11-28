package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
)

func createTestSQLiteDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)",
		"INSERT INTO notes (id, body) VALUES (1, 'Albert remembers'), (2, 'SQLite WAL mode')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed sqlite %q: %v", stmt, err)
		}
	}
}

func fixture(t *testing.T) (Config, *age.X25519Identity) {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	c := Config{
		Root:        filepath.Join(t.TempDir(), "installation"),
		WorkDir:     t.TempDir(),
		ComposeFile: "compose.yaml",
		Enabled:     true,
		Time:        "03:00",
		Timezone:    "Europe/Moscow",
		KeepDays:    2,
		Endpoint:    "backup.example:9000",
		Bucket:      "backups",
		Prefix:      "instance/",
		Recipient:   key.Recipient().String(),
	}
	createTestSQLiteDB(t, filepath.Join(c.Root, "data", "openalbert.db"))
	for name, value := range map[string]string{
		".env":                            "TOKEN=never-publish-this-secret",
		"compose.yaml":                    "services: {}",
		"models.toml":                     "# synthetic model configuration",
		"data/knowledge/SOUL.md":          "I am Albert",
		"data/knowledge/.openalbert-123":  "in-flight knowledge temp",
		"data/artifacts/blob.bin":         "artifact body",
		"data/artifacts/inprogress.1.tmp": "in-flight artifact temp",
		"data/chatgpt/oauth.json":         `{"token":"synthetic-oauth-token"}`,
		"data/extra/excluded.txt":         "excluded",
		"backup-state/old.json":           "excluded",
		"unrelated-host-file.txt":         "excluded",
	} {
		path := filepath.Join(c.Root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c, key
}

func assertWorkDirEmpty(t *testing.T, workDir string) {
	t.Helper()
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("work_dir not cleaned up: %v", names)
	}
}

type memoryRemote struct {
	mu        sync.Mutex
	copies    []Copy
	archives  map[string][]byte
	uploadErr error
	deleted   []string
	onUpload  func()
}

func (m *memoryRemote) List(context.Context) ([]Copy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Copy(nil), m.copies...), nil
}

func (m *memoryRemote) Upload(ctx context.Context, c Copy, path string) error {
	m.mu.Lock()
	onUpload := m.onUpload
	uploadErr := m.uploadErr
	m.mu.Unlock()
	if onUpload != nil {
		onUpload()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if uploadErr != nil {
		return uploadErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.archives == nil {
		m.archives = map[string][]byte{}
	}
	m.archives[c.ID] = b
	m.copies = append(m.copies, c)
	return nil
}

func (*memoryRemote) Download(context.Context, Copy, string) error { return errors.New("unused") }

func (m *memoryRemote) Delete(_ context.Context, c Copy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, c.ID)
	var kept []Copy
	for _, old := range m.copies {
		if old.ID != c.ID {
			kept = append(kept, old)
		}
	}
	m.copies = kept
	return nil
}

func (m *memoryRemote) archiveBytes(id string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.archives[id]...)
}

func (m *memoryRemote) deletedIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.deleted...)
}

func (m *memoryRemote) seed(copies ...Copy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.copies = append(m.copies, copies...)
}

func TestArchiveEncryptedRestoreAndAuthentication(t *testing.T) {
	c, key := fixture(t)
	modelConfigFile := filepath.Join("config", "provider$BACKUP_MODEL.toml")
	if err := os.MkdirAll(filepath.Join(c.Root, "config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(c.Root, "models.toml"), filepath.Join(c.Root, modelConfigFile)); err != nil {
		t.Fatal(err)
	}
	credentials := []byte("ANALYSIS_LLM_API_KEY=synthetic-analysis-secret\n")
	if err := os.WriteFile(filepath.Join(c.Root, "models.env"), credentials, 0600); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := snapshotSQLite(context.Background(), filepath.Join(c.Root, "data", "openalbert.db"), snapshotPath); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "archive.age")
	m := Manifest{
		Version:              ManifestVersion,
		CreatedAt:            time.Now().UTC(),
		ComposeFile:          c.ComposeFile,
		ModelConfigFile:      filepath.ToSlash(modelConfigFile),
		ModelCredentialsFile: "models.env",
	}
	if err := archive(context.Background(), c, snapshotPath, path, m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("never-publish")) || bytes.Contains(data, []byte("synthetic-analysis-secret")) || bytes.Contains(data, []byte("synthetic-oauth-token")) || !bytes.HasPrefix(data, []byte("age-encryption.org/v1")) {
		t.Fatal("archive not encrypted")
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	if err := Restore(context.Background(), path, identity, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", modelConfigFile, "models.env", "compose.yaml", "data/knowledge/SOUL.md", "data/artifacts/blob.bin"} {
		want, err := os.ReadFile(filepath.Join(c.Root, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("restore %s: %v", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(destination, "models.env")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("model credential permissions were not retained: %v", err)
	}
	for _, excluded := range []string{"data/chatgpt", "data/extra", "data/knowledge/.openalbert-123", "data/artifacts/inprogress.1.tmp", "backup-state", "unrelated-host-file.txt", "data/openalbert.db-wal", "data/openalbert.db-shm"} {
		if _, err := os.Lstat(filepath.Join(destination, excluded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restored excluded path %s: %v", excluded, err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(destination, "data", "openalbert.db"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM notes").Scan(&count); err != nil || count != 2 {
		t.Fatalf("restored sqlite rows: count=%d err=%v", count, err)
	}
	if err := Restore(context.Background(), path, identity, destination); err == nil {
		t.Fatal("overwrote existing restore")
	}
	// Authentication must cover the complete stream, including after tar EOF.
	for _, broken := range [][]byte{data[:len(data)-1], append(append([]byte(nil), data[:len(data)-1]...), data[len(data)-1]^1)} {
		bad := filepath.Join(t.TempDir(), "bad.age")
		_ = os.WriteFile(bad, broken, 0600)
		dest := filepath.Join(t.TempDir(), "restore")
		if err := Restore(context.Background(), bad, identity, dest); err == nil {
			t.Fatal("accepted damaged ciphertext")
		}
		if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed restore left partially usable data")
		}
	}
}

func TestArchiveSurvivesConcurrentAtomicKnowledgeRenames(t *testing.T) {
	c, key := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Continuously atomically replace data/knowledge/SOUL.md using the writer's
	// .openalbert-* temp-file + rename pattern while archiving.
	knowledgeDir := filepath.Join(c.Root, "data", "knowledge")
	target := filepath.Join(knowledgeDir, "SOUL.md")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ctx.Err() == nil; i++ {
			tmp, err := os.CreateTemp(knowledgeDir, ".openalbert-*")
			if err != nil {
				return
			}
			_, _ = tmp.WriteString(fmt.Sprintf("version-%d\n", i))
			_ = tmp.Close()
			_ = os.Rename(tmp.Name(), target)
		}
	}()

	for range 5 {
		snapDir := t.TempDir()
		snapshotPath := filepath.Join(snapDir, "openalbert.db")
		if err := snapshotSQLite(ctx, filepath.Join(c.Root, "data", "openalbert.db"), snapshotPath); err != nil {
			t.Fatal(err)
		}
		outPath := filepath.Join(t.TempDir(), "archive.tar.gz.age")
		m := Manifest{Version: ManifestVersion, CreatedAt: time.Now().UTC(), ComposeFile: "compose.yaml"}
		if err := archive(ctx, c, snapshotPath, outPath, m); err != nil {
			cancel()
			<-done
			t.Fatalf("archive failed during concurrent atomic knowledge rename: %v", err)
		}
		idFile := filepath.Join(t.TempDir(), "id")
		if err := os.WriteFile(idFile, []byte(key.String()), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Restore(ctx, outPath, idFile, filepath.Join(t.TempDir(), "out")); err != nil {
			cancel()
			<-done
			t.Fatalf("restore failed: %v", err)
		}
	}
	cancel()
	<-done
}

func TestRestorePreservesPermissionsNonRootDifferentUID(t *testing.T) {
	c, key := fixture(t)
	snapshotPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := snapshotSQLite(context.Background(), filepath.Join(c.Root, "data", "openalbert.db"), snapshotPath); err != nil {
		t.Fatal(err)
	}
	dbBytes, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := json.Marshal(Manifest{Version: ManifestVersion, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	enc, err := age.Encrypt(&buf, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	now := time.Now().UTC().Truncate(time.Second)
	entries := []struct {
		header *tar.Header
		body   []byte
	}{
		{&tar.Header{Name: "backup-manifest.json", Typeflag: tar.TypeReg, Mode: 0600, Uid: 65534, Gid: 65534, Size: int64(len(manifestBytes)), ModTime: now}, manifestBytes},
		{&tar.Header{Name: "data", Typeflag: tar.TypeDir, Mode: 0755, Uid: 65534, Gid: 65534, ModTime: now}, nil},
		{&tar.Header{Name: "data/openalbert.db", Typeflag: tar.TypeReg, Mode: 0600, Uid: 65534, Gid: 65534, Size: int64(len(dbBytes)), ModTime: now}, dbBytes},
		{&tar.Header{Name: "data/knowledge", Typeflag: tar.TypeDir, Mode: 0755, Uid: 65534, Gid: 65534, ModTime: now}, nil},
		{&tar.Header{Name: "data/knowledge/SOUL.md", Typeflag: tar.TypeReg, Mode: 0640, Uid: 65534, Gid: 65534, Size: int64(len("hello")), ModTime: now}, []byte("hello")},
	}
	for _, entry := range entries {
		if err := tw.WriteHeader(entry.header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(tw.Close(), gz.Close(), enc.Close()); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "cross-uid.age")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restore")
	if err := Restore(context.Background(), archivePath, identity, dest); err != nil {
		t.Fatalf("non-root restore with different UID/GID failed: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "data/knowledge/SOUL.md"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("permissions not preserved: %v", err)
	}
}

func TestBareBinaryMinimalLayoutCreateAndRestore(t *testing.T) {
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "bare-install")
	createTestSQLiteDB(t, filepath.Join(root, "data", "openalbert.db"))
	if err := os.MkdirAll(filepath.Join(root, "data", "knowledge"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "knowledge", "SOUL.md"), []byte("minimal"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{
		Root:      root,
		WorkDir:   t.TempDir(),
		Enabled:   true,
		Time:      "03:00",
		Timezone:  "UTC",
		KeepDays:  7,
		Endpoint:  "backup.example:9000",
		Bucket:    "backups",
		Prefix:    "bare/",
		Recipient: key.Recipient().String(),
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	remote := &memoryRemote{}
	r, err := NewRunner(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	r.Remote = remote
	if err := r.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertWorkDirEmpty(t, c.WorkDir)
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "completed" || s.LastSuccess == nil {
		t.Fatalf("unexpected state: %+v %v", s, err)
	}
	archivePath := filepath.Join(t.TempDir(), "archive.age")
	if err := os.WriteFile(archivePath, remote.archiveBytes(s.LastSuccess.ID), 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restore")
	if err := Restore(context.Background(), archivePath, identity, dest); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := readJSON(filepath.Join(dest, "backup-manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != ManifestVersion || m.ComposeFile != "" || m.ModelConfigFile != "" || m.ModelCredentialsFile != "" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "data", "knowledge", "SOUL.md")); err != nil || string(got) != "minimal" {
		t.Fatalf("knowledge not restored: %q %v", got, err)
	}
}

func TestPreflightOptionalFilesAndSymlinkRejection(t *testing.T) {
	t.Run("models.env", func(t *testing.T) {
		for _, scenario := range []string{"absent", "present", "symlink", "directory"} {
			t.Run(scenario, func(t *testing.T) {
				c, _ := fixture(t)
				path := filepath.Join(c.Root, "models.env")
				switch scenario {
				case "present":
					if err := os.WriteFile(path, []byte("KEY=val\n"), 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(c.Root, ".env"), path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				m, err := (&Runner{Config: c}).preflight(context.Background())
				if scenario == "absent" || scenario == "present" {
					want := ""
					if scenario == "present" {
						want = "models.env"
					}
					if err != nil || m.ModelCredentialsFile != want {
						t.Fatalf("optional model credentials: %q %v", m.ModelCredentialsFile, err)
					}
				} else if err == nil {
					t.Fatal("nonregular model credentials accepted")
				}
			})
		}
	})

	t.Run("model_config_file", func(t *testing.T) {
		for _, scenario := range []string{"default", "nested", "missing", "symlink-file", "symlink-parent"} {
			t.Run(scenario, func(t *testing.T) {
				c, _ := fixture(t)
				switch scenario {
				case "nested":
					c.ModelConfigFile = filepath.Join("config", "provider.toml")
					target := filepath.Join(c.Root, c.ModelConfigFile)
					if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, []byte("# nested"), 0644); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Remove(filepath.Join(c.Root, "models.toml")); err != nil {
						t.Fatal(err)
					}
				case "symlink-file":
					c.ModelConfigFile = filepath.Join("config", "provider.toml")
					target := filepath.Join(c.Root, c.ModelConfigFile)
					if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(c.Root, "models.toml"), target); err != nil {
						t.Fatal(err)
					}
				case "symlink-parent":
					c.ModelConfigFile = filepath.Join("config", "provider.toml")
					if err := os.Symlink(t.TempDir(), filepath.Join(c.Root, "config")); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.Validate(); err != nil {
					t.Fatal(err)
				}
				m, err := (&Runner{Config: c}).preflight(context.Background())
				switch scenario {
				case "default":
					if err != nil || m.ModelConfigFile != "models.toml" {
						t.Fatalf("default model config: %+v %v", m, err)
					}
				case "nested":
					if err != nil || m.ModelConfigFile != "config/provider.toml" {
						t.Fatalf("nested model config: %+v %v", m, err)
					}
				case "missing":
					if err != nil || m.ModelConfigFile != "" {
						t.Fatalf("missing optional model config: %+v %v", m, err)
					}
				default:
					if err == nil {
						t.Fatalf("accepted symlinked model config (%s)", scenario)
					}
				}
			})
		}
	})
}

func TestRestoreRejectsTraversalLinksWALAndCorruptDB(t *testing.T) {
	_, key := fixture(t)
	identity := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	writeArchive := func(t *testing.T, items []struct {
		h    *tar.Header
		body []byte
	}) string {
		t.Helper()
		var b bytes.Buffer
		e, err := age.Encrypt(&b, key.Recipient())
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(e)
		tw := tar.NewWriter(gz)
		for _, item := range items {
			if err := tw.WriteHeader(item.h); err != nil {
				t.Fatal(err)
			}
			if len(item.body) > 0 {
				if _, err := tw.Write(item.body); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := errors.Join(tw.Close(), gz.Close(), e.Close()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "bad.age")
		if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for _, h := range []*tar.Header{
		{Name: "../outside", Typeflag: tar.TypeReg, Size: 1, Mode: 0600},
		{Name: "/absolute", Typeflag: tar.TypeReg, Size: 1, Mode: 0600},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"},
		{Name: "data/openalbert.db-wal", Typeflag: tar.TypeReg, Size: 1, Mode: 0600},
	} {
		var body []byte
		if h.Size > 0 {
			body = []byte("x")
		}
		path := writeArchive(t, []struct {
			h    *tar.Header
			body []byte
		}{{h, body}})
		if err := Restore(context.Background(), path, identity, filepath.Join(t.TempDir(), "restore")); err == nil {
			t.Fatalf("accepted unsafe header %+v", h)
		}
	}

	// Corrupt SQLite database must fail integrity check during Restore.
	mBytes, _ := json.Marshal(Manifest{Version: ManifestVersion, CreatedAt: time.Now().UTC()})
	corruptDB := []byte("not a sqlite database at all")
	corruptArchive := writeArchive(t, []struct {
		h    *tar.Header
		body []byte
	}{
		{&tar.Header{Name: "backup-manifest.json", Typeflag: tar.TypeReg, Size: int64(len(mBytes)), Mode: 0600}, mBytes},
		{&tar.Header{Name: "data/openalbert.db", Typeflag: tar.TypeReg, Size: int64(len(corruptDB)), Mode: 0600}, corruptDB},
		{&tar.Header{Name: "data/knowledge", Typeflag: tar.TypeDir, Mode: 0755}, nil},
	})
	dest := filepath.Join(t.TempDir(), "restore-corrupt")
	if err := Restore(context.Background(), corruptArchive, identity, dest); err == nil {
		t.Fatal("accepted corrupt sqlite database")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed corrupt restore left destination directory")
	}

	// Manifest version 1 must be rejected.
	v1Bytes, _ := json.Marshal(Manifest{Version: 1, CreatedAt: time.Now().UTC()})
	v1Archive := writeArchive(t, []struct {
		h    *tar.Header
		body []byte
	}{
		{&tar.Header{Name: "backup-manifest.json", Typeflag: tar.TypeReg, Size: int64(len(v1Bytes)), Mode: 0600}, v1Bytes},
	})
	if err := Restore(context.Background(), v1Archive, identity, filepath.Join(t.TempDir(), "restore-v1")); err == nil {
		t.Fatal("accepted v1 manifest")
	}
}

func TestCreateOnlineSnapshotUnderConcurrentWrites(t *testing.T) {
	c, key := fixture(t)
	dbPath := filepath.Join(c.Root, "data", "openalbert.db")
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"CREATE TABLE events (seq INTEGER PRIMARY KEY, payload TEXT NOT NULL)",
		"CREATE TABLE audit (seq INTEGER PRIMARY KEY, note TEXT NOT NULL)",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	const seedCount = 20
	for i := 1; i <= seedCount; i++ {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO events (seq, payload) VALUES (?, ?)", i, fmt.Sprintf("seed-%d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO audit (seq, note) VALUES (?, ?)", i, fmt.Sprintf("audit-%d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}

	stopWriter := make(chan struct{})
	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	var committed int
	go func() {
		seq := seedCount
		for {
			select {
			case <-stopWriter:
				writerDone <- nil
				return
			default:
			}
			seq++
			tx, err := db.Begin()
			if err != nil {
				writerDone <- err
				return
			}
			if _, err := tx.Exec("INSERT INTO events (seq, payload) VALUES (?, ?)", seq, fmt.Sprintf("live-%d", seq)); err != nil {
				_ = tx.Rollback()
				writerDone <- err
				return
			}
			if _, err := tx.Exec("INSERT INTO audit (seq, note) VALUES (?, ?)", seq, fmt.Sprintf("audit-%d", seq)); err != nil {
				_ = tx.Rollback()
				writerDone <- err
				return
			}
			if err := tx.Commit(); err != nil {
				writerDone <- err
				return
			}
			committed = seq
			if seq == seedCount+1 {
				close(writerStarted)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	<-writerStarted
	remote := &memoryRemote{}
	r := &Runner{Config: c, Remote: remote}
	if err := r.Create(context.Background()); err != nil {
		close(stopWriter)
		<-writerDone
		t.Fatalf("online Create failed during concurrent writes: %v", err)
	}
	close(stopWriter)
	if err := <-writerDone; err != nil {
		t.Fatalf("concurrent writer failed: %v", err)
	}
	if committed <= seedCount {
		t.Fatalf("expected concurrent writes to advance beyond %d, got %d", seedCount, committed)
	}
	assertWorkDirEmpty(t, c.WorkDir)

	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "completed" || s.LastSuccess == nil {
		t.Fatalf("unexpected backup state: %+v %v", s, err)
	}
	archivePath := filepath.Join(t.TempDir(), "concurrent.age")
	if err := os.WriteFile(archivePath, remote.archiveBytes(s.LastSuccess.ID), 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := Restore(context.Background(), archivePath, identity, dest); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	for _, wal := range []string{"data/openalbert.db-wal", "data/openalbert.db-shm"} {
		if _, err := os.Lstat(filepath.Join(dest, wal)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected WAL/SHM file in restore: %s", wal)
		}
	}

	restoredDB, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dest, "data", "openalbert.db"), true))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	var check string
	if err := restoredDB.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("restored DB integrity_check=%q err=%v", check, err)
	}
	var eventCount, auditCount, mismatched int
	if err := restoredDB.QueryRow("SELECT COUNT(*) FROM events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := restoredDB.QueryRow("SELECT COUNT(*) FROM audit").Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := restoredDB.QueryRow("SELECT COUNT(*) FROM events e LEFT JOIN audit a ON e.seq = a.seq WHERE a.seq IS NULL").Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if eventCount < seedCount || eventCount != auditCount || mismatched != 0 {
		t.Fatalf("inconsistent snapshot: events=%d audit=%d mismatched=%d", eventCount, auditCount, mismatched)
	}
}

func TestRetentionCountsSuccessfulDatesAndPreservesSameDayCopies(t *testing.T) {
	var copies []Copy
	for i, date := range []string{"2026-10-26", "2026-10-26", "2026-10-23", "2026-10-20", "2026-10-20"} {
		at, _ := time.Parse(time.DateOnly, date)
		copies = append(copies, Copy{ID: fmt.Sprint(i), Day: date, CreatedAt: at})
	}
	expired := Expired(copies, 2)
	if len(expired) != 2 || expired[0].Day != "2026-10-20" || expired[1].Day != "2026-10-20" {
		t.Fatalf("wrong retention: %+v", expired)
	}
}

func TestCreateRotatesOnlyAfterSuccessAndCleansWorkDir(t *testing.T) {
	for _, scenario := range []string{"success", "upload failure", "cancelled", "corrupt db"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := fixture(t)
			remote := &memoryRemote{
				copies: []Copy{
					{ID: "old1", Day: "2020-01-01", CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
					{ID: "old2", Day: "2020-01-02", CreatedAt: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)},
				},
			}
			if scenario == "upload failure" {
				remote.uploadErr = errors.New("upload failed")
			}
			if scenario == "corrupt db" {
				if err := os.WriteFile(filepath.Join(c.Root, "data", "openalbert.db"), []byte("corrupt-bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				remote.onUpload = cancel
			}
			r := &Runner{Config: c, Remote: remote}
			err := r.Create(ctx)
			if (err == nil) != (scenario == "success") {
				t.Fatalf("unexpected result: %v", err)
			}
			assertWorkDirEmpty(t, c.WorkDir)
			if scenario == "success" {
				deleted := remote.deletedIDs()
				if len(deleted) != 1 || deleted[0] != "old1" {
					t.Fatalf("wrong rotation %v", deleted)
				}
			} else if len(remote.deletedIDs()) != 0 {
				t.Fatal("rotated after failed backup")
			}
			s, err := (Reader{Dir: c.StateDir}).Read()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "success" && (s.Phase != "completed" || s.LastSuccess == nil || len(s.Copies) != 2) {
				t.Fatalf("missing success state %+v", s)
			}
			if scenario != "success" && s.Phase != "failed" {
				t.Fatalf("missing failure %+v", s)
			}
			if scenario == "corrupt db" && s.ErrorCode != "snapshot_failed" {
				t.Fatalf("expected snapshot_failed, got %q", s.ErrorCode)
			}
			raw, _ := os.ReadFile(filepath.Join(c.StateDir, "status.json"))
			if bytes.Contains(raw, []byte("never-publish")) {
				t.Fatal("secret leaked into status")
			}
		})
	}
}

func TestStateFreshnessMonitorAndDeduplication(t *testing.T) {
	c, _ := fixture(t)
	now := time.Now().UTC()
	old := State{
		Version:     1,
		Enabled:     true,
		Phase:       "uploading",
		UpdatedAt:   now.Add(-10 * time.Minute),
		HeartbeatAt: now.Add(-10 * time.Minute),
		NextRunAt:   now.Add(time.Hour),
		RunID:       "interrupted",
	}
	if err := AtomicJSON(filepath.Join(c.StateDir, "status.json"), old); err != nil {
		t.Fatal(err)
	}
	reader := Reader{Dir: c.StateDir}
	if s := reader.View(now); s.Phase != "unknown" || s.ErrorCode != "heartbeat_expired" {
		t.Fatalf("stale phase reported as live %+v", s)
	}
	r := &Runner{Config: c}
	if err := r.Monitor(now); err != nil {
		t.Fatal(err)
	}
	if err := r.Monitor(now); err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 1 || events[0].Kind != "overdue" {
		t.Fatalf("health event not deduplicated: %v %v", events, err)
	}
	// Monitor must skip quietly while another operation holds the lock.
	lock, err := r.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := r.Monitor(now.Add(48 * time.Hour)); err != nil {
		t.Fatalf("monitor failed while busy: %v", err)
	}
}

func TestScheduleAndConfigValidation(t *testing.T) {
	c, _ := fixture(t)
	now := time.Date(2026, 10, 26, 23, 30, 0, 0, time.UTC)
	if got := c.NextRun(now).Format(time.RFC3339); got != "2026-10-27T03:00:00+03:00" {
		t.Fatalf("wrong next run %s", got)
	}
	dst := c
	dst.Timezone = "America/New_York"
	dst.Time = "02:30"
	beforeGap := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	if got := dst.NextRun(beforeGap).Format(time.RFC3339); got != "2026-03-09T02:30:00-04:00" {
		t.Fatalf("scheduled nonexistent DST time: %s", got)
	}
	// Explicit external absolute state_dir and empty compose_file are allowed.
	custom := c
	custom.ComposeFile = ""
	custom.StateDir = t.TempDir()
	custom.ModelConfigFile = "config/models.toml"
	if err := custom.Validate(); err != nil {
		t.Fatalf("valid custom config rejected: %v", err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.KeepDays = 0 },
		func(c *Config) { c.Time = "24:00" },
		func(c *Config) { c.Timezone = "bad/zone" },
		func(c *Config) { c.Prefix = "" },
		func(c *Config) { c.WorkDir = c.Root },
		func(c *Config) { c.StateDir = c.Root },
		func(c *Config) { c.StateDir = filepath.Join(c.Root, "data", "state") },
		func(c *Config) { c.StateDir = c.WorkDir },
		func(c *Config) { c.StateDir = "relative-state" },
		func(c *Config) { c.ComposeFile = "other.yaml" },
		func(c *Config) { c.ModelConfigFile = "../outside.toml" },
		func(c *Config) { c.ModelConfigFile = "data/models.toml" },
		func(c *Config) { c.ModelConfigFile = "models.yaml" },
		func(c *Config) { c.Recipient = "private key" },
	} {
		bad := c
		change(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid config: %+v", bad)
		}
	}
	output := t.TempDir()
	if err := c.Units("/usr/local/bin/albert-backup", "/etc/backup.json", "/etc/backup.env", output); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(output, "openalbert-backup.timer"))
	if err != nil || !bytes.Contains(b, []byte("03:00:00 Europe/Moscow")) || !bytes.Contains(b, []byte("Persistent=true")) {
		t.Fatalf("invalid timer %s %v", b, err)
	}
	b, _ = os.ReadFile(filepath.Join(output, "openalbert-backup.service"))
	if bytes.Contains(b, []byte("SECRET")) || !bytes.Contains(b, []byte("EnvironmentFile=")) || bytes.Contains(b, []byte("docker.service")) {
		t.Fatalf("unexpected service unit: %s", b)
	}
}

func TestInputsRejectSymlinks(t *testing.T) {
	for _, target := range []string{"data/knowledge/outside", "data/artifacts/outside", "data/openalbert.db", ".env"} {
		t.Run(target, func(t *testing.T) {
			c, _ := fixture(t)
			full := filepath.Join(c.Root, target)
			_ = os.Remove(full)
			if err := os.Symlink("/tmp", full); err != nil {
				t.Fatal(err)
			}
			uploaded := false
			remote := &memoryRemote{onUpload: func() { uploaded = true }}
			r := &Runner{Config: c, Remote: remote}
			if err := r.Create(context.Background()); err == nil {
				t.Fatalf("accepted symlink at %s", target)
			}
			if uploaded {
				t.Fatal("uploaded despite symlinked input")
			}
		})
	}
}

func TestStatusRemainsReadableWithServiceUmask(t *testing.T) {
	c, _ := fixture(t)
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	r := &Runner{Config: c}
	if err := r.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := r.emit("starting", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{c.StateDir, filepath.Join(c.StateDir, "events")} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatalf("public directory unreadable under service umask: %s %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(c.StateDir, "status.json"))
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("status unreadable by application UID")
	}
}

func TestStagingSymlinkCannotPutArchiveInsideSource(t *testing.T) {
	c, _ := fixture(t)
	if err := os.Remove(c.WorkDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(c.Root, "data"), c.WorkDir); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: c, Remote: &memoryRemote{}}
	if err := r.Create(context.Background()); err == nil {
		t.Fatal("staging symlink resolved into backup source")
	}
}
