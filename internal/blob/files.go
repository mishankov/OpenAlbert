package blob

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"jarvis/internal/domain"
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

// ListFiles reads a bounded page directly from S3, including objects without
// database metadata. StartAfter preserves S3's lexical ordering across pages.
func (s *Store) ListFiles(ctx context.Context, prefix, after string) (domain.StoredFiles, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := domain.StoredFiles{Bucket: s.bucket, Files: []domain.StoredFile{}}
	for info := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, StartAfter: after, Recursive: true, WithMetadata: true, MaxKeys: 101}) {
		if info.Err != nil {
			return out, info.Err
		}
		if len(out.Files) == 100 {
			out.NextAfter = out.Files[99].Key
			break
		}
		out.Files = append(out.Files, storedFile(info))
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
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
