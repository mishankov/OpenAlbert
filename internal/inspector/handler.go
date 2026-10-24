// Package inspector serves a read-only view of persisted agent activity.
package inspector

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"openalbert/internal/domain"
)

//go:embed web/*
var assets embed.FS

type ArtifactStore interface {
	Artifact(context.Context, string, string) (domain.Artifact, bool, error)
	ListFiles(context.Context, string, string) (domain.StoredFiles, error)
	OpenFile(context.Context, string) (domain.StoredFile, io.ReadSeekCloser, error)
}

func Register(mux *http.ServeMux, store Store, artifacts ArtifactStore) {
	registerFiles(mux, artifacts)
	files, _ := fs.Sub(assets, "web")
	mux.Handle("GET /inspect/", inspectorResponse(http.StripPrefix("/inspect/", http.FileServer(http.FS(files)))))
	mux.Handle("GET /api/inspect/sessions", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := Filter{Query: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status"), Trigger: r.URL.Query().Get("trigger"), Conversation: r.URL.Query().Get("conversation_id"), Before: number(r, "before"), Limit: 51}
		rows, err := store.InspectSessions(r.Context(), f)
		if err != nil {
			fail(w, 500)
			return
		}
		var next int64
		if len(rows) > 50 {
			rows = rows[:50]
			next = rows[49].Seq
		}
		write(w, map[string]any{"sessions": rows, "next_before": next})
	})))
	mux.Handle("GET /api/inspect/sessions/{id}", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, found, err := store.InspectSession(r.Context(), r.PathValue("id"), number(r, "after"))
		if err != nil {
			fail(w, 500)
			return
		}
		if !found {
			fail(w, 404)
			return
		}
		write(w, out)
	})))
	mux.Handle("GET /api/inspect/trace", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, found, err := store.InspectTrace(r.Context(), r.URL.Query().Get("session_id"), r.URL.Query().Get("id"))
		if err != nil {
			fail(w, 500)
			return
		}
		if !found {
			fail(w, 404)
			return
		}
		write(w, out)
	})))
	mux.Handle("GET /api/inspect/artifact", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, found, err := artifacts.Artifact(r.Context(), r.URL.Query().Get("conversation_id"), r.URL.Query().Get("id"))
		if err != nil {
			fail(w, 500)
			return
		}
		if !found {
			fail(w, 404)
			return
		}
		mime := "application/octet-stream"
		if out.MIME == "image/png" || out.MIME == "image/jpeg" || out.MIME == "image/webp" {
			mime = out.MIME
		}
		if out.MIME == "audio/ogg" || out.MIME == "audio/mpeg" || out.MIME == "audio/wav" || out.MIME == "audio/mp4" || out.MIME == "audio/webm" {
			mime = out.MIME
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Disposition", "inline")
		_, _ = w.Write(out.Data)
	})))
}
func number(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
func fail(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(code)})
}
func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func inspectorResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
