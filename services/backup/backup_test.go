package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
)

func fixture(t *testing.T) (Config, *age.X25519Identity) {
	t.Helper()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Root: filepath.Join(t.TempDir(), "installation"), WorkDir: t.TempDir(), ComposeFile: "compose.yaml", Enabled: true, Time: "03:00", Timezone: "Europe/Moscow", KeepDays: 2, Endpoint: "backup.example:9000", Bucket: "backups", Prefix: "instance/", Recipient: key.Recipient().String()}
	for name, value := range map[string]string{".env": "TOKEN=never-publish-this-secret", "compose.yaml": "services: {}", "container_data/postgres/PG_VERSION": "17", "container_data/minio/bucket/object": "artifact body", "container_data/knowledge/SOUL.md": "I am Albert", "container_data/stt/weights": "excluded", "container_data/victorialogs/log": "excluded", "container_data/backup-state/old": "excluded"} {
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

func TestArchiveEncryptedRestoreAndAuthentication(t *testing.T) {
	c, key := fixture(t)
	path := filepath.Join(t.TempDir(), "archive.age")
	m := Manifest{Version: 1, CreatedAt: time.Now(), ComposeFile: c.ComposeFile, Images: map[string]string{"openalbert": "registry/app@sha256:abc"}}
	if err := archive(context.Background(), c, path, m); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("never-publish")) || !bytes.HasPrefix(data, []byte("age-encryption.org/v1")) {
		t.Fatal("archive not encrypted")
	}
	identity := filepath.Join(t.TempDir(), "identity")
	_ = os.WriteFile(identity, []byte(key.String()), 0600)
	destination := filepath.Join(t.TempDir(), "restore")
	if err := Restore(context.Background(), path, identity, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "compose.yaml", "container_data/postgres/PG_VERSION", "container_data/minio/bucket/object", "container_data/knowledge/SOUL.md"} {
		want, _ := os.ReadFile(filepath.Join(c.Root, name))
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("restore %s: %v", name, err)
		}
	}
	for _, name := range []string{"stt", "victorialogs", "backup-state"} {
		if _, err := os.Stat(filepath.Join(destination, "container_data", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restored excluded %s", name)
		}
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

func TestCheckoutArchiveRestoresComputerAndComposePaths(t *testing.T) {
	c, key := fixture(t)
	c.ComposeFile = "deploy/compose.local.yaml"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		c.ComposeFile:                                    "services: {}",
		"services/postgres/init.sql":                     "CREATE DATABASE temporal;",
		"services/temporal/dynamicconfig.yaml":           "limit: 1",
		"container_data/computer/client/id_ed25519":      "private test key",
		"container_data/computer/server/authorized_keys": "public test key",
		"container_data/computer/home/file.txt":          "home data",
		"container_data/computer/workspace/result.txt":   "work data",
	}
	for name, value := range files {
		path := filepath.Join(c.Root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "archive.age")
	m := Manifest{Version: 1, CreatedAt: time.Now(), ComposeFile: c.ComposeFile, Images: map[string]string{"computer": "registry/computer@sha256:abc"}}
	if err := archive(context.Background(), c, path, m); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(identity, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	if err := Restore(context.Background(), path, identity, destination); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(got) != want {
			t.Fatalf("restore %s: %q, %v", name, got, err)
		}
	}
}

func TestRestoreRejectsTraversalAndLinks(t *testing.T) {
	_, key := fixture(t)
	identity := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(identity, []byte(key.String()), 0600)
	for _, h := range []*tar.Header{{Name: "../outside", Typeflag: tar.TypeReg, Size: 1, Mode: 0600}, {Name: "/absolute", Typeflag: tar.TypeReg, Size: 1, Mode: 0600}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}} {
		var b bytes.Buffer
		e, _ := age.Encrypt(&b, key.Recipient())
		gz := gzip.NewWriter(e)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(h)
		if h.Size > 0 {
			_, _ = tw.Write([]byte("x"))
		}
		_ = tw.Close()
		_ = gz.Close()
		_ = e.Close()
		path := filepath.Join(t.TempDir(), "bad.age")
		_ = os.WriteFile(path, b.Bytes(), 0600)
		if err := Restore(context.Background(), path, identity, filepath.Join(t.TempDir(), "restore")); err == nil {
			t.Fatalf("accepted unsafe header %+v", h)
		}
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

type memoryRemote struct {
	copies    []Copy
	archives  map[string][]byte
	uploadErr error
	deleted   []string
	onUpload  func()
}

func (m *memoryRemote) List(context.Context) ([]Copy, error) {
	return append([]Copy(nil), m.copies...), nil
}
func (m *memoryRemote) Upload(_ context.Context, c Copy, path string) error {
	if m.onUpload != nil {
		m.onUpload()
	}
	if m.uploadErr != nil {
		return m.uploadErr
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m.archives[c.ID] = b
	m.copies = append(m.copies, c)
	return nil
}
func (*memoryRemote) Download(context.Context, Copy, string) error { return errors.New("unused") }
func (m *memoryRemote) Delete(_ context.Context, c Copy) error {
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

func fakeCommands(t *testing.T, c Config, steps *[]string, failStop bool, failArchive func()) Command {
	t.Helper()
	running := true
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if args[0] == "image" {
			return []byte(`["registry/test@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`), nil
		}
		if args[0] == "inspect" {
			if args[len(args)-1] == "{{json .State}}" {
				return []byte(`{"Running":true,"Health":{"Status":"healthy"}}`), nil
			}
			if args[len(args)-1] == "{{.Image}}" {
				return []byte("sha256:test-image"), nil
			}
			service := strings.TrimPrefix(args[1], "id-")
			paths := map[string][]string{"postgres": {"postgres", "/var/lib/postgresql/data"}, "minio": {"minio", "/data"}, "openalbert": {"knowledge", "/data/knowledge"}}
			if p, ok := paths[service]; ok {
				return json.Marshal([]mount{{Type: "bind", Source: filepath.Join(c.Root, "container_data", p[0]), Destination: p[1]}})
			}
			return []byte(`[]`), nil
		}
		if strings.Contains(joined, " config --format json") {
			services := map[string]any{}
			services[ControllerService] = map[string]any{} // No digest is required for the controller itself.
			for name, v := range map[string][]string{"postgres": {"postgres", "/var/lib/postgresql/data"}, "minio": {"minio", "/data"}, "openalbert": {"knowledge", "/data/knowledge"}, "temporal": nil} {
				s := map[string]any{"image": "registry/test:stable"}
				if v != nil {
					s["volumes"] = []map[string]string{{"Type": "bind", "Source": filepath.Join(c.Root, "container_data", v[0]), "Target": v[1]}}
				}
				services[name] = s
			}
			return json.Marshal(map[string]any{"services": services})
		}
		if strings.Contains(joined, " ps --all --quiet") {
			var ids []string
			for i, arg := range args {
				if arg == "--quiet" {
					for _, name := range args[i+1:] {
						ids = append(ids, "id-"+name)
					}
					break
				}
			}
			return []byte(strings.Join(ids, "\n")), nil
		}
		if strings.Contains(joined, " ps --status running --services") {
			if running {
				return []byte("openalbert\ntemporal\npostgres\nminio\nbackup"), nil
			}
			return []byte("backup"), nil
		}
		if strings.Contains(joined, " stop --timeout") {
			for _, arg := range args {
				if arg == ControllerService {
					t.Fatal("backup controller stopped itself")
				}
			}
			*steps = append(*steps, "stop")
			running = false
			if failArchive != nil {
				failArchive()
			}
			if failStop {
				return nil, errors.New("stop failed")
			}
			return nil, nil
		}
		if strings.Contains(joined, " start ") {
			for _, arg := range args {
				if arg == ControllerService {
					t.Fatal("backup controller resumed itself")
				}
			}
			*steps = append(*steps, "start")
			running = true
			return nil, nil
		}
		t.Fatalf("unexpected Docker command %s", joined)
		return nil, nil
	}
}

func TestCreateResumesBeforeUploadAndRotatesOnlyAfterSuccess(t *testing.T) {
	for _, scenario := range []string{"success", "upload failure", "partial stop failure", "archive failure", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := fixture(t)
			remote := &memoryRemote{archives: map[string][]byte{}, copies: []Copy{{ID: "old1", Day: "2020-01-01", CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, {ID: "old2", Day: "2020-01-02", CreatedAt: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)}}}
			if scenario == "upload failure" {
				remote.uploadErr = errors.New("upload failed")
			}
			var steps []string
			remote.onUpload = func() {
				if strings.Join(steps, ",") != "stop,start" {
					t.Fatal("upload before service resumption")
				}
				steps = append(steps, "upload")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &Runner{Config: c, Remote: remote, Command: fakeCommands(t, c, &steps, scenario == "partial stop failure", func() {
				if scenario == "archive failure" {
					_ = os.Remove(filepath.Join(c.Root, ".env"))
				}
				if scenario == "cancelled" {
					cancel()
				}
			})}
			err := r.Create(ctx)
			if (err == nil) != (scenario == "success") {
				t.Fatalf("unexpected result %v", err)
			}
			if !strings.HasPrefix(strings.Join(steps, ","), "stop,start") {
				t.Fatalf("services not resumed: %v", steps)
			}
			if scenario == "success" {
				if len(remote.deleted) != 1 || remote.deleted[0] != "old1" {
					t.Fatalf("wrong rotation %v", remote.deleted)
				}
			} else if len(remote.deleted) != 0 {
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
			raw, _ := os.ReadFile(filepath.Join(c.StateDir, "status.json"))
			if bytes.Contains(raw, []byte("never-publish")) {
				t.Fatal("secret leaked into status")
			}
			if _, err := os.Stat(filepath.Join(c.StateDir, "resume.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("resume journal not cleared")
			}
		})
	}
}

func TestStateFreshnessMonitorAndInterruptedServiceRecovery(t *testing.T) {
	c, _ := fixture(t)
	now := time.Now().UTC()
	old := State{Version: 1, Enabled: true, Phase: "uploading", UpdatedAt: now.Add(-10 * time.Minute), HeartbeatAt: now.Add(-10 * time.Minute), NextRunAt: now.Add(time.Hour), RunID: "interrupted"}
	if err := AtomicJSON(filepath.Join(c.StateDir, "status.json"), old); err != nil {
		t.Fatal(err)
	}
	reader := Reader{Dir: c.StateDir}
	if s := reader.View(now); s.Phase != "unknown" || s.ErrorCode != "heartbeat_expired" {
		t.Fatalf("stale phase reported as live %+v", s)
	}
	var steps []string
	r := &Runner{Config: c, Command: fakeCommands(t, c, &steps, false, nil)}
	if err := r.Monitor(now); err != nil {
		t.Fatal(err)
	}
	if err := r.Monitor(now); err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 1 {
		t.Fatalf("health event not deduplicated: %v %v", events, err)
	}
	if err := AtomicJSON(filepath.Join(c.StateDir, "resume.json"), []string{"openalbert", "postgres"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "start" {
		t.Fatal("interrupted services not recovered")
	}
	s, _ := reader.Read()
	if s.Phase != "failed" || s.ErrorCode != "interrupted_backup" {
		t.Fatalf("interrupted copy reported as success: %+v", s)
	}
	lock, err := r.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := r.Recover(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent recovery accepted: %v", err)
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
	for _, change := range []func(*Config){func(c *Config) { c.KeepDays = 0 }, func(c *Config) { c.Time = "24:00" }, func(c *Config) { c.Timezone = "bad/zone" }, func(c *Config) { c.Prefix = "" }, func(c *Config) { c.WorkDir = c.Root }, func(c *Config) { c.Recipient = "private key" }} {
		bad := c
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("accepted invalid config")
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
	if bytes.Contains(b, []byte("SECRET")) || !bytes.Contains(b, []byte("EnvironmentFile=")) {
		t.Fatal("unit credential handling")
	}
}

func TestInputsRejectSymlinksBeforeStopping(t *testing.T) {
	c, _ := fixture(t)
	if err := os.Symlink("/tmp", filepath.Join(c.Root, "container_data/knowledge/outside")); err != nil {
		t.Fatal(err)
	}
	var steps []string
	r := &Runner{Config: c, Command: fakeCommands(t, c, &steps, false, nil), Remote: &memoryRemote{}}
	if err := r.Create(context.Background()); err == nil {
		t.Fatal("accepted external symlink")
	}
	if len(steps) != 0 {
		t.Fatal("caused downtime before discovering unsupported input")
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
	if err := os.Symlink(filepath.Join(c.Root, "container_data/postgres"), c.WorkDir); err != nil {
		t.Fatal(err)
	}
	var steps []string
	r := &Runner{Config: c, Remote: &memoryRemote{}, Command: fakeCommands(t, c, &steps, false, nil)}
	if err := r.Create(context.Background()); err == nil {
		t.Fatal("staging symlink resolved into backup source")
	}
	if len(steps) != 0 {
		t.Fatal("stopped services before detecting recursive backup layout")
	}
}
