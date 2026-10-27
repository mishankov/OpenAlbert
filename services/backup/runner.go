package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var validID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{16}$`)
var ErrBusy = errors.New("another backup operation is running")

type Command func(context.Context, string, ...string) ([]byte, error)

func RunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	// Docker errors can contain interpolated .env values. Never publish raw output.
	if err != nil {
		return nil, fmt.Errorf("%s command failed", filepath.Base(name))
	}
	return out, nil
}

type Runner struct {
	Config      Config
	Remote      Remote
	Command     Command
	HostRoot    string
	ProjectName string
	mu          sync.Mutex
	state       State
}

func (r *Runner) compose(ctx context.Context, args ...string) ([]byte, error) {
	command := r.Command
	if command == nil {
		command = RunCommand
	}
	base := []string{"compose", "--project-directory", r.Config.Root, "--env-file", filepath.Join(r.Config.Root, ".env"), "-f", filepath.Join(r.Config.Root, r.Config.ComposeFile)}
	if r.ProjectName != "" {
		base = append(base, "--project-name", r.ProjectName)
	}
	return command(ctx, "docker", append(base, args...)...)
}

func (r *Runner) lock() (*os.File, error) {
	if err := r.Config.Validate(); err != nil {
		return nil, err
	}
	if err := publicDirectory(r.Config.StateDir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(r.Config.StateDir, "backup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrBusy
	}
	return f, nil
}

func (r *Runner) loadState() {
	r.state, _ = (Reader{Dir: r.Config.StateDir}).Read()
	c := r.Config
	r.state.Version, r.state.Enabled = 1, c.Enabled
	r.state.Time, r.state.Timezone, r.state.KeepDays = c.Time, c.Timezone, c.KeepDays
	r.state.NextRunAt = c.NextRun(time.Now())
}

func (r *Runner) save(phase, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Phase, r.state.ErrorCode = phase, code
	r.state.UpdatedAt, r.state.HeartbeatAt = time.Now().UTC(), time.Now().UTC()
	return AtomicJSON(filepath.Join(r.Config.StateDir, "status.json"), r.state)
}

func (r *Runner) emit(kind, code string) error {
	if err := publicDirectory(filepath.Join(r.Config.StateDir, "events")); err != nil {
		return err
	}
	e := Event{ID: r.state.RunID + "-" + kind, RunID: r.state.RunID, Kind: kind, At: time.Now().UTC(), ErrorCode: code}
	return AtomicJSON(filepath.Join(r.Config.StateDir, "events", e.ID+".json"), e)
}

func (r *Runner) Sync() error {
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	r.loadState()
	return r.save("idle", "")
}

func (r *Runner) heartbeat(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.mu.Lock()
			r.state.HeartbeatAt = time.Now().UTC()
			_ = AtomicJSON(filepath.Join(r.Config.StateDir, "status.json"), r.state)
			r.mu.Unlock()
		}
	}
}

func (r *Runner) preflight(ctx context.Context) ([]string, Manifest, error) {
	c := r.Config
	var m Manifest
	for _, input := range []string{c.Root, filepath.Join(c.Root, ".env"), filepath.Join(c.Root, c.ComposeFile)} {
		info, err := os.Lstat(input)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, m, errors.New("backup inputs are missing or symlinked")
		}
	}
	if err := os.MkdirAll(c.WorkDir, 0700); err != nil {
		return nil, m, err
	}
	actualRoot, e := filepath.EvalSymlinks(c.Root)
	if e != nil {
		return nil, m, e
	}
	actualWork, e := filepath.EvalSymlinks(c.WorkDir)
	if e != nil {
		return nil, m, e
	}
	if within(actualRoot, actualWork) || within(actualWork, actualRoot) {
		return nil, m, errors.New("staging directory resolves inside the installation")
	}
	if err := os.Chmod(c.WorkDir, 0700); err != nil {
		return nil, m, err
	}
	raw, err := r.compose(ctx, "config", "--format", "json")
	if err != nil {
		return nil, m, err
	}
	var config struct {
		Services map[string]struct {
			Image   string                                  `json:"image"`
			Volumes []struct{ Type, Source, Target string } `json:"volumes"`
		} `json:"services"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, m, errors.New("cannot read Compose configuration")
	}
	type dataMount struct{ path, target string }
	expected := map[string][]dataMount{"postgres": {{"postgres", "/var/lib/postgresql/data"}}, "minio": {{"minio", "/data"}}, "openalbert": {{"knowledge", "/data/knowledge"}}}
	if _, ok := config.Services["computer"]; ok {
		expected["computer"] = []dataMount{{"computer/server", "/etc/openalbert-ssh"}, {"computer/home", "/home/albert"}, {"computer/workspace", "/workspace"}}
		expected["openalbert"] = append(expected["openalbert"], dataMount{"computer/client", "/data/computer"})
	}
	var size int64
	for service, mounts := range expected {
		for _, mount := range mounts {
			for path := filepath.Join(c.Root, "container_data", mount.path); path != c.Root; path = filepath.Dir(path) {
				info, e := os.Lstat(path)
				if e != nil || info.Mode()&os.ModeSymlink != 0 {
					return nil, m, errors.New("backup data directories must not be symlinks")
				}
			}
			found := false
			for _, v := range config.Services[service].Volumes {
				if v.Target == mount.target {
					source := filepath.Clean(v.Source)
					found = v.Type == "bind" && (source == filepath.Join(c.Root, "container_data", mount.path) || (r.HostRoot != "" && source == filepath.Join(r.HostRoot, "container_data", mount.path)))
				}
			}
			if !found {
				return nil, m, errors.New("unsupported data mounts; backup requires the standard container_data layout")
			}
			err = filepath.WalkDir(filepath.Join(c.Root, "container_data", mount.path), func(_ string, entry os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				info, e := entry.Info()
				if e != nil {
					return e
				}
				if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
					return errors.New("backup data contains unsupported links or special files")
				}
				size += info.Size()
				return ctx.Err()
			})
			if err != nil {
				return nil, m, err
			}
		}
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(c.WorkDir, &stat); err != nil {
		return nil, m, err
	}
	if uint64(size)+(64<<20) > stat.Bavail*uint64(stat.Bsize) {
		return nil, m, errors.New("insufficient staging disk space")
	}
	raw, err = r.compose(ctx, "ps", "--status", "running", "--services")
	if err != nil {
		return nil, m, err
	}
	running := withoutController(strings.Fields(string(raw)))
	sort.Strings(running)
	for _, required := range []string{"openalbert", "temporal", "postgres", "minio"} {
		if !contains(running, required) {
			return nil, m, errors.New("all four core services must be running before backup")
		}
	}
	m = Manifest{Version: 1, CreatedAt: time.Now().UTC(), ComposeFile: c.ComposeFile, Images: map[string]string{}}
	command := r.Command
	if command == nil {
		command = RunCommand
	}
	for name, service := range config.Services {
		if name == ControllerService {
			continue
		}
		if service.Image == "" {
			return nil, m, errors.New("backup requires named images; build and tag local images first")
		}
		image := service.Image
		// A mutable tag may have been pulled since the container was started.
		ids, e := r.compose(ctx, "ps", "--all", "--quiet", name)
		if e != nil {
			return nil, m, e
		}
		if containers := strings.Fields(string(ids)); len(containers) > 0 {
			if dataMounts, ok := expected[name]; ok {
				mountJSON, e := command(ctx, "docker", "inspect", containers[0], "--format", "{{json .Mounts}}")
				if e != nil {
					return nil, m, e
				}
				var mounts []mount
				if json.Unmarshal(mountJSON, &mounts) != nil {
					return nil, m, errors.New("cannot inspect running data mounts")
				}
				hostRoot := r.HostRoot
				if hostRoot == "" {
					hostRoot = c.Root
				}
				for _, data := range dataMounts {
					found := false
					for _, v := range mounts {
						if v.Type == "bind" && v.Destination == data.target && filepath.Clean(v.Source) == filepath.Join(hostRoot, "container_data", data.path) {
							found = true
						}
					}
					if !found {
						return nil, m, errors.New("running service data differs from the installation being backed up")
					}
				}
			}
			installed, e := command(ctx, "docker", "inspect", containers[0], "--format", "{{.Image}}")
			if e != nil {
				return nil, m, e
			}
			image = strings.TrimSpace(string(installed))
		} else if _, required := expected[name]; required {
			return nil, m, errors.New("running core service container is missing")
		}
		raw, err = command(ctx, "docker", "image", "inspect", image, "--format", "{{json .RepoDigests}}")
		if err != nil {
			return nil, m, errors.New("cannot resolve installed image digest")
		}
		var digests []string
		if json.Unmarshal(raw, &digests) != nil || len(digests) == 0 {
			return nil, m, errors.New("all images must have registry digests for portable restore; use published images")
		}
		m.Images[name] = digests[0]
	}
	return running, m, nil
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (r *Runner) Create(ctx context.Context) (err error) {
	if err = r.Recover(ctx); err != nil {
		return err
	}
	lock, err := r.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	r.loadState()
	if !r.Config.Enabled {
		return r.save("idle", "")
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	r.state.RunID = time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:])
	code := "preflight_failed"
	defer func() {
		if err != nil {
			_ = r.save("failed", code)
			_ = r.emit("failed", code)
		}
	}()
	if err = r.save("preparing", ""); err != nil {
		return err
	}
	pulseCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go r.heartbeat(pulseCtx, done)
	defer func() { cancel(); <-done }()
	if r.Remote == nil {
		r.Remote, err = NewS3(ctx, r.Config)
		if err != nil {
			return err
		}
	}
	// Verify the remote before causing any downtime.
	if _, err = r.Remote.List(ctx); err != nil {
		return err
	}
	running, manifest, err := r.preflight(ctx)
	if err != nil {
		return err
	}
	if err = r.emit("starting", ""); err != nil {
		return err
	}
	if r.Config.NoticeSeconds > 0 {
		timer := time.NewTimer(time.Duration(r.Config.NoticeSeconds) * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	stage, err := os.MkdirTemp(r.Config.WorkDir, "run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	path := filepath.Join(stage, "archive.tar.gz.age")
	code = "local_copy_failed"
	if err = r.save("copying", ""); err != nil {
		return err
	}
	// Always attempt resumption, even if stop partially fails or the caller cancels.
	resume := func() error {
		cleanup, stop := context.WithTimeout(context.Background(), 4*time.Minute)
		defer stop()
		return r.resumeServices(cleanup, running)
	}
	stopped := true
	if err = AtomicJSON(filepath.Join(r.Config.StateDir, "resume.json"), running); err != nil {
		return err
	}
	defer func() {
		if stopped {
			resumeErr := resume()
			if resumeErr == nil {
				_ = os.Remove(filepath.Join(r.Config.StateDir, "resume.json"))
			} else {
				code = "restart_failed"
			}
			err = errors.Join(err, resumeErr)
		}
	}()
	if _, err = r.compose(ctx, append([]string{"stop", "--timeout", "90"}, running...)...); err != nil {
		return err
	}
	if remaining, e := r.compose(ctx, "ps", "--status", "running", "--services"); e != nil || len(withoutController(strings.Fields(string(remaining)))) != 0 {
		return errors.New("services did not stop; refusing a live file copy")
	}
	if err = archive(ctx, r.Config, path, manifest); err != nil {
		return err
	}
	code = "restart_failed"
	if err = r.save("restarting", ""); err != nil {
		return err
	}
	if err = resume(); err != nil {
		return err
	}
	stopped = false
	if err = os.Remove(filepath.Join(r.Config.StateDir, "resume.json")); err != nil {
		return err
	}
	code = "upload_failed"
	if err = r.save("uploading", ""); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, &contextReader{ctx: ctx, r: f})
	f.Close()
	if err != nil {
		return err
	}
	loc, _ := time.LoadLocation(r.Config.Timezone)
	copy := Copy{ID: r.state.RunID, Day: manifest.CreatedAt.In(loc).Format("2006-01-02"), CreatedAt: manifest.CreatedAt, Key: r.Config.Prefix + "copies/" + r.state.RunID + "/archive.tar.gz.age", SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}
	if err = r.Remote.Upload(ctx, copy, path); err != nil {
		return err
	}
	code = "rotation_failed"
	copies, err := r.Remote.List(ctx)
	if err != nil {
		return err
	}
	// Never rotate unless the newly committed copy is visible in the inventory.
	found := false
	for _, c := range copies {
		found = found || c.ID == copy.ID
	}
	if !found {
		return errors.New("new backup is absent from verified inventory")
	}
	r.mu.Lock()
	r.state.LastSuccess = &copy
	r.state.Copies = copies
	r.mu.Unlock()
	for _, old := range Expired(copies, r.Config.KeepDays) {
		if err = r.Remote.Delete(ctx, old); err != nil {
			return err
		}
	}
	copies, err = r.Remote.List(ctx)
	if err != nil {
		return err
	}
	if remote, ok := r.Remote.(interface {
		Sweep(context.Context, []Copy) error
	}); ok {
		if err = remote.Sweep(ctx, copies); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.state.Copies = copies
	r.state.NextRunAt = r.Config.NextRun(time.Now())
	r.mu.Unlock()
	if err = r.save("completed", ""); err != nil {
		return err
	}
	if err = r.emit("completed", ""); err != nil {
		return err
	}
	// Bound the public event spool. Temporal owns already-started event executions.
	entries, _ := os.ReadDir(filepath.Join(r.Config.StateDir, "events"))
	for _, entry := range entries {
		if info, e := entry.Info(); e == nil && info.ModTime().Before(time.Now().AddDate(0, 0, -90)) {
			_ = os.Remove(filepath.Join(r.Config.StateDir, "events", entry.Name()))
		}
	}
	return nil
}
