package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Run through task test:backup. All containers, networks, buckets and files
// belong to a unique disposable Compose project, never the developer's stack.
func TestDockerBackupRestore(t *testing.T) {
	if os.Getenv("OPENALBERT_TEST_BACKUP_DOCKER") != "1" {
		t.Skip("run task test:backup for isolated Docker restore verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "installation")
	if err = os.MkdirAll(filepath.Join(root, "container_data/knowledge"), 0755); err != nil {
		t.Fatal(err)
	}
	project := "albert-backup-test-" + strings.ToLower(rand.Text()[:10])
	if configured := os.Getenv("OPENALBERT_TEST_BACKUP_PROJECT"); configured != "" {
		project = configured
	}
	pgImage := "postgres:17.9-alpine"
	minioImage := os.Getenv("OPENALBERT_TEST_BACKUP_MINIO_IMAGE")
	if minioImage == "" {
		minioImage = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	}
	sleepImage := "node:24-alpine"
	volume := func(source, target string) map[string]any {
		return map[string]any{"type": "bind", "source": filepath.Join(root, "container_data", source), "target": target}
	}
	services := map[string]any{
		"postgres":   map[string]any{"image": pgImage, "environment": map[string]string{"POSTGRES_USER": "openalbert", "POSTGRES_PASSWORD": "test-only-password", "POSTGRES_DB": "openalbert"}, "volumes": []any{volume("postgres", "/var/lib/postgresql/data")}, "healthcheck": map[string]any{"test": []string{"CMD-SHELL", "pg_isready -h 127.0.0.1 -U openalbert"}, "interval": "1s", "timeout": "2s", "retries": 60}},
		"minio":      map[string]any{"image": minioImage, "command": []string{"server", "/data"}, "environment": map[string]string{"MINIO_ROOT_USER": "test-access", "MINIO_ROOT_PASSWORD": "test-only-secret"}, "volumes": []any{volume("minio", "/data")}},
		"temporal":   map[string]any{"image": sleepImage, "init": true, "entrypoint": []string{"sleep", "infinity"}},
		"openalbert": map[string]any{"image": sleepImage, "init": true, "entrypoint": []string{"sleep", "infinity"}, "volumes": []any{volume("knowledge", "/data/knowledge"), volume("computer/client", "/data/computer")}},
		"computer":   map[string]any{"image": sleepImage, "init": true, "entrypoint": []string{"sleep", "infinity"}, "volumes": []any{volume("computer/server", "/etc/openalbert-ssh"), volume("computer/home", "/home/albert"), volume("computer/workspace", "/workspace")}},
	}
	controllerImage := os.Getenv("OPENALBERT_TEST_BACKUP_CONTROLLER_IMAGE")
	if controllerImage == "" {
		controllerImage = "openalbert-ci:backup"
	}
	bind := func(source, target string, ro bool) map[string]any {
		return map[string]any{"type": "bind", "source": source, "target": target, "read_only": ro}
	}
	services[ControllerService] = map[string]any{"image": controllerImage, "profiles": []string{"backup"}, "init": true, "read_only": true, "tmpfs": []string{"/tmp"}, "environment": map[string]string{"BACKUP_CONTAINER": "true"}, "stop_grace_period": "5m", "volumes": []any{
		bind(root, "/installation", true), bind(filepath.Join(root, "backup-config"), "/etc/openalbert-backup", true), bind(filepath.Join(root, "container_data/backup-state"), "/installation/container_data/backup-state", false), bind(filepath.Join(root, "container_data/backup-work"), "/var/lib/openalbert-backup", false), bind("/var/run/docker.sock", "/var/run/docker.sock", false),
	}}
	model := map[string]any{"name": project, "services": services}
	if err = AtomicJSON(filepath.Join(root, "compose.yaml"), model); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, ".env"), []byte("# isolated backup smoke\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Root: root, WorkDir: t.TempDir(), ComposeFile: "compose.yaml", Enabled: true, Time: "03:00", Timezone: "Europe/Moscow", KeepDays: 2, Endpoint: "placeholder:9000", Bucket: "backups", Prefix: "instance/", Recipient: key.Recipient().String()}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Config: c}
	if _, err = r.compose(ctx, "up", "-d", "--wait", "--wait-timeout", "90"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_, _ = r.compose(clean, "--profile", "backup", "down", "--volumes", "--remove-orphans")
	})
	sql := func(r *Runner, db, query string) string {
		t.Helper()
		b, e := r.compose(ctx, "exec", "-T", "postgres", "psql", "-U", "openalbert", "-d", db, "-Atc", query)
		if e != nil {
			t.Fatal(e)
		}
		return strings.TrimSpace(string(b))
	}
	for _, db := range []string{"temporal", "temporal_visibility"} {
		sql(r, "openalbert", "CREATE DATABASE "+db)
	}
	for _, db := range []string{"openalbert", "temporal", "temporal_visibility"} {
		sql(r, db, "CREATE TABLE backup_probe(value text); INSERT INTO backup_probe VALUES ('"+db+"-preserved')")
	}
	computerFiles := map[string]string{
		"client/id_ed25519":      "synthetic client key",
		"server/authorized_keys": "synthetic authorized key",
		"home/notes.txt":         "home preserved",
		"workspace/result.txt":   "workspace preserved",
	}
	for name, value := range computerFiles {
		if err = os.WriteFile(filepath.Join(root, "container_data/computer", name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "container_data/knowledge/SOUL.md"), []byte("Albert remembers"), 0600); err != nil {
		t.Fatal(err)
	}
	// Connect through the disposable network from this test container.
	containerName, _ := os.Hostname()
	if _, err = RunCommand(ctx, "docker", "network", "connect", project+"_default", containerName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = RunCommand(context.Background(), "docker", "network", "disconnect", project+"_default", containerName)
	})
	artifactClient, err := minio.New("minio:9000", &minio.Options{Creds: credentials.NewStaticV4("test-access", "test-only-secret", ""), Secure: false})
	if err != nil {
		t.Fatal(err)
	}
	waitS3 := func(client *minio.Client) {
		t.Helper()
		for n := 0; n < 40; n++ {
			if _, e := client.ListBuckets(ctx); e == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatal("MinIO did not become ready")
	}
	waitS3(artifactClient)
	if err = artifactClient.MakeBucket(ctx, "artifacts", minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	body := "artifact preserved across backup and restore"
	if _, err = artifactClient.PutObject(ctx, "artifacts", "document.txt", strings.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatal(err)
	}
	// A second MinIO container is the external backup destination; it is never
	// stopped by the application's Compose commands.
	remoteName := project + "-remote"
	if _, err = RunCommand(ctx, "docker", "run", "-d", "--name", remoteName, "--network", project+"_default", "-e", "MINIO_ROOT_USER=test-access", "-e", "MINIO_ROOT_PASSWORD=test-only-secret", minioImage, "server", "/data"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = RunCommand(context.Background(), "docker", "rm", "-f", remoteName) })
	remoteClient, err := minio.New(remoteName+":9000", &minio.Options{Creds: credentials.NewStaticV4("test-access", "test-only-secret", ""), Secure: false})
	if err != nil {
		t.Fatal(err)
	}
	waitS3(remoteClient)
	if err = remoteClient.MakeBucket(ctx, c.Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BACKUP_S3_ACCESS_KEY", "test-access")
	t.Setenv("BACKUP_S3_SECRET_KEY", "test-only-secret")
	c.Endpoint = remoteName + ":9000"
	r.Config = c
	// Run the actual controller image with the installation mounted read-only at
	// a different path. It must keep running while all data services are stopped.
	controllerConfig := c
	controllerConfig.Root = "/installation"
	controllerConfig.StateDir = ""
	controllerConfig.WorkDir = "/var/lib/openalbert-backup"
	controllerConfig.CredentialsFile = "/etc/openalbert-backup/s3.json"
	if err = controllerConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "backup-config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = AtomicJSON(filepath.Join(root, "backup-config/config.json"), controllerConfig); err != nil {
		t.Fatal(err)
	}
	if err = AtomicJSON(filepath.Join(root, "backup-config/s3.json"), map[string]string{"access_key": "test-access", "secret_key": "test-only-secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.compose(ctx, "--profile", "backup", "up", "-d", "--no-deps", "backup"); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 40; n++ {
		if state, e := (Reader{Dir: c.StateDir}).Read(); e == nil && state.Phase == "idle" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	controllerID, err := r.compose(ctx, "ps", "-q", "backup")
	if err != nil {
		t.Fatal(err)
	}
	startedAt, err := RunCommand(ctx, "docker", "inspect", strings.TrimSpace(string(controllerID)), "--format", "{{.State.StartedAt}}")
	if err != nil {
		t.Fatal(err)
	}
	createInContainer := func() {
		t.Helper()
		previous, _ := (Reader{Dir: c.StateDir}).Read()
		if _, e := r.compose(ctx, "exec", "-T", "backup", "/usr/local/bin/albert-backup", "request"); e != nil {
			logs, _ := r.compose(ctx, "logs", "--tail", "20", "backup")
			t.Fatalf("container backup request failed: %v; logs: %s", e, logs)
		}
		completed := false
		for n := 0; n < 240; n++ {
			state, e := (Reader{Dir: c.StateDir}).Read()
			if e == nil && state.RunID != previous.RunID {
				if state.Phase == "completed" {
					completed = true
					break
				}
				if state.Phase == "failed" {
					logs, _ := r.compose(ctx, "logs", "--tail", "20", "backup")
					t.Fatalf("backup failed: %+v; logs: %s", state, logs)
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !completed {
			logs, _ := r.compose(ctx, "logs", "--tail", "20", "backup")
			t.Fatalf("backup did not complete: %s", logs)
		}
		after, e := RunCommand(ctx, "docker", "inspect", strings.TrimSpace(string(controllerID)), "--format", "{{.State.StartedAt}}")
		if e != nil || string(after) != string(startedAt) {
			t.Fatalf("backup controller was restarted or stopped: %v", e)
		}
	}
	createInContainer()
	if _, err = r.compose(ctx, "exec", "-T", "--user", "10001", "backup", "cat", "/installation/container_data/backup-state/status.json"); err != nil {
		t.Fatal("application UID cannot read status", err)
	}
	s, err := (Reader{Dir: c.StateDir}).Read()
	if err != nil || s.Phase != "completed" || s.LastSuccess == nil {
		t.Fatalf("backup state: %+v %v", s, err)
	}
	remote, err := NewS3(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "download.age")
	if err = remote.Download(ctx, *s.LastSuccess, archive); err != nil {
		t.Fatal(err)
	}
	// Seed two earlier successful dates using real, verified encrypted bytes.
	for i, day := range []string{"2020-01-01", "2020-01-02"} {
		old := *s.LastSuccess
		old.ID = fmt.Sprintf("2020010%dT000000Z-0000000000000000", i+1)
		old.Day = day
		old.CreatedAt, _ = time.Parse(time.DateOnly, day)
		old.Key = c.Prefix + "copies/" + old.ID + "/archive.tar.gz.age"
		if err = remote.Upload(ctx, old, archive); err != nil {
			t.Fatal(err)
		}
	}
	createInContainer()
	copies, err := remote.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, copy := range copies {
		if copy.Day == "2020-01-01" {
			t.Fatal("expired successful date survived rotation")
		}
	}
	if len(copies) != 3 {
		t.Fatalf("same-day copies displaced a previous date: %+v", copies)
	}
	if _, err = remoteClient.StatObject(ctx, c.Bucket, c.Prefix+"copies/20200101T000000Z-0000000000000000/archive.tar.gz.age", minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
		t.Fatalf("expired bytes were not deleted: %v", err)
	}
	versioned := c
	versioned.Bucket = "versioned-backups"
	if err = remoteClient.MakeBucket(ctx, versioned.Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = remoteClient.EnableVersioning(ctx, versioned.Bucket); err != nil {
		t.Fatal(err)
	}
	if _, err = NewS3(ctx, versioned); err == nil {
		t.Fatal("accepted versioned bucket with ineffective physical deletion")
	}
	identity := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(identity, []byte(key.String()), 0600)
	destination := filepath.Join(t.TempDir(), "restored")
	if err = Restore(ctx, archive, identity, destination); err != nil {
		t.Fatal(err)
	}
	// Start the restored data under a different Compose project. No original
	// container or volume is reused, and no real provider is contacted.
	restoredModel := map[string]any{}
	if err = readJSON(filepath.Join(destination, "compose.yaml"), &restoredModel); err != nil {
		t.Fatal(err)
	}
	restoredModel["name"] = project + "-restored"
	raw, _ := json.Marshal(restoredModel)
	raw = []byte(strings.ReplaceAll(string(raw), root, destination))
	if err = os.WriteFile(filepath.Join(destination, "compose.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	restoredConfig := c
	restoredConfig.Root = destination
	restoredConfig.StateDir = ""
	restoredConfig.WorkDir = t.TempDir()
	if err = restoredConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	restored := &Runner{Config: restoredConfig}
	if _, err = restored.compose(ctx, "up", "-d", "--wait", "--wait-timeout", "90"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_, _ = restored.compose(clean, "down", "--volumes", "--remove-orphans")
	})
	for _, db := range []string{"openalbert", "temporal", "temporal_visibility"} {
		if got := sql(restored, db, "SELECT value FROM backup_probe"); got != db+"-preserved" {
			t.Fatalf("lost %s: %s", db, got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(destination, "container_data/knowledge/SOUL.md")); string(got) != "Albert remembers" {
		t.Fatal("lost knowledge")
	}
	for name, want := range computerFiles {
		got, err := os.ReadFile(filepath.Join(destination, "container_data/computer", name))
		if err != nil || string(got) != want {
			t.Fatalf("lost computer file %s: %q, %v", name, got, err)
		}
	}
	// Verify artifact bytes on the restored MinIO without changing DNS aliases.
	ids, err := restored.compose(ctx, "ps", "-q", "minio")
	if err != nil {
		t.Fatal(err)
	}
	ip, err := RunCommand(ctx, "docker", "inspect", strings.TrimSpace(string(ids)), "--format", `{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RunCommand(ctx, "docker", "network", "connect", project+"-restored_default", containerName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = RunCommand(context.Background(), "docker", "network", "disconnect", project+"-restored_default", containerName)
	})
	client, err := minio.New(strings.TrimSpace(string(ip))+":9000", &minio.Options{Creds: credentials.NewStaticV4("test-access", "test-only-secret", ""), Secure: false})
	if err != nil {
		t.Fatal(err)
	}
	waitS3(client)
	o, err := client.GetObject(ctx, "artifacts", "document.txt", minio.GetObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	expected := sha256.Sum256([]byte(body))
	_, err = io.Copy(h, o)
	o.Close()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(expected[:]) {
		t.Fatal("lost artifact bytes")
	}
	// Corrupt remote ciphertext: download must fail before any extraction.
	bad := "corrupted"
	_, err = remoteClient.PutObject(ctx, c.Bucket, s.LastSuccess.Key, strings.NewReader(bad), int64(len(bad)), minio.PutObjectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Download(ctx, *s.LastSuccess, filepath.Join(t.TempDir(), "bad.age")); err == nil {
		t.Fatal("accepted corrupt S3 bytes")
	}
}
