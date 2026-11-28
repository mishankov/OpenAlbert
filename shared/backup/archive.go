package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

const ManifestVersion = 2

type Manifest struct {
	Version              int       `json:"version"`
	CreatedAt            time.Time `json:"created_at"`
	ComposeFile          string    `json:"compose_file,omitempty"`
	ModelConfigFile      string    `json:"model_config_file,omitempty"`
	ModelCredentialsFile string    `json:"model_credentials_file,omitempty"`
}

// isTempBackupFile matches atomic-write temp files created by OpenAlbert's
// knowledge, blob, and backup state writers so online backups never archive or
// fail on in-flight temporary names.
func isTempBackupFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(name, ".openalbert-") ||
		strings.HasPrefix(name, ".tmp-") ||
		strings.HasSuffix(lower, ".tmp")
}

// inspectRelativePath checks each path segment below root top-down so a
// symlinked parent directory is caught even if the leaf does not exist.
func inspectRelativePath(root, rel string) (os.FileInfo, bool, error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	cur := root
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, false, errors.New("invalid relative backup path")
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, errors.New("backup inputs must be regular files or directories, not symlinks/devices")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, false, errors.New("backup input parent is not a directory")
		}
		if i == len(parts)-1 {
			return info, true, nil
		}
	}
	return nil, false, errors.New("empty relative backup path")
}

