// Package blob stores immutable artifact bodies on the local filesystem and ownership/metadata in SQLite.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"openalbert/apps/backend/internal/domain"
)

const (
	maxArtifactSize = 20 << 20
	maxMetadataSize = 1 << 20
	metaSuffix      = ".meta.json"
	tmpSuffix       = ".tmp"
	localBucket     = "local"
	dirPerm         = 0o700
)

type sidecarMetadata struct {
	Artifact domain.Artifact `json:"artifact"`
	SHA256   string          `json:"sha256"`
}

type Store struct {
	domain.Repository
	dir string
	// Serialize writes so a retry cannot replace metadata or race on
	// atomic creation of the same artifact key.
	mu sync.Mutex
}

var _ domain.Repository = (*Store)(nil)

func New(repo domain.Repository, dir string) (*Store, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return nil, errors.New("artifact directory is required")
	}
	absDir, err := filepath.Abs(filepath.Clean(trimmed))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absDir, dirPerm); err != nil {
		return nil, err
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("artifact directory path is not a directory")
	}
	return &Store{Repository: repo, dir: absDir}, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func key(scope, id string) string {
	return "artifacts/" + digest([]byte(scope)) + "/" + digest([]byte(id))
}

func (s *Store) objectPath(objectKey string) string {
	rel := strings.TrimPrefix(objectKey, "artifacts/")
	return filepath.Join(s.dir, filepath.FromSlash(rel))
}

func metaPath(bodyPath string) string {
	return bodyPath + metaSuffix
}

func validate(art domain.Artifact) error {
	if strings.TrimSpace(art.ID) == "" || strings.TrimSpace(art.ConversationID) == "" {
		return errors.New("artifact ID and conversation are required")
	}
	if len(art.Data) > maxArtifactSize {
		return errors.New("artifact exceeds 20 MiB")
	}
	return nil
}

func writeFileAtomic(dst string, data []byte) (retErr error) {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".*"+tmpSuffix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	return nil
}

func readSidecarFile(path string) (sidecarMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return sidecarMetadata{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxMetadataSize+1))
	if err != nil {
		return sidecarMetadata{}, err
	}
	if len(raw) > maxMetadataSize {
		return sidecarMetadata{}, errors.New("artifact metadata exceeds limit")
	}
	var meta sidecarMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return sidecarMetadata{}, err
	}
	return meta, nil
}

func readAndVerifySidecar(path, scope, id string) (sidecarMetadata, error) {
	meta, err := readSidecarFile(path)
	if err != nil {
		return sidecarMetadata{}, fmt.Errorf("invalid artifact object metadata: %w", err)
	}
	if meta.Artifact.ConversationID != scope || meta.Artifact.ID != id {
		return sidecarMetadata{}, errors.New("artifact object scope mismatch")
	}
	return meta, nil
}

func (s *Store) SaveArtifact(ctx context.Context, art domain.Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validate(art); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists, err := s.Repository.Artifact(ctx, art.ConversationID, art.ID); err != nil {
		return err
	} else if exists {
		return nil
	}

	bodyPath := s.objectPath(key(art.ConversationID, art.ID))
	sidecarPath := metaPath(bodyPath)

	// A crash can happen after local disk writes commit but before SQLite
	// commits metadata. Preserve the first bytes on disk and replay the original
	// metadata from the sidecar rather than inventing new metadata.
	if _, err := os.Stat(bodyPath); err == nil {
		meta, metaErr := readAndVerifySidecar(sidecarPath, art.ConversationID, art.ID)
		if metaErr != nil {
			return metaErr
		}
		if meta.SHA256 == "" {
			return errors.New("artifact integrity check failed")
		}
		original := meta.Artifact
		original.Data = nil
		return s.Repository.SaveArtifact(ctx, original)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if art.CreatedAt.IsZero() {
		art.CreatedAt = time.Now().UTC()
	}
	art.SizeBytes = int64(len(art.Data))

	metaArt := art
	metaArt.Data = nil
	rawMeta, err := json.Marshal(sidecarMetadata{
		Artifact: metaArt,
		SHA256:   digest(art.Data),
	})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(bodyPath), dirPerm); err != nil {
		return err
	}
	// Write sidecar before body so body never exists on disk without valid metadata.
	if err := writeFileAtomic(sidecarPath, rawMeta); err != nil {
		return err
	}
	if err := writeFileAtomic(bodyPath, art.Data); err != nil {
		return err
	}

	art.Data = nil
	return s.Repository.SaveArtifact(ctx, art)
}

func (s *Store) Artifact(ctx context.Context, scope, id string) (domain.Artifact, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.Artifact{}, false, err
	}
	art, ok, err := s.Repository.Artifact(ctx, scope, id)
	if err != nil || !ok {
		return art, ok, err
	}
	bodyPath := s.objectPath(key(scope, id))
	f, err := os.Open(bodyPath)
	if err != nil {
		return art, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return art, false, err
	}
	if !info.Mode().IsRegular() {
		return art, false, errors.New("artifact object is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxArtifactSize+1))
	if err != nil {
		return art, false, err
	}
	if len(data) > maxArtifactSize {
		return art, false, errors.New("artifact exceeds limit")
	}
	meta, err := readAndVerifySidecar(metaPath(bodyPath), scope, id)
	if err != nil {
		return art, false, err
	}
	if meta.SHA256 == "" || meta.SHA256 != digest(data) {
		return art, false, errors.New("artifact integrity check failed")
	}
	art.Data = data
	return art, true, nil
}

func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Repository.Ping(ctx); err != nil {
		return err
	}
	f, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("artifact directory is inaccessible: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("artifact directory is inaccessible: %w", err)
	}
	if !info.IsDir() {
		return errors.New("artifact directory path is not a directory")
	}
	return nil
}
