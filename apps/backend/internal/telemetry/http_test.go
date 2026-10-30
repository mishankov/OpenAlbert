package telemetry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPWideEventSQLAndRouteRedaction(t *testing.T) {
	for _, status := range []int{200, 404, 500} {
		var out bytes.Buffer
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/inspect/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
			RecordSQL(r.Context(), "SELECT data FROM sessions WHERE id=$1", 120*time.Millisecond, 1, status == 500)
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(status)
			_, _ = w.Write([]byte("PRIVATE_RESPONSE"))
		})
		r := httptest.NewRequest("GET", "/api/inspect/sessions/PRIVATE_ID?q=PRIVATE_QUERY", strings.NewReader("PRIVATE_BODY"))
		r.Header.Set("Authorization", "PRIVATE_TOKEN")
		HTTP(New(&out), mux).ServeHTTP(httptest.NewRecorder(), r)
		var row map[string]any
		if err := json.Unmarshal(out.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row["http_status"] != float64(status) || row["http_route"] != "GET /api/inspect/sessions/{id}" || row["db_query_count"] != float64(1) || row["request_id"] == "" {
			t.Fatalf("incomplete request event: %v", row)
		}
		if (status >= 400) != (row["result"] == "error") || strings.Contains(out.String(), "PRIVATE") || strings.Count(strings.TrimSpace(out.String()), "\n") != 0 {
			t.Fatalf("wrong outcome or request data leaked: %s", out.String())
		}
	}
}

func TestHTTPWideEventSkipsAssetsAndPreservesResponseController(t *testing.T) {
	var out bytes.Buffer
	handler := HTTP(New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatal(err)
		}
	}))
	for _, path := range []string{"/healthz", "/inspect/app.js", "/api/test"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Body.String() != "body" || !w.Flushed {
			t.Fatalf("response behavior changed for %s", path)
		}
	}
	if strings.Count(strings.TrimSpace(out.String()), "\n") != 0 || !strings.Contains(out.String(), `"http_status":200`) {
		t.Fatalf("assets produced events or implicit status lost: %s", out.String())
	}
}

func TestHTTPPanicDoesNotBecomeSuccessfulEvent(t *testing.T) {
	var out bytes.Buffer
	handler := HTTP(New(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("PRIVATE_PANIC")
	}))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("middleware swallowed panic")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/test", nil))
	}()
	if !strings.Contains(out.String(), `"http_status":500`) || !strings.Contains(out.String(), `"result":"error"`) || strings.Contains(out.String(), "PRIVATE") {
		t.Fatalf("panic logged as success or panic body leaked: %s", out.String())
	}
}
