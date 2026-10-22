package inspector

import (
	"context"
	"io"
	"io/fs"
	"jarvis/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore struct{}

type audioArtifactStore struct{ fakeStore }

func (audioArtifactStore) Artifact(_ context.Context, scope, id string) (domain.Artifact, bool, error) {
	if scope != "personal:7" || id != "audio" {
		return domain.Artifact{}, false, nil
	}
	return domain.Artifact{MIME: "audio/ogg", Data: []byte("ogg")}, true, nil
}
func TestInspectorAudioUsesSafeMIMEAndSameOriginPolicy(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, fakeStore{}, audioArtifactStore{})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/artifact?conversation_id=personal:7&id=audio", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "audio/ogg" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "media-src 'self'") {
		t.Fatal("audio playback policy missing")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/artifact?conversation_id=other&id=audio", nil))
	if w.Code != 404 {
		t.Fatal("audio escaped scope")
	}
}

func (fakeStore) InspectSessions(context.Context, Filter) ([]Session, error) { return []Session{}, nil }
func (fakeStore) InspectSession(context.Context, string, int64) (Detail, bool, error) {
	return Detail{}, false, nil
}
func (fakeStore) InspectTrace(context.Context, string, string) (domain.Trace, bool, error) {
	return domain.Trace{}, false, nil
}
func (fakeStore) Artifact(context.Context, string, string) (domain.Artifact, bool, error) {
	return domain.Artifact{}, false, nil
}
func (fakeStore) ListFiles(context.Context, string, string) (domain.StoredFiles, error) {
	return domain.StoredFiles{Bucket: "jarvis", Files: []domain.StoredFile{}}, nil
}
func (fakeStore) OpenFile(context.Context, string) (domain.StoredFile, io.ReadSeekCloser, error) {
	return domain.StoredFile{}, nil, fs.ErrNotExist
}
func TestLocalReadOnlyInspector(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, fakeStore{}, fakeStore{})
	for _, tc := range []struct {
		method, path, host, origin string
		code                       int
	}{
		{"GET", "/inspect/", "localhost:8080", "", 200},
		{"GET", "/api/inspect/sessions", "127.0.0.1:8080", "", 200},
		{"GET", "/api/inspect/sessions/missing", "localhost:8080", "", 404},
		{"POST", "/api/inspect/sessions", "localhost:8080", "", 405},
		{"GET", "/api/inspect/sessions", "attacker.example:8080", "", 403},
		{"GET", "/api/inspect/sessions", "localhost:8080", "https://attacker.example", 403},
		{"GET", "/api/inspect/sessions", "localhost:8080", "http://localhost:8080", 200},
		{"GET", "/api/inspect/files", "localhost:8080", "", 200},
		{"GET", "/api/inspect/file?key=missing", "localhost:8080", "", 404},
		{"GET", "/api/inspect/file", "localhost:8080", "", 400},
		{"GET", "/api/inspect/file/content?key=missing", "attacker.example", "", 403},
		{"GET", "/api/inspect/files", "localhost:8080", "https://attacker.example", 403},
		{"DELETE", "/api/inspect/file?key=notes.txt", "localhost:8080", "", 405},
	} {
		t.Run(tc.method+tc.path+tc.host+tc.origin, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("%d: %s", rec.Code, rec.Body.String())
			}
			if rec.Code == 200 && !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("private inspector was cacheable")
			}
		})
	}
}
