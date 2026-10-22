package inspector

import (
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

const maxTextPreview = 256 << 10

func imageMIME(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif":
		return true
	}
	return false
}

func textMIME(value string) bool {
	return strings.HasPrefix(value, "text/") || value == "application/json" || value == "application/xml" || value == "application/yaml" || value == "application/x-yaml" || strings.HasSuffix(value, "+json") || strings.HasSuffix(value, "+xml")
}

func registerFiles(mux *http.ServeMux, store ArtifactStore) {
	mux.Handle("GET /api/inspect/files", local(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, err := store.ListFiles(r.Context(), r.URL.Query().Get("prefix"), r.URL.Query().Get("after"))
		if err != nil {
			fail(w, 500)
			return
		}
		write(w, out)
	})))
	serve := func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		if key == "" {
			fail(w, 400)
			return
		}
		file, object, err := store.OpenFile(r.Context(), key)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fail(w, 404)
			} else {
				fail(w, 500)
			}
			return
		}
		defer object.Close()
		mediaType, _, _ := mime.ParseMediaType(file.MIME)
		if strings.HasSuffix(r.URL.Path, "/content") {
			disposition := "attachment"
			contentType := "application/octet-stream"
			if imageMIME(mediaType) && r.URL.Query().Get("download") != "1" {
				disposition, contentType = "inline", mediaType
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": path.Base(strings.ReplaceAll(file.Name, "\\", "/"))}))
			http.ServeContent(w, r, file.Name, file.ModifiedAt, object)
			return
		}
		out := map[string]any{"file": file, "preview_kind": "unsupported"}
		if imageMIME(mediaType) {
			out["preview_kind"] = "image"
		} else if textMIME(mediaType) {
			data, err := io.ReadAll(io.LimitReader(object, maxTextPreview+1))
			if err != nil {
				fail(w, 500)
				return
			}
			truncated := len(data) > maxTextPreview
			if truncated {
				data = data[:maxTextPreview]
				// A byte limit can split the last UTF-8 rune.
				for n := 0; n < utf8.UTFMax-1 && !utf8.Valid(data); n++ {
					data = data[:len(data)-1]
				}
			}
			if utf8.Valid(data) && !strings.ContainsRune(string(data), 0) {
				out["preview_kind"], out["text"], out["truncated"] = "text", string(data), truncated
			}
		}
		write(w, out)
	}
	mux.Handle("GET /api/inspect/file", local(http.HandlerFunc(serve)))
	mux.Handle("GET /api/inspect/file/content", local(http.HandlerFunc(serve)))
}