func archive(ctx context.Context, c Config, snapshotPath, output string, manifest Manifest) (err error) {
	if manifest.Version != ManifestVersion {
		return errors.New("unsupported backup manifest version")
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	recipient, err := age.ParseX25519Recipient(c.Recipient)
	if err != nil {
		return err
	}
	encrypted, err := age.Encrypt(f, recipient)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, encrypted.Close()) }()
	gz := gzip.NewWriter(encrypted)
	defer func() { err = errors.Join(err, gz.Close()) }()
	tw := tar.NewWriter(gz)
	defer func() { err = errors.Join(err, tw.Close()) }()

	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err = tw.WriteHeader(&tar.Header{Name: "backup-manifest.json", Mode: 0600, Uid: os.Getuid(), Gid: os.Getgid(), Size: int64(len(raw)), ModTime: manifest.CreatedAt}); err != nil {
		return err
	}
	if _, err = tw.Write(raw); err != nil {
		return err
	}

	writeFile := func(rel, srcPath string, metaInfo os.FileInfo, optional bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			input *os.File
			fInfo os.FileInfo
		)
		const maxOpenAttempts = 5
		for attempt := range maxOpenAttempts {
			lstatInfo, err := os.Lstat(srcPath)
			if err != nil {
				if optional && errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if lstatInfo.Mode()&os.ModeSymlink != 0 || !lstatInfo.Mode().IsRegular() {
				return errors.New("backup inputs must be regular files or directories, not symlinks/devices")
			}
			f, err := os.Open(srcPath)
			if err != nil {
				if optional && errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			info, statErr := f.Stat()
			if statErr != nil {
				return errors.Join(statErr, f.Close())
			}
			if !info.Mode().IsRegular() {
				return errors.Join(errors.New("backup inputs must be regular files or directories, not symlinks/devices"), f.Close())
			}
			if os.SameFile(lstatInfo, info) {
				input, fInfo = f, info
				break
			}
			_ = f.Close()
			if attempt == maxOpenAttempts-1 {
				return errors.New("file changed repeatedly during archive")
			}
		}
		headerInfo := metaInfo
		if headerInfo == nil {
			headerInfo = fInfo
		}
		h, err := tar.FileInfoHeader(headerInfo, "")
		if err != nil {
			return errors.Join(err, input.Close())
		}
		h.Name = filepath.ToSlash(rel)
		h.Size = fInfo.Size()
		h.ModTime = fInfo.ModTime()
		if err = tw.WriteHeader(h); err != nil {
			return errors.Join(err, input.Close())
		}
		written, copyErr := io.Copy(tw, &contextReader{ctx: ctx, r: input})
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
		if written != h.Size {
			return errors.New("file size changed during archive")
		}
		return nil
	}

	// Archive data directory header and online SQLite snapshot (never the live DB or WAL/SHM files).
	dataInfo, ok, err := inspectRelativePath(c.Root, "data")
	if err != nil {
		return err
	}
	if !ok || !dataInfo.IsDir() {
		return errors.New("required data directory is missing")
	}
	dataHeader, err := tar.FileInfoHeader(dataInfo, "")
	if err != nil {
		return err
	}
	dataHeader.Name = "data"
	if err = tw.WriteHeader(dataHeader); err != nil {
		return err
	}

	origDBInfo, ok, err := inspectRelativePath(c.Root, "data/openalbert.db")
	if err != nil {
		return err
	}
	if !ok || !origDBInfo.Mode().IsRegular() {
		return errors.New("required data/openalbert.db is missing")
	}
	if err = writeFile("data/openalbert.db", snapshotPath, origDBInfo, false); err != nil {
		return err
	}

	walkDir := func(rel string, required bool) error {
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
		rootDir := filepath.Join(c.Root, rel)
		return filepath.WalkDir(rootDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if path != rootDir && errors.Is(walkErr, os.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.IsDir() && isTempBackupFile(entry.Name()) {
				return nil
			}
			fi, err := entry.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if fi.Mode()&os.ModeSymlink != 0 || (!fi.Mode().IsRegular() && !fi.IsDir()) {
				return errors.New("backup inputs must be regular files or directories, not symlinks/devices")
			}
			entryRel, err := filepath.Rel(c.Root, path)
			if err != nil {
				return err
			}
			if fi.IsDir() {
				h, err := tar.FileInfoHeader(fi, "")
				if err != nil {
					return err
				}
				h.Name = filepath.ToSlash(entryRel)
				return tw.WriteHeader(h)
			}
			return writeFile(entryRel, path, nil, true)
		})
	}

	if err = walkDir("data/knowledge", true); err != nil {
		return err
	}
	if err = walkDir("data/artifacts", false); err != nil {
		return err
	}

	archiveFile := func(rel string, required bool) error {
		info, exists, err := inspectRelativePath(c.Root, rel)
		if err != nil {
			return err
		}
		if !exists {
			if required {
				return errors.New("required backup file is missing: " + rel)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("backup file input must be a regular file: " + rel)
		}
		return writeFile(rel, filepath.Join(c.Root, rel), info, !required)
	}

	if err = archiveFile(".env", false); err != nil {
		return err
	}
	if manifest.ModelCredentialsFile != "" {
		if manifest.ModelCredentialsFile != "models.env" {
			return errors.New("invalid model credentials archive path")
		}
		if err = archiveFile(manifest.ModelCredentialsFile, true); err != nil {
			return err
		}
	}
	if manifest.ModelConfigFile != "" {
		if !validModelConfigPath(manifest.ModelConfigFile) {
			return errors.New("invalid model configuration archive path")
		}
		if err = archiveFile(manifest.ModelConfigFile, true); err != nil {
			return err
		}
	}
	if manifest.ComposeFile != "" {
		if !validComposeFile(manifest.ComposeFile) {
			return errors.New("invalid compose archive path")
		}
		if err = archiveFile(manifest.ComposeFile, true); err != nil {
			return err
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Restore only writes into a newly created directory and verifies the restored SQLite snapshot.
func Restore(ctx context.Context, encryptedPath, identityPath, destination string) (err error) {
	if !filepath.IsAbs(destination) {
		return errors.New("restore destination must be absolute")
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		return errors.New("restore destination must not already exist")
	}
	defer func() {
		if err != nil {
			os.RemoveAll(destination)
		}
	}()
	key, err := os.Open(identityPath)
	if err != nil {
		return err
	}
	identities, err := age.ParseIdentities(io.LimitReader(key, 1<<20))
	key.Close()
	if err != nil {
		return errors.New("invalid age identity file")
	}
	f, err := os.Open(encryptedPath)
	if err != nil {
		return err
	}
	defer f.Close()
	plaintext, err := age.Decrypt(f, identities...)
	if err != nil {
		return errors.New("backup decryption failed")
	}
	gz, err := gzip.NewReader(plaintext)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var directories []*tar.Header
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		slashName := filepath.ToSlash(filepath.Clean(name))
		target := filepath.Join(destination, name)
		if filepath.IsAbs(name) || !within(destination, target) || target == destination || seen[target] {
			return errors.New("unsafe or duplicate archive path")
		}
		if slashName == "data/openalbert.db-wal" || slashName == "data/openalbert.db-shm" {
			return errors.New("archive must not contain live sqlite WAL/SHM files")
		}
		seen[target] = true
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			return errors.New("archive contains unsupported file type")
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			headerCopy := *h
			directories = append(directories, &headerCopy)
			continue
		}
		out, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, &contextReader{ctx: ctx, r: tr})
		e = errors.Join(e, out.Close())
		if e != nil {
			return e
		}
		if err = restoreMetadata(target, h); err != nil {
			return err
		}
	}
	// Drain gzip and age to verify their trailers/authentication, including truncation.
	if _, err = io.Copy(io.Discard, gz); err != nil {
		return err
	}
	if _, err = io.Copy(io.Discard, plaintext); err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err = restoreMetadata(filepath.Join(destination, filepath.FromSlash(directories[i].Name)), directories[i]); err != nil {
			return err
		}
	}
	var m Manifest
	if err = readJSON(filepath.Join(destination, "backup-manifest.json"), &m); err != nil || m.Version != ManifestVersion {
		return errors.New("missing or invalid restore manifest")
	}
	if m.ComposeFile != "" {
		if !validComposeFile(m.ComposeFile) {
			return errors.New("invalid compose restore path")
		}
		if info, e := os.Lstat(filepath.Join(destination, m.ComposeFile)); e != nil || !info.Mode().IsRegular() {
			return errors.New("restored compose file is missing or not a regular file")
		}
	}
	if m.ModelCredentialsFile != "" {
		if m.ModelCredentialsFile != "models.env" {
			return errors.New("invalid model credentials restore path")
		}
		if info, e := os.Lstat(filepath.Join(destination, m.ModelCredentialsFile)); e != nil || !info.Mode().IsRegular() {
			return errors.New("restored model credentials are not a regular file")
		}
	}
	if m.ModelConfigFile != "" {
		if !validModelConfigPath(m.ModelConfigFile) {
			return errors.New("invalid model configuration restore path")
		}
		if info, e := os.Lstat(filepath.Join(destination, m.ModelConfigFile)); e != nil || !info.Mode().IsRegular() {
			return errors.New("restored model configuration is not a regular file")
		}
	}
	dbPath := filepath.Join(destination, "data", "openalbert.db")
	if info, e := os.Lstat(dbPath); e != nil || !info.Mode().IsRegular() {
		return errors.New("incomplete backup archive: missing data/openalbert.db")
	}
	knowledgePath := filepath.Join(destination, "data", "knowledge")
	if info, e := os.Lstat(knowledgePath); e != nil || !info.IsDir() {
		return errors.New("incomplete backup archive: missing data/knowledge")
	}
	if err = verifySQLite(ctx, dbPath); err != nil {
		return err
	}
	return nil
}

func restoreMetadata(path string, h *tar.Header) error {
	if os.Geteuid() == 0 {
		if err := os.Chown(path, h.Uid, h.Gid); err != nil {
			return err
		}
	}
	if err := os.Chmod(path, os.FileMode(h.Mode)&0777); err != nil {
		return err
	}
	return os.Chtimes(path, h.ModTime, h.ModTime)
}
