package blob

import (
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"openalbert/apps/backend/internal/domain"
)

func storedFile(info minio.ObjectInfo) domain.StoredFile {
	out := domain.StoredFile{Key: info.Key, Name: path.Base(info.Key), MIME: info.ContentType, Size: info.Size, ModifiedAt: info.LastModified}
	if out.MIME == "" {
		for name, value := range info.UserMetadata {
			if strings.EqualFold(name, "content-type") {
				out.MIME = value
			}
		}
	}
	// MinIO includes user metadata in listings. Other S3 servers may only return
	// it on StatObject; the object key remains a usable fallback in that case.
	raw, err := base64.StdEncoding.DecodeString(metadataValue(info, artifactMetadata))
	var art domain.Artifact
	if err == nil && json.Unmarshal(raw, &art) == nil && art.ID != "" && art.ConversationID != "" && key(art.ConversationID, art.ID) == info.Key {
		if art.Name != "" {
			out.Name = art.Name
		}
		out.ConversationID, out.ArtifactID = art.ConversationID, art.ID
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

// Keep only the newest page and one lookahead object while scanning S3's
// lexically ordered listing. The oldest retained object is at the heap root.
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

// ListFiles sorts the entire matching bucket listing by modification time,
// newest first. S3 has no date-order listing, so each page scans metadata only;
// memory stays bounded to 101 objects. The cursor records time and key rather
// than relying on the previous page's object still existing.
func (s *Store) ListFiles(ctx context.Context, prefix, after string) (domain.StoredFiles, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := domain.StoredFiles{Bucket: s.bucket, Files: []domain.StoredFile{}}
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
	page := fileHeap{}
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true, WithMetadata: true, MaxKeys: 1000}) {
		if info.Err != nil {
			return out, info.Err
		}
		file := storedFile(info)
		if after != "" && !fileBefore(domain.StoredFile{Key: position.Key, ModifiedAt: position.ModifiedAt}, file) {
			continue
		}
		if len(page) < 101 {
			heap.Push(&page, file)
		} else if fileBefore(file, page[0]) {
			page[0] = file
			heap.Fix(&page, 0)
		}
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

func (s *Store) OpenFile(ctx context.Context, objectKey string) (domain.StoredFile, io.ReadSeekCloser, error) {
	object, err := s.client.GetObject(ctx, s.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return domain.StoredFile{}, nil, err
	}
	info, err := object.Stat()
	if err != nil {
		_ = object.Close()
		if response := minio.ToErrorResponse(err); response.Code == "NoSuchKey" || response.Code == "NoSuchObject" {
			return domain.StoredFile{}, nil, fs.ErrNotExist
		}
		return domain.StoredFile{}, nil, err
	}
	return storedFile(info), object, nil
}
