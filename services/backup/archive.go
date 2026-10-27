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
	"syscall"
	"time"

	"filippo.io/age"
)

type Manifest struct {
	Version     int               `json:"version"`
	CreatedAt   time.Time         `json:"created_at"`
	ComposeFile string            `json:"compose_file"`
	Images      map[string]string `json:"images"`
}

func archive(ctx context.Context, c Config, output string, manifest Manifest) (err error) {
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
	raw, _ := json.Marshal(manifest)
	if err = tw.WriteHeader(&tar.Header{Name: "backup-manifest.json", Mode: 0600, Uid: os.Getuid(), Gid: os.Getgid(), Size: int64(len(raw)), ModTime: manifest.CreatedAt}); err != nil {
		return err
	}
	if _, err = tw.Write(raw); err != nil {
		return err
	}
	paths := []string{".env", c.ComposeFile, "container_data/postgres", "container_data/minio", "container_data/knowledge"}
	if filepath.Base(c.ComposeFile) == "compose.local.yaml" {
		paths = append(paths, "services/postgres/init.sql", "services/temporal/dynamicconfig.yaml")
	}
	if _, ok := manifest.Images["computer"]; ok {
		paths = append(paths, "container_data/computer")
	}
	for _, name := range paths {
		err = filepath.WalkDir(filepath.Join(c.Root, name), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			// External tablespaces, symlinked data and device files require a different backup layout.
			if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
				return errors.New("backup inputs must be regular files or directories, not symlinks/devices")
			}
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name, _ = filepath.Rel(c.Root, path)
			h.Name = filepath.ToSlash(h.Name)
			if err = tw.WriteHeader(h); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, &contextReader{ctx: ctx, r: input})
			return errors.Join(err, input.Close())
		})
		if err != nil {
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

// Restore only writes into a newly created directory. It never starts services.
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
		target := filepath.Join(destination, name)
		if filepath.IsAbs(name) || !within(destination, target) || target == destination || seen[target] {
			return errors.New("unsafe or duplicate archive path")
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
			copy := *h
			directories = append(directories, &copy)
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
	if err = readJSON(filepath.Join(destination, "backup-manifest.json"), &m); err != nil || m.Version != 1 || !validComposeFile(m.ComposeFile) {
		return errors.New("missing or invalid restore manifest")
	}
	requiredPaths := []string{".env", m.ComposeFile, "container_data/postgres", "container_data/minio", "container_data/knowledge"}
	if _, ok := m.Images["computer"]; ok {
		requiredPaths = append(requiredPaths, "container_data/computer")
	}
	for _, required := range requiredPaths {
		if required == "" || strings.Contains(required, "..") {
			return errors.New("invalid restore manifest path")
		}
		if _, err = os.Stat(filepath.Join(destination, required)); err != nil {
			return errors.New("incomplete backup archive")
		}
	}
	services := map[string]map[string]string{}
	for name, image := range m.Images {
		services[name] = map[string]string{"image": image}
	}
	if len(services) == 0 {
		return errors.New("restore manifest has no image digests")
	}
	if err = AtomicJSON(filepath.Join(destination, "compose.backup-images.json"), map[string]any{"services": services}); err != nil {
		return err
	}
	return nil
}

func restoreMetadata(path string, h *tar.Header) error {
	if os.Geteuid() == 0 {
		if err := os.Chown(path, h.Uid, h.Gid); err != nil {
			return err
		}
	} else if st, err := os.Stat(path); err != nil {
		return err
	} else if stat, ok := st.Sys().(*syscall.Stat_t); ok && (int(stat.Uid) != h.Uid || int(stat.Gid) != h.Gid) {
		return errors.New("restore requires root to preserve original ownership")
	}
	if err := os.Chmod(path, os.FileMode(h.Mode)&0777); err != nil {
		return err
	}
	return os.Chtimes(path, h.ModTime, h.ModTime)
}
