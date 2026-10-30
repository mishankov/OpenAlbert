package telemetry

import (
	"crypto/rand"
	"log/slog"
	"net/http"
	"strings"
)

// HTTP emits one event per API request, including its SQL summary. Health checks
// and static assets are excluded. Only route templates are logged, never URLs,
// query parameters, headers or bodies.
func HTTP(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		e := Start(r.Context(), logger, "http.request")
		e.Set("request_id", rand.Text())
		e.Set("http_method", r.Method)
		r = r.WithContext(e.Context())
		response := &httpResponse{ResponseWriter: w}
		completed := false
		defer func() {
			status := response.status
			if !completed {
				status = http.StatusInternalServerError
			} else if status == 0 {
				status = http.StatusOK
			}
			e.Set("http_route", r.Pattern)
			e.Set("http_status", status)
			var err error
			if status >= 400 {
				err = httpStatusError(status)
			} else {
				err = r.Context().Err()
			}
			e.End(err)
		}()
		next.ServeHTTP(response, r)
		completed = true
	})
}

type httpStatusError int

func (e httpStatusError) Error() string   { return http.StatusText(int(e)) }
func (e httpStatusError) HTTPStatus() int { return int(e) }

type httpResponse struct {
	http.ResponseWriter
	status int
}

func (w *httpResponse) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *httpResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *httpResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
