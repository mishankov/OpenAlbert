// Package logpipe normalizes Docker Splunk events before they enter VictoriaLogs.
// It acknowledges a batch only after VictoriaLogs accepts the entire request.
package logpipe

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const MaxBatchBytes = 64 << 20

type Handler struct {
	Target           string
	Client           *http.Client
	accepted, failed atomic.Uint64
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","accepted_batches":%d,"failed_batches":%d}`, h.accepted.Load(), h.failed.Load())
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/services/collector/event" && r.URL.Path != "/services/collector/event/1.0" && r.URL.Path != "/insert/splunk/services/collector/event") {
		http.NotFound(w, r)
		return
	}
	var source io.Reader = r.Body
	defer r.Body.Close()
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(source)
		if err != nil {
			http.Error(w, "invalid compressed batch", http.StatusBadRequest)
			return
		}
		defer reader.Close()
		source = reader
	} else if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		http.Error(w, "unsupported content encoding", http.StatusUnsupportedMediaType)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(source, MaxBatchBytes+1))
	if err != nil || len(raw) > MaxBatchBytes {
		http.Error(w, "invalid or oversized batch", http.StatusRequestEntityTooLarge)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	count := 0
	for {
		var envelope map[string]any
		if err := decoder.Decode(&envelope); err == io.EOF {
			break
		} else if err != nil {
			http.Error(w, "invalid event batch", http.StatusBadRequest)
			return
		}
		row, err := Normalize(envelope)
		if err != nil {
			http.Error(w, "invalid Docker event", http.StatusBadRequest)
			return
		}
		if err = encoder.Encode(row); err != nil {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		count++
	}
	if count == 0 {
		http.Error(w, "empty batch", http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.Target, &payload)
	if err != nil {
		h.failed.Add(1)
		http.Error(w, "log destination unavailable", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/stream+json")
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		h.failed.Add(1)
		http.Error(w, "log destination unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.failed.Add(1)
		http.Error(w, "log destination rejected batch", http.StatusServiceUnavailable)
		return
	}
	h.accepted.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"text":"Success","code":0}`))
}

var rawLevel = regexp.MustCompile(`(?i)\b(ERROR|WARNING|WARN|FATAL|PANIC|DEBUG|INFO|NOTICE|LOG)\b`)

// Normalize gives container metadata priority over fields printed by an application.
// No environment variables, request headers or original Docker envelope are retained.
func Normalize(envelope map[string]any) (map[string]any, error) {
	event, ok := envelope["event"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing Docker event")
	}
	attrs, _ := event["attrs"].(map[string]any)
	service, _ := attrs["compose_service"].(string)
	// Some Docker events contain JSON as a string instead of an object.
	if service == "temporal-ui" {
		if line, ok := event["line"].(string); ok {
			var decoded map[string]any
			d := json.NewDecoder(strings.NewReader(line))
			d.UseNumber()
			if d.Decode(&decoded) == nil && decoded != nil {
				event["line"] = decoded
			}
		}
	}
	row := map[string]any{}
	message := ""
	inferRawLevel := true
	switch line := event["line"].(type) {
	case map[string]any:
		for k, v := range line {
			if !strings.HasPrefix(k, "_") {
				row[k] = v
			}
		}
		message, _ = line["msg"].(string)
		if message == "" {
			message, _ = line["message"].(string)
		}
		if message == "" {
			b, _ := json.Marshal(line)
			message = string(b)
			// JSON property names such as "error" do not express severity.
			inferRawLevel = false
		}
	case string:
		message = line
	default:
		return nil, fmt.Errorf("missing Docker line")
	}
	if service == "" {
		service = "unknown"
	}
	if service == "temporal-ui" {
		if normalized, text, ok := temporalHTTP(row); ok {
			row, message = normalized, text
		}
	}
	container, _ := event["tag"].(string)
	project, _ := attrs["com.docker.compose.project"].(string)
	level, _ := row["level"].(string)
	if level == "" {
		level, _ = row["severity"].(string)
	}
	if level == "" && inferRawLevel {
		level = rawLevel.FindString(message)
	}
	level = strings.ToLower(level)
	switch level {
	case "warning":
		level = "warn"
	case "fatal", "panic":
		level = "fatal"
	case "trace", "debug", "info", "warn", "error":
	case "notice", "log", "":
		level = "info"
	default:
		level = "info"
	}
	delete(row, "msg")
	delete(row, "message")
	delete(row, "time")
	delete(row, "ts")
	row["service"], row["container"], row["project"], row["level"] = service, container, project, level
	row["host"] = envelope["host"]
	row["stream"] = event["source"]
	row["_msg"] = "[" + service + "] " + message
	// Docker supplies epoch seconds. Preserve fractional precision instead of using ingestion time.
	if n, ok := envelope["time"].(json.Number); ok {
		parts := strings.SplitN(n.String(), ".", 2)
		seconds, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, err
		}
		var nanos int64
		if len(parts) == 2 {
			fraction := (parts[1] + "000000000")[:9]
			nanos, err = strconv.ParseInt(fraction, 10, 64)
			if err != nil {
				return nil, err
			}
		}
		row["_time"] = time.Unix(seconds, nanos).UTC().Format(time.RFC3339Nano)
	} else if stamp, ok := envelope["time"].(string); ok {
		row["_time"] = stamp
	} else {
		return nil, fmt.Errorf("missing timestamp")
	}
	return row, nil
}

// Temporal UI's access logger has no message field. Normalize its HTTP events
// without copying query strings, headers, user agents or arbitrary error bodies.
// All requests are retained; filtering happens at query time.
func temporalHTTP(fields map[string]any) (map[string]any, string, bool) {
	method, _ := fields["method"].(string)
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
	default:
		return nil, "", false
	}
	number := func(key string) (float64, bool) {
		switch v := fields[key].(type) {
		case json.Number:
			n, err := v.Float64()
			return n, err == nil
		case float64:
			return v, true
		default:
			return 0, false
		}
	}
	status, ok := number("status")
	if !ok || status < 100 || status > 599 {
		return nil, "", false
	}
	path := ""
	if uri, ok := fields["uri"].(string); ok {
		if u, err := url.Parse(uri); err == nil {
			path = u.EscapedPath()
		}
	}
	level := "info"
	if supplied, _ := fields["level"].(string); supplied != "" {
		switch strings.ToLower(supplied) {
		case "warn", "warning":
			level = "warn"
		case "error", "fatal", "panic":
			level = "error"
		}
	}
	errorText, _ := fields["error"].(string)
	if status >= 500 || errorText != "" {
		level = "error"
	} else if status >= 400 {
		level = "warn"
	}
	row := map[string]any{"operation": "http.request", "http_method": method, "http_path": path, "http_status": int(status), "level": level, "http_error": errorText != ""}
	text := fmt.Sprintf("%s %s · HTTP %d", method, path, int(status))
	if latency, ok := number("latency"); ok && latency >= 0 {
		row["duration_ms"] = latency / float64(time.Millisecond)
		text += fmt.Sprintf(" · %.1f мс", row["duration_ms"])
	}
	return row, text, true
}
