package inspector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"jarvis/internal/domain"
)

type fileStore struct {
	fakeStore
	file domain.StoredFile
	data string
	err  error
}
type fileReader struct{ *strings.Reader }

func (fileReader) Close() error { return nil }
func (s fileStore) OpenFile(context.Context, string) (domain.StoredFile, io.ReadSeekCloser, error) {
	return s.file, fileReader{strings.NewReader(s.data)}, s.err
}
func (s fileStore) ListFiles(_ context.Context, prefix, after string) (domain.StoredFiles, error) {
	if prefix != "a/" || after != "a/1" {
		return domain.StoredFiles{}, errors.New("wrong pagination")
	}
	return domain.StoredFiles{Bucket: "jarvis", Files: []domain.StoredFile{s.file}, NextAfter: "a/2"}, s.err
}
func fileRequest(store fileStore, target string, headers map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	Register(mux, fakeStore{}, store)
	req := httptest.NewRequest("GET", "http://localhost:8080"+target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
func TestFileListPagination(t *testing.T) {
	rec := fileRequest(fileStore{file: domain.StoredFile{Key: "a/2"}}, "/api/inspect/files?prefix=a%2F&after=a%2F1", nil)
	var out domain.StoredFiles
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.NextAfter != "a/2" || len(out.Files) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = fileRequest(fileStore{err: fs.ErrInvalid}, "/api/inspect/files?prefix=a%2F&after=a%2F1", nil)
	if rec.Code != 400 {
		t.Fatalf("invalid cursor: %d %s", rec.Code, rec.Body)
	}
}
func TestFilePreviewAndDownload(t *testing.T) {
	for _, tc := range []struct{ name, mime, data, kind string }{
		{"text", "text/plain; charset=utf-8", "Заметки <script>alert(1)</script>", "text"},
		{"json", "application/problem+json", `{"a":1}`, "text"},
		{"image", "image/png", "PNG", "image"},
		{"svg", "image/svg+xml", "<svg/>", "text"},
		{"binary", "application/pdf", "PDF", "unsupported"},
		{"invalid UTF8", "text/plain", "\xff", "unsupported"},
		{"binary as text", "text/plain", "a\x00b", "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fileStore{file: domain.StoredFile{Key: "a/key", Name: "Заметки.txt", MIME: tc.mime, Size: int64(len(tc.data))}, data: tc.data}
			rec := fileRequest(s, "/api/inspect/file?key=a%2Fkey", nil)
			var out struct {
				Kind string `json:"preview_kind"`
				Text string `json:"text"`
			}
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Kind != tc.kind {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if tc.kind == "text" && out.Text != tc.data {
				t.Fatal("text changed")
			}
			rec = fileRequest(s, "/api/inspect/file/content?key=a%2Fkey&download=1", nil)
			disposition, params, _ := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
			if rec.Code != 200 || rec.Body.String() != tc.data || disposition != "attachment" || params["filename"] != s.file.Name {
				t.Fatalf("download: %d %s %v", rec.Code, rec.Body, rec.Header())
			}
			rec = fileRequest(s, "/api/inspect/file/content?key=a%2Fkey", map[string]string{"Range": "bytes=0-0"})
			if rec.Code != 206 || rec.Body.String() != tc.data[:1] {
				t.Fatalf("range: %d %s", rec.Code, rec.Body)
			}
			if tc.kind != "image" && !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
				t.Fatal("active file served inline")
			}
		})
	}
}
func TestTextPreviewBoundedAndErrors(t *testing.T) {
	data := strings.Repeat("я", maxTextPreview) // The limit may split a multibyte character.
	s := fileStore{file: domain.StoredFile{MIME: "text/plain"}, data: "a" + data}
	rec := fileRequest(s, "/api/inspect/file?key="+url.QueryEscape("a/+ #?.txt"), nil)
	var out struct {
		Text      string
		Truncated bool
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil || !out.Truncated || !utf8.ValidString(out.Text) || len(out.Text) > maxTextPreview || !strings.HasPrefix(s.data, out.Text) {
		t.Fatalf("invalid preview: %d", len(out.Text))
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{fs.ErrNotExist, 404}, {errors.New("secret endpoint failure"), 500}} {
		s.err = tc.err
		rec = fileRequest(s, "/api/inspect/file?key=x", nil)
		if rec.Code != tc.code || strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
}
