// Package blob stores immutable artifact bodies in S3 and ownership/metadata in PostgreSQL.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"openalbert/apps/backend/internal/domain"
)

const maxArtifactSize = 20 << 20

const artifactMetadata = "OpenAlbert-Artifact"
const digestMetadata = "OpenAlbert-Sha256"

type Store struct {
	domain.Repository
	client *minio.Client
	bucket string
	// Compose runs one OpenAlbert replica. Serialize its writes so a retry cannot
	// replace metadata, even when two activities refer to the same artifact.
	mu sync.Mutex
}

var _ domain.Repository = (*Store)(nil)

func New(ctx context.Context, repo domain.Repository, endpoint, accessKey, secretKey, bucket string, secure bool) (*Store, error) {
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: secure})
	if err != nil {
		return nil, err
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("artifact bucket is missing; run bucket initialization")
	}
	return &Store{Repository: repo, client: client, bucket: bucket}, nil
}
func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func key(scope, id string) string {
	return "artifacts/" + digest([]byte(scope)) + "/" + digest([]byte(id))
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
	if art.CreatedAt.IsZero() {
		art.CreatedAt = time.Now().UTC()
	}
	art.SizeBytes = int64(len(art.Data))
	metadata := art
	metadata.Data = nil
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	if len(encoded) > 6000 {
		return errors.New("artifact metadata exceeds S3 header limit")
	}
	opts := minio.PutObjectOptions{ContentType: art.MIME, DisableMultipart: true, UserMetadata: map[string]string{artifactMetadata: encoded, digestMetadata: digest(art.Data)}}
	// A crash can happen after S3 commits but before PostgreSQL commits metadata.
	// Conditional creation preserves the first bytes; the S3 metadata below lets
	// a replay finish the original PostgreSQL write rather than invent new metadata.
	opts.SetMatchETagExcept("*")
	_, err = s.client.PutObject(ctx, s.bucket, key(art.ConversationID, art.ID), bytes.NewReader(art.Data), int64(len(art.Data)), opts)
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.StatusCode != 412 && response.Code != "PreconditionFailed" {
			return err
		}
		info, statErr := s.client.StatObject(ctx, s.bucket, key(art.ConversationID, art.ID), minio.StatObjectOptions{})
		if statErr != nil {
			return statErr
		}
		original, metaErr := decodeMetadata(info, art.ConversationID, art.ID)
		if metaErr != nil {
			return metaErr
		}
		return s.Repository.SaveArtifact(ctx, original)
	}
	art.Data = nil
	return s.Repository.SaveArtifact(ctx, art)
}
func metadataValue(info minio.ObjectInfo, name string) string {
	for k, v := range info.UserMetadata {
		if strings.EqualFold(k, name) || strings.EqualFold(k, "X-Amz-Meta-"+name) {
			return v
		}
	}
	return info.Metadata.Get("X-Amz-Meta-" + name)
}
func decodeMetadata(info minio.ObjectInfo, scope, id string) (domain.Artifact, error) {
	raw, err := base64.StdEncoding.DecodeString(metadataValue(info, artifactMetadata))
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("invalid artifact object metadata: %w", err)
	}
	var art domain.Artifact
	if err = json.Unmarshal(raw, &art); err != nil {
		return art, fmt.Errorf("invalid artifact object metadata: %w", err)
	}
	if art.ConversationID != scope || art.ID != id {
		return art, errors.New("artifact object scope mismatch")
	}
	return art, nil
}
func (s *Store) Artifact(ctx context.Context, scope, id string) (domain.Artifact, bool, error) {
	art, ok, err := s.Repository.Artifact(ctx, scope, id)
	if err != nil || !ok {
		return art, ok, err
	}
	object, err := s.client.GetObject(ctx, s.bucket, key(scope, id), minio.GetObjectOptions{})
	if err != nil {
		return art, false, err
	}
	defer object.Close()
	data, err := io.ReadAll(io.LimitReader(object, maxArtifactSize+1))
	if err != nil {
		return art, false, err
	}
	if len(data) > maxArtifactSize {
		return art, false, errors.New("artifact exceeds limit")
	}
	info, err := object.Stat()
	if err != nil {
		return art, false, err
	}
	if _, err = decodeMetadata(info, scope, id); err != nil {
		return art, false, err
	}
	if expected := metadataValue(info, digestMetadata); expected == "" || expected != digest(data) {
		return art, false, errors.New("artifact integrity check failed")
	}
	art.Data = data
	return art, true, nil
}
func (s *Store) Ping(ctx context.Context) error {
	if err := s.Repository.Ping(ctx); err != nil {
		return err
	}
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("artifact bucket is missing")
	}
	return nil
}
