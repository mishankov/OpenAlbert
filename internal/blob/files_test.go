package blob

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/storage"
)

func xmlText(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	if !q.Has("metadata") || q.Get("max-keys") != "1000" || q.Get("delimiter") != "" || q.Get("start-after") != "" {
		http.Error(w, "invalid list options", 400)
		return
	}
	keys := []string{}
	for objectPath := range f.objects {
		key := strings.TrimPrefix(objectPath, "/jarvis/")
		if strings.HasPrefix(key, q.Get("prefix")) && key > q.Get("continuation-token") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit, _ := strconv.Atoi(q.Get("max-keys"))
	truncated := len(keys) > limit
	if truncated {
		keys = keys[:limit]
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<ListBucketResult><Name>jarvis</Name><IsTruncated>%t</IsTruncated>`, truncated)
	if truncated {
		fmt.Fprintf(w, `<NextContinuationToken>%s</NextContinuationToken>`, xmlText(keys[len(keys)-1]))
	}
	for _, key := range keys {
		obj := f.objects["/jarvis/"+key]
		modified := obj.headers.Get("Last-Modified")
		if modified == "" {
			modified = "2026-10-22T12:00:00Z"
		}
		fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified><UserMetadata><content-type>%s</content-type>`, xmlText(key), len(obj.data), modified, xmlText(obj.headers.Get("Content-Type")))
		for name, values := range obj.headers {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
				fmt.Fprintf(w, "<%s>%s</%s>", name, xmlText(values[0]), name)
			}
		}
		fmt.Fprint(w, `</UserMetadata></Contents>`)
	}
	fmt.Fprint(w, `</ListBucketResult>`)
}

func TestListAndOpenBucketFiles(t *testing.T) {
	ctx := context.Background()
	s, f := setup(t, storage.NewMemory())
	art := domain.Artifact{ID: "notes", ConversationID: "c", Name: "Заметки.txt", MIME: "text/plain", Data: []byte("hello")}
	if err := s.SaveArtifact(ctx, art); err != nil {
		t.Fatal(err)
	}
	// Objects outside the app's artifact namespace are also browsable.
	base := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for i := range 1105 {
		f.objects[fmt.Sprintf("/jarvis/uploads/%04d +#?.txt", i)] = fakeObject{data: []byte("raw"), headers: http.Header{"Content-Type": {"text/plain"}, "Last-Modified": {base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)}}}
	}
	page, err := s.ListFiles(ctx, "artifacts/", "")
	if err != nil || len(page.Files) != 1 || page.Files[0].Name != art.Name || page.Files[0].ConversationID != art.ConversationID || page.Files[0].MIME != art.MIME {
		t.Fatalf("artifact names: %#v %v", page, err)
	}
	page, err = s.ListFiles(ctx, "uploads/", "")
	if err != nil || len(page.Files) != 100 || page.NextAfter == "" || page.Files[0].Key != "uploads/1104 +#?.txt" || page.Files[99].Key != "uploads/1005 +#?.txt" {
		t.Fatalf("first page: %#v %v", page, err)
	}
	last := page.NextAfter
	page, err = s.ListFiles(ctx, "uploads/", last)
	if err != nil || len(page.Files) != 100 || page.NextAfter == "" || page.Files[0].Key != "uploads/1004 +#?.txt" {
		t.Fatalf("second page: %#v %v", page, err)
	}
	info, reader, err := s.OpenFile(ctx, page.Files[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "raw" || info.Size != 3 || info.MIME != "text/plain" {
		t.Fatalf("raw file: %#v %s %v", info, data, err)
	}
	info, reader, err = s.OpenFile(ctx, key(art.ConversationID, art.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if info.Name != art.Name || info.ArtifactID != art.ID {
		t.Fatalf("artifact stat: %#v", info)
	}
	if _, _, err := s.OpenFile(ctx, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	page, err = s.ListFiles(ctx, "nothing/", "")
	if err != nil || page.Files == nil || len(page.Files) != 0 {
		t.Fatalf("empty: %#v %v", page, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListFiles(cancelled, "", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestFileDatePaginationTiesAndDeletedCursor(t *testing.T) {
	ctx := context.Background()
	s, f := setup(t, storage.NewMemory())
	for i := range 205 {
		f.objects[fmt.Sprintf("/jarvis/ties/%03d", i)] = fakeObject{headers: http.Header{}}
	}
	var after string
	for pageNo, count := range []int{100, 100, 5} {
		page, err := s.ListFiles(ctx, "ties/", after)
		if err != nil || len(page.Files) != count {
			t.Fatalf("page %d: %d %v", pageNo, len(page.Files), err)
		}
		for i, file := range page.Files {
			if file.Key != fmt.Sprintf("ties/%03d", pageNo*100+i) {
				t.Fatalf("tie order: %s", file.Key)
			}
		}
		if pageNo < 2 && page.NextAfter == "" {
			t.Fatal("missing continuation")
		}
		if pageNo == 2 && page.NextAfter != "" {
			t.Fatal("unexpected continuation")
		}
		after = page.NextAfter
		// The following page must not require the previous object to exist.
		f.mu.Lock()
		delete(f.objects, "/jarvis/"+page.Files[len(page.Files)-1].Key)
		f.mu.Unlock()
	}
	for _, invalid := range []string{"old-key", strings.Repeat("x", 4097), "e30"} {
		if _, err := s.ListFiles(ctx, "", invalid); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("invalid cursor accepted: %v", err)
		}
	}
}
