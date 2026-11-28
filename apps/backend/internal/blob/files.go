package blob

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func isInternalFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, metaSuffix) || strings.HasSuffix(lower, tmpSuffix)
}

func storedFileFromDisk(objectKey, bodyPath string, info fs.FileInfo) domain.StoredFile {
	out := domain.StoredFile{
		Key:        objectKey,
		Name:       path.Base(objectKey),
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}
	if meta, err := readSidecarFile(metaPath(bodyPath)); err == nil {
		art := meta.Artifact
		if art.ID != "" && art.ConversationID != "" && key(art.ConversationID, art.ID) == objectKey {
			if art.Name != "" {
				out.Name = art.Name
			}
			if art.MIME != "" {
				out.MIME = art.MIME
			}
			out.ConversationID, out.ArtifactID = art.ConversationID, art.ID
		}
	}
	return out
}

type filePosition struct {
	Key        string    `json:"key"`
	ModifiedAt time.Time `json:"modified_at"`
}

// Equal timestamps use the key as a stable tie-breaker.
func fileBefore(a, b domain.StoredFile) bool {
	if a.ModifiedAt.Equal(b.ModifiedAt) {
		return a.Key < b.Key
	}
	return a.ModifiedAt.After(b.ModifiedAt)
}

// Keep only the newest page and one lookahead object while scanning the
// artifacts tree. The oldest retained object is at the heap root.
type fileHeap []domain.StoredFile

func (h fileHeap) Len() int           { return len(h) }
func (h fileHeap) Less(i, j int) bool { return fileBefore(h[j], h[i]) }
func (h fileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(v any)        { *h = append(*h, v.(domain.StoredFile)) }
func (h *fileHeap) Pop() any {
	last := (*h)[len(*h)-1]
	*h = (*h)[:len(*h)-1]
	return last
}

// ListFiles walks the local artifacts directory, filters by prefix, and sorts
// matching files newest first with lexical key tie-breaking. Memory stays
// bounded to 101 objects; the cursor records time and key so pagination works
// even if the previous page's last object is later removed.
func (s *Store) ListFiles(ctx context.Context, prefix, after string) (domain.StoredFiles, error) {
	out := domain.StoredFiles{Bucket: localBucket, Files: []domain.StoredFile{}}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	var position filePosition
	if after != "" {
		if len(after) > 4096 {
			return out, fs.ErrInvalid
		}
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || json.Unmarshal(raw, &position) != nil || position.Key == "" || position.ModifiedAt.IsZero() {
			return out, fs.ErrInvalid
		}
	}
	page := make(fileHeap, 0)
	err := filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if p != s.dir && errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if isInternalFile(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(s.dir, p)
		if err != nil {
			return err
		}
		objectKey := "artifacts/" + filepath.ToSlash(rel)
		if prefix != "" && !strings.HasPrefix(objectKey, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file := storedFileFromDisk(objectKey, p, info)
		if after != "" && !fileBefore(domain.StoredFile{Key: position.Key, ModifiedAt: position.ModifiedAt}, file) {
			return nil
		}
		if len(page) < 101 {
			heap.Push(&page, file)
		} else if fileBefore(file, page[0]) {
			page[0] = file
			heap.Fix(&page, 0)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	sort.Slice(page, func(i, j int) bool { return fileBefore(page[i], page[j]) })
	if len(page) > 100 {
		page = page[:100]
		last := page[99]
		raw, _ := json.Marshal(filePosition{Key: last.Key, ModifiedAt: last.ModifiedAt})
		out.NextAfter = base64.RawURLEncoding.EncodeToString(raw)
	}
	out.Files = []domain.StoredFile(page)
	return out, nil
}

func (s *Store) resolveObjectKey(objectKey string) (string, bool) {
	if objectKey == "" || strings.ContainsRune(objectKey, 0) || strings.Contains(objectKey, "\\") {
		return "", false
	}
	if strings.HasPrefix(objectKey, "/") || strings.HasSuffix(objectKey, "/") {
		return "", false
	}
	if !fs.ValidPath(objectKey) || path.Clean(objectKey) != objectKey {
		return "", false
	}
	if !strings.HasPrefix(objectKey, "artifacts/") {
		return "", false
	}
	if isInternalFile(objectKey) {
		return "", false
	}
	relKey := strings.TrimPrefix(objectKey, "artifacts/")
	if relKey == "" {
		return "", false
	}
	fullPath := filepath.Clean(filepath.Join(s.dir, filepath.FromSlash(relKey)))
	rel, err := filepath.Rel(s.dir, fullPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return fullPath, true
}

func (s *Store) OpenFile(ctx context.Context, objectKey string) (domain.StoredFile, io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return domain.StoredFile{}, nil, err
	}
	fullPath, ok := s.resolveObjectKey(objectKey)
	if !ok {
		return domain.StoredFile{}, nil, fs.ErrNotExist
	}
	linfo, err := os.Lstat(fullPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.StoredFile{}, nil, fs.ErrNotExist
		}
		return domain.StoredFile{}, nil, err
	}
	if !linfo.Mode().IsRegular() {
		return domain.StoredFile{}, nil, fs.ErrNotExist
	}
	evalRoot, err := filepath.EvalSymlinks(s.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.StoredFile{}, nil, fs.ErrNotExist
		}
		return domain.StoredFile{}, nil, err
	}
	evalPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.StoredFile{}, nil, fs.ErrNotExist
		}
		return domain.StoredFile{}, nil, err
	}
	rel, err := filepath.Rel(evalRoot, evalPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return domain.StoredFile{}, nil, fs.ErrNotExist
	}
	f, err := os.Open(evalPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return domain.StoredFile{}, nil, fs.ErrNotExist
		}
		return domain.StoredFile{}, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return domain.StoredFile{}, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return domain.StoredFile{}, nil, fs.ErrNotExist
	}
	return storedFileFromDisk(objectKey, evalPath, info), f, nil
}
