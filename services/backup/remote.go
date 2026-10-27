package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Remote interface {
	List(context.Context) ([]Copy, error)
	Upload(context.Context, Copy, string) error
	Download(context.Context, Copy, string) error
	Delete(context.Context, Copy) error
}

type S3 struct {
	client         *minio.Client
	bucket, prefix string
}

func NewS3(ctx context.Context, c Config) (*S3, error) {
	key, secret := os.Getenv("BACKUP_S3_ACCESS_KEY"), os.Getenv("BACKUP_S3_SECRET_KEY")
	token := os.Getenv("BACKUP_S3_SESSION_TOKEN")
	if c.CredentialsFile != "" {
		var credentials struct {
			AccessKey    string `json:"access_key"`
			SecretKey    string `json:"secret_key"`
			SessionToken string `json:"session_token"`
		}
		if err := readJSON(c.CredentialsFile, &credentials); err != nil {
			return nil, errors.New("cannot read backup S3 credentials file")
		}
		key, secret, token = credentials.AccessKey, credentials.SecretKey, credentials.SessionToken
	}
	if key == "" || secret == "" {
		return nil, errors.New("backup S3 access and secret keys are required (credentials_file or BACKUP_S3_* environment)")
	}
	client, err := minio.New(c.Endpoint, &minio.Options{Creds: credentials.NewStaticV4(key, secret, token), Secure: c.Secure, Region: c.Region})
	if err != nil {
		return nil, errors.New("invalid backup S3 configuration")
	}
	if ok, err := client.BucketExists(ctx, c.Bucket); err != nil || !ok {
		return nil, errors.New("backup bucket is unavailable")
	}
	versioning, err := client.GetBucketVersioning(ctx, c.Bucket)
	if err != nil {
		return nil, errors.New("cannot verify backup bucket versioning")
	}
	if versioning.Status != "" {
		return nil, errors.New("backup bucket must have versioning disabled; otherwise rotation leaves stored versions")
	}
	return &S3{client: client, bucket: c.Bucket, prefix: c.Prefix + "copies/"}, nil
}

func (s *S3) valid(c Copy) bool {
	_, dateErr := time.Parse(time.DateOnly, c.Day)
	checksum, hashErr := hex.DecodeString(c.SHA256)
	return validID.MatchString(c.ID) && c.Key == s.prefix+c.ID+"/archive.tar.gz.age" && hashErr == nil && len(checksum) == 32 && c.Size > 0 && !c.CreatedAt.IsZero() && dateErr == nil
}

// Sweep only removes uncommitted archives left by interrupted uploads. Completed
// copies are governed exclusively by successful-date retention.
func (s *S3) Sweep(ctx context.Context, copies []Copy) error {
	committed := map[string]bool{}
	for _, c := range copies {
		committed[c.Key] = true
	}
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix, Recursive: true}) {
		if object.Err != nil {
			return errors.New("cannot list incomplete backup objects")
		}
		rel := strings.TrimPrefix(object.Key, s.prefix)
		id, tail, _ := strings.Cut(rel, "/")
		if !validID.MatchString(id) || tail != "archive.tar.gz.age" || committed[object.Key] || object.LastModified.After(time.Now().Add(-48*time.Hour)) {
			continue
		}
		if err := s.client.RemoveObject(ctx, s.bucket, object.Key, minio.RemoveObjectOptions{}); err != nil {
			return errors.New("cannot remove incomplete backup object")
		}
	}
	return nil
}

func (s *S3) List(ctx context.Context) ([]Copy, error) {
	var copies []Copy
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix, Recursive: true}) {
		if object.Err != nil {
			return nil, errors.New("cannot list backup objects")
		}
		if !strings.HasSuffix(object.Key, "/complete.json") {
			continue
		}
		o, err := s.client.GetObject(ctx, s.bucket, object.Key, minio.GetObjectOptions{})
		if err != nil {
			return nil, errors.New("cannot read backup manifest")
		}
		var c Copy
		err = json.NewDecoder(io.LimitReader(o, 8192)).Decode(&c)
		o.Close()
		if err != nil || !s.valid(c) || object.Key != s.prefix+c.ID+"/complete.json" {
			return nil, errors.New("invalid backup manifest; rotation refused")
		}
		copies = append(copies, c)
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].CreatedAt.After(copies[j].CreatedAt) })
	return copies, nil
}

func (s *S3) Upload(ctx context.Context, c Copy, path string) error {
	if !s.valid(c) {
		return errors.New("invalid backup descriptor")
	}
	if _, err := s.client.FPutObject(ctx, s.bucket, c.Key, path, minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return errors.New("backup upload failed")
	}
	// Read back all encrypted bytes. ETag is not a checksum for multipart uploads.
	o, err := s.client.GetObject(ctx, s.bucket, c.Key, minio.GetObjectOptions{})
	if err != nil {
		return errors.New("backup verification failed")
	}
	h := sha256.New()
	n, err := io.Copy(h, o)
	o.Close()
	if err != nil || n != c.Size || hex.EncodeToString(h.Sum(nil)) != c.SHA256 {
		return errors.New("backup verification failed")
	}
	raw, _ := json.Marshal(c)
	_, err = s.client.PutObject(ctx, s.bucket, s.prefix+c.ID+"/complete.json", strings.NewReader(string(raw)), int64(len(raw)), minio.PutObjectOptions{ContentType: "application/json", DisableMultipart: true})
	if err != nil {
		return errors.New("cannot commit backup manifest")
	}
	return nil
}

func (s *S3) Download(ctx context.Context, c Copy, path string) error {
	if !s.valid(c) {
		return errors.New("invalid backup descriptor")
	}
	o, err := s.client.GetObject(ctx, s.bucket, c.Key, minio.GetObjectOptions{})
	if err != nil {
		return errors.New("backup download failed")
	}
	defer o.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), o)
	err = errors.Join(copyErr, f.Close())
	if err != nil || n != c.Size || hex.EncodeToString(h.Sum(nil)) != c.SHA256 {
		os.Remove(path)
		return errors.New("downloaded backup checksum mismatch")
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, c Copy) error {
	if !s.valid(c) {
		return errors.New("invalid backup descriptor")
	}
	if err := s.client.RemoveObject(ctx, s.bucket, c.Key, minio.RemoveObjectOptions{}); err != nil {
		return errors.New("cannot delete expired backup")
	}
	if err := s.client.RemoveObject(ctx, s.bucket, s.prefix+c.ID+"/complete.json", minio.RemoveObjectOptions{}); err != nil {
		return errors.New("cannot delete expired backup manifest")
	}
	return nil
}

// Expired counts distinct successful local dates, not elapsed days or attempts.
func Expired(copies []Copy, keepDays int) []Copy {
	sorted := append([]Copy(nil), copies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt) })
	days := map[string]bool{}
	var expired []Copy
	for _, c := range sorted {
		if !days[c.Day] && len(days) >= keepDays {
			expired = append(expired, c)
			continue
		}
		days[c.Day] = true
	}
	return expired
}
