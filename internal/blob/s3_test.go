package blob

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"openalbert/internal/domain"
	"openalbert/internal/storage"
)

type fakeObject struct {
	data    []byte
	headers http.Header
}
type fakeS3 struct {
	mu                 sync.Mutex
	objects            map[string]fakeObject
	conditionalCreates int
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("location") {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
		return
	}
	if r.URL.Path == "/openalbert" || r.URL.Path == "/openalbert/" {
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if strings.Contains(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING") {
			body, err = decodeChunks(body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		if r.Header.Get("If-None-Match") == "*" {
			f.conditionalCreates++
			if _, exists := f.objects[r.URL.Path]; exists {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code><Message>Already exists</Message></Error>`)
				return
			}
		}
		f.objects[r.URL.Path] = fakeObject{data: body, headers: r.Header.Clone()}
		sum := md5.Sum(body)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
		return
	}
	obj, ok := f.objects[r.URL.Path]
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
		return
	}
	for k, values := range obj.headers {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
			w.Header()[k] = values
		}
	}
	sum := md5.Sum(obj.data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", obj.headers.Get("Content-Type"))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(obj.data)
}
func decodeChunks(data []byte) ([]byte, error) {
	r := bufio.NewReader(bytes.NewReader(data))
	var out bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		n, err := strconv.ParseInt(size, 16, 64)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out.Bytes(), nil
		}
		if _, err = io.CopyN(&out, r, n); err != nil {
			return nil, err
		}
		if _, err = r.ReadString('\n'); err != nil {
			return nil, err
		}
	}
}
func setup(t *testing.T, repo domain.Repository) (*Store, *fakeS3) {
	t.Helper()
	f := &fakeS3{objects: map[string]fakeObject{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	s, err := New(context.Background(), repo, strings.TrimPrefix(server.URL, "http://"), "test-access", "test-secret", "openalbert", false)
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}
func TestArtifactsImmutableScopedAndConcurrent(t *testing.T) {
	ctx := context.Background()
	s, f := setup(t, storage.NewMemory())
	original := domain.Artifact{ID: "notes", ConversationID: "a/b", Name: "Заметки.txt", MIME: "text/plain", Data: []byte("first bytes")}
	if err := s.SaveArtifact(ctx, original); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			retry := original
			retry.Name = "replacement"
			retry.Data = []byte(fmt.Sprint(i))
			if err := s.SaveArtifact(ctx, retry); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got, ok, err := s.Artifact(ctx, original.ConversationID, original.ID)
	if err != nil || !ok || got.Name != original.Name || string(got.Data) != "first bytes" {
		t.Fatalf("immutable artifact %#v %v %v", got, ok, err)
	}
	different := original
	different.ConversationID = "a_b"
	different.Data = []byte("different scope")
	if err = s.SaveArtifact(ctx, different); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.Artifact(ctx, different.ConversationID, different.ID)
	if err != nil || !ok || string(got.Data) != "different scope" {
		t.Fatalf("scope collision %#v %v %v", got, ok, err)
	}
	if _, ok, err = s.Artifact(ctx, "another", original.ID); err != nil || ok {
		t.Fatal("artifact leaked across conversations")
	}
	f.mu.Lock()
	count := len(f.objects)
	conditional := f.conditionalCreates
	f.mu.Unlock()
	if count != 2 || conditional != 2 {
		t.Fatalf("unexpected writes objects=%d conditional=%d", count, conditional)
	}
}

type failingRepository struct {
	domain.Repository
	mu       sync.Mutex
	failNext bool
}

func (r *failingRepository) SaveArtifact(ctx context.Context, a domain.Artifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return errors.New("simulated database commit failure")
	}
	return r.Repository.SaveArtifact(ctx, a)
}
func TestReplayAfterObjectCommittedBeforeMetadata(t *testing.T) {
	ctx := context.Background()
	repo := &failingRepository{Repository: storage.NewMemory(), failNext: true}
	s, f := setup(t, repo)
	original := domain.Artifact{ID: "same", ConversationID: "scope", Name: "first.txt", MIME: "text/plain", Data: []byte("committed object"), CreatedAt: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	if err := s.SaveArtifact(ctx, original); err == nil {
		t.Fatal("database failure was hidden")
	}
	if _, ok, _ := repo.Artifact(ctx, "scope", "same"); ok {
		t.Fatal("failed database committed metadata")
	}
	retry := original
	retry.Name = "second.txt"
	retry.MIME = "application/json"
	retry.Data = []byte("replacement")
	retry.CreatedAt = time.Now()
	if err := s.SaveArtifact(ctx, retry); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Artifact(ctx, "scope", "same")
	if err != nil || !ok || got.Name != original.Name || got.MIME != original.MIME || !got.CreatedAt.Equal(original.CreatedAt) || string(got.Data) != string(original.Data) {
		t.Fatalf("replay lost original %#v %v %v", got, ok, err)
	}
	f.mu.Lock()
	conditional := f.conditionalCreates
	f.mu.Unlock()
	if conditional != 2 {
		t.Fatalf("conditional creates=%d", conditional)
	}
}

func TestObjectIntegrity(t *testing.T) {
	ctx := context.Background()
	s, f := setup(t, storage.NewMemory())
	a := domain.Artifact{ID: "one", ConversationID: "scope", Name: "one", Data: []byte("original")}
	if err := s.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	path := "/openalbert/" + key(a.ConversationID, a.ID)
	obj := f.objects[path]
	obj.data = []byte("changed")
	f.objects[path] = obj
	f.mu.Unlock()
	if _, _, err := s.Artifact(ctx, a.ConversationID, a.ID); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("changed S3 object accepted: %v", err)
	}
}
func TestObjectKeysNeverCollideBySanitization(t *testing.T) {
	for _, pair := range [][2]string{{"a/b", "a_b"}, {"a\\b", "a_b"}, {"../a", "__a"}, {"a/b", "a\\b"}} {
		if key(pair[0], "id") == key(pair[1], "id") {
			t.Fatalf("scope keys collide: %v", pair)
		}
		if key("scope", pair[0]) == key("scope", pair[1]) {
			t.Fatalf("ID keys collide: %v", pair)
		}
	}
}
func TestRejectArtifactBeforeObjectWrite(t *testing.T) {
	s, f := setup(t, storage.NewMemory())
	for _, a := range []domain.Artifact{{ID: "one"}, {ConversationID: "scope"}, {ID: "one", ConversationID: "scope", Data: make([]byte, maxArtifactSize+1)}} {
		if err := s.SaveArtifact(context.Background(), a); err == nil {
			t.Fatal("invalid artifact accepted")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.objects) > 0 {
		t.Fatal("invalid artifact uploaded")
	}
}
func TestRealMinIO(t *testing.T) {
	endpoint := os.Getenv("OPENALBERT_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OPENALBERT_TEST_S3_ENDPOINT, OPENALBERT_TEST_S3_ACCESS_KEY, OPENALBERT_TEST_S3_SECRET_KEY and OPENALBERT_TEST_S3_BUCKET")
	}
	ctx := context.Background()
	repo := &failingRepository{Repository: storage.NewMemory(), failNext: true}
	open := func() *Store {
		s, err := New(ctx, repo, endpoint, os.Getenv("OPENALBERT_TEST_S3_ACCESS_KEY"), os.Getenv("OPENALBERT_TEST_S3_SECRET_KEY"), os.Getenv("OPENALBERT_TEST_S3_BUCKET"), false)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	scope := fmt.Sprintf("blob-test-%d", time.Now().UnixNano())
	a := domain.Artifact{ID: "one", ConversationID: scope, Name: "notes.txt", MIME: "text/plain", Data: []byte("first")}
	t.Cleanup(func() {
		_ = s.client.RemoveObject(context.Background(), s.bucket, key(scope, a.ID), minio.RemoveObjectOptions{})
	})
	if err := s.SaveArtifact(ctx, a); err == nil {
		t.Fatal("expected simulated DB commit failure")
	}
	// New process, same durable S3 body and repository without committed metadata.
	s = open()
	a.Data = []byte("second")
	a.Name = "replacement.txt"
	if err := s.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Artifact(ctx, scope, a.ID)
	if err != nil || !ok || string(got.Data) != "first" || got.Name != "notes.txt" {
		t.Fatalf("MinIO artifact %#v %v %v", got, ok, err)
	}
}
