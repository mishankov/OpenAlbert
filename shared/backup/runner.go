package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var validID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{16}$`)
var ErrBusy = errors.New("another backup operation is running")

type Runner struct {
	Config Config
	Remote Remote
	mu     sync.Mutex
	state  State
}

func NewRunner(_ context.Context, c Config) (*Runner, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Runner{Config: c}, nil
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
	now := time.Now().UTC()
	r.state.UpdatedAt, r.state.HeartbeatAt = now, now
	return AtomicJSON(filepath.Join(r.Config.StateDir, "status.json"), r.state)
}

func (r *Runner) emit(kind, code string) error {
	if err := publicDirectory(filepath.Join(r.Config.StateDir, "events")); err != nil {
		return err
	}
	r.mu.Lock()
	runID := r.state.RunID
	r.mu.Unlock()
	e := Event{ID: runID + "-" + kind, RunID: runID, Kind: kind, At: time.Now().UTC(), ErrorCode: code}
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

func walkDirSize(ctx context.Context, dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			if path != dir && errors.Is(e, os.ErrNotExist) {
				return nil
			}
			return e
		}
		if !entry.IsDir() && isTempBackupFile(entry.Name()) {
			return nil
		}
		info, e := entry.Info()
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("backup data contains unsupported links or special files")
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return ctx.Err()
	})
	return total, err
}

func (r *Runner) preflight(ctx context.Context) (Manifest, error) {
	c := r.Config
	var m Manifest

	rootInfo, err := os.Lstat(c.Root)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return m, errors.New("installation root is missing, symlinked, or not a directory")
	}

	if err := os.MkdirAll(c.WorkDir, 0700); err != nil {
		return m, err
	}
	actualRoot, err := filepath.EvalSymlinks(c.Root)
	if err != nil {
		return m, err
	}
	actualWork, err := filepath.EvalSymlinks(c.WorkDir)
	if err != nil {
		return m, err
	}
	if within(actualRoot, actualWork) || within(actualWork, actualRoot) {
		return m, errors.New("staging directory resolves inside the installation")
	}
	if err := os.Chmod(c.WorkDir, 0700); err != nil {
		return m, err
	}

	dbInfo, ok, err := inspectRelativePath(c.Root, "data/openalbert.db")
	if err != nil {
		return m, err
	}
	if !ok || !dbInfo.Mode().IsRegular() {
		return m, errors.New("required data/openalbert.db is missing or not a regular file")
	}
	dbSize := dbInfo.Size()
	if walInfo, walErr := os.Lstat(filepath.Join(c.Root, "data", "openalbert.db-wal")); walErr == nil && walInfo.Mode().IsRegular() {
		dbSize += walInfo.Size()
	}

	var otherSize int64
	inspectDir := func(rel string, required bool) error {
		info, exists, err := inspectRelativePath(c.Root, rel)
		if err != nil {
			return err
		}
		if !exists {
			if required {
				return errors.New("required backup directory is missing: " + rel)
			}
			return nil
		}
		if !info.IsDir() {
			return errors.New("backup directory input is not a directory: " + rel)
		}
		sz, err := walkDirSize(ctx, filepath.Join(c.Root, rel))
		if err != nil {
			return err
		}
		otherSize += sz
		return nil
	}

	if err := inspectDir("data/knowledge", true); err != nil {
		return m, err
	}
	if err := inspectDir("data/artifacts", false); err != nil {
		return m, err
	}

	inspectOptionalFile := func(rel string) (bool, error) {
		info, exists, err := inspectRelativePath(c.Root, rel)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
		if !info.Mode().IsRegular() {
			return false, errors.New("backup input must be a regular file without symlinks: " + rel)
		}
		otherSize += info.Size()
		return true, nil
	}

	if _, err := inspectOptionalFile(".env"); err != nil {
		return m, err
	}

	modelCredentials := ""
	if ok, err := inspectOptionalFile("models.env"); err != nil {
		return m, err
	} else if ok {
		modelCredentials = "models.env"
	}

	modelConfigTarget := c.ModelConfigFile
	if modelConfigTarget == "" {
		modelConfigTarget = "models.toml"
	}
	modelConfig := ""
	if ok, err := inspectOptionalFile(modelConfigTarget); err != nil {
		return m, err
	} else if ok {
		modelConfig = filepath.ToSlash(modelConfigTarget)
	}

	composeFile := ""
	if c.ComposeFile != "" {
		if ok, err := inspectOptionalFile(c.ComposeFile); err != nil {
			return m, err
		} else if ok {
			composeFile = filepath.ToSlash(c.ComposeFile)
		}
	}

	// WorkDir holds both the SQLite snapshot and the encrypted archive before upload.
	var requiredSpace uint64 = 64 << 20
	if dbSize > 0 {
		requiredSpace += uint64(dbSize) * 2
	}
	if otherSize > 0 {
		requiredSpace += uint64(otherSize)
	}
	if err := checkDiskSpace(c.WorkDir, requiredSpace); err != nil {
		return m, err
	}

	m = Manifest{
		Version:              ManifestVersion,
		CreatedAt:            time.Now().UTC(),
		ComposeFile:          composeFile,
		ModelConfigFile:      modelConfig,
		ModelCredentialsFile: modelCredentials,
	}
	return m, nil
}

func (r *Runner) Create(ctx context.Context) (err error) {
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
	r.mu.Lock()
	r.state.RunID = time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:])
	runID := r.state.RunID
	r.mu.Unlock()

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
	if _, err = r.Remote.List(ctx); err != nil {
		return err
	}
	manifest, err := r.preflight(ctx)
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

	snapshotPath := filepath.Join(r.Config.WorkDir, "snapshot-"+runID+".db")
	defer os.Remove(snapshotPath)

	code = "snapshot_failed"
	if err = r.save("snapshotting", ""); err != nil {
		return err
	}
	if err = snapshotSQLite(ctx, filepath.Join(r.Config.Root, "data", "openalbert.db"), snapshotPath); err != nil {
		return err
	}

	path := filepath.Join(stage, "archive.tar.gz.age")
	code = "local_copy_failed"
	if err = r.save("copying", ""); err != nil {
		return err
	}
	if err = archive(ctx, r.Config, snapshotPath, path, manifest); err != nil {
		return err
	}
	_ = os.Remove(snapshotPath)

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
	copyDesc := Copy{
		ID:        runID,
		Day:       manifest.CreatedAt.In(loc).Format("2006-01-02"),
		CreatedAt: manifest.CreatedAt,
		Key:       r.Config.Prefix + "copies/" + runID + "/archive.tar.gz.age",
		SHA256:    hex.EncodeToString(hash.Sum(nil)),
		Size:      size,
	}
	if err = r.Remote.Upload(ctx, copyDesc, path); err != nil {
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
		found = found || c.ID == copyDesc.ID
	}
	if !found {
		return errors.New("new backup is absent from verified inventory")
	}
	r.mu.Lock()
	r.state.LastSuccess = &copyDesc
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
	// Bound the public event spool.
	entries, _ := os.ReadDir(filepath.Join(r.Config.StateDir, "events"))
	for _, entry := range entries {
		if info, e := entry.Info(); e == nil && info.ModTime().Before(time.Now().AddDate(0, 0, -90)) {
			_ = os.Remove(filepath.Join(r.Config.StateDir, "events", entry.Name()))
		}
	}
	return nil
}
