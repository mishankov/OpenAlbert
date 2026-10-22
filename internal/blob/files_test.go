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
	if !q.Has("metadata") || q.Get("max-keys") != "101" || q.Get("delimiter") != "" {
		http.Error(w, "invalid list options", 400)
		return
	}
	keys := []string{}
	for objectPath := range f.objects {
		key := strings.TrimPrefix(objectPath, "/jarvis/")
		if strings.HasPrefix(key, q.Get("prefix")) && key > q.Get("start-after") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit, _ := strconv.Atoi(q.Get("max-keys"))
	if len(keys) > limit {
		keys = keys[:limit]
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, `<ListBucketResult><Name>jarvis</Name><IsTruncated>false</IsTruncated>`)
	for _, key := range keys {
		obj := f.objects["/jarvis/"+key]
		fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>%s</LastModified><UserMetadata><content-type>%s</content-type>`, xmlText(key), len(obj.data), time.Now().UTC().Format(time.RFC3339), xmlText(obj.headers.Get("Content-Type")))
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
	for i := range 105 {
		f.objects[fmt.Sprintf("/jarvis/uploads/%03d +#?.txt", i)] = fakeObject{data: []byte("raw"), headers: http.Header{"Content-Type": {"text/plain"}}}
	}
	page, err := s.ListFiles(ctx, "artifacts/", "")
	if err != nil || len(page.Files) != 1 || page.Files[0].Name != art.Name || page.Files[0].ConversationID != art.ConversationID || page.Files[0].MIME != art.MIME {
		t.Fatalf("artifact names: %#v %v", page, err)
	}
	page, err = s.ListFiles(ctx, "uploads/", "")
	if err != nil || len(page.Files) != 100 || page.NextAfter != page.Files[99].Key {
		t.Fatalf("first page: %#v %v", page, err)
	}
	last := page.NextAfter
	page, err = s.ListFiles(ctx, "uploads/", last)
	if err != nil || len(page.Files) != 5 || page.NextAfter != "" || page.Files[0].Key <= last {
		t.Fatalf("last page: %#v %v", page, err)
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
