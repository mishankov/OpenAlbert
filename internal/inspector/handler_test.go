package inspector

import (
	"context"
	"jarvis/internal/domain"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore struct{}

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
