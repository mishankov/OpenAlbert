package logpipe

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fixture = `{"time":1792479900.123456789,"host":"docker-desktop","event":{"tag":"openalbert-openalbert-1","source":"stdout","attrs":{"compose_service":"openalbert","com.docker.compose.project":"example"},"line":{"time":"ignored","level":"INFO","msg":"Обращение обработано","operation":"turn.complete","turn_id":"t-1","service":"forged","_stream":"forged","input_tokens":42}}}`

func TestNormalizePreservesFieldsAndUsesTrustedContainerIdentity(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(fixture))
	decoder.UseNumber()
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	row, err := Normalize(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if row["service"] != "openalbert" || row["level"] != "info" || row["turn_id"] != "t-1" || row["container"] != "openalbert-openalbert-1" || row["project"] != "example" {
		t.Fatalf("bad normalized fields: %v", row)
	}
	if row["_msg"] != "[openalbert] Обращение обработано" || row["_time"] != time.Unix(1792479900, 123456789).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("bad presentation/time: %v", row)
	}
	if _, ok := row["_stream"]; ok {
		t.Fatal("application overrode reserved stream")
	}
	for key := range row {
		if strings.HasPrefix(key, "event.") {
			t.Fatal("Docker envelope was retained")
		}
	}
}

func TestGatewayAcceptsConcatenatedAndCompressedDockerBatches(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		var forwarded string
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			forwarded = string(b)
			w.WriteHeader(http.StatusNoContent)
		}))
		handler := &Handler{Target: backend.URL}
		body := []byte(fixture + fixture)
		if compressed {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			_, _ = z.Write(body)
			_ = z.Close()
			body = b.Bytes()
		}
		req := httptest.NewRequest(http.MethodPost, "/services/collector/event/1.0", bytes.NewReader(body))
		req.Header.Set("Authorization", "Splunk private-token")
		if compressed {
			req.Header.Set("Content-Encoding", "gzip")
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"code":0`) || len(strings.Split(strings.TrimSpace(forwarded), "\n")) != 2 {
			t.Fatalf("batch not accepted: %d %s", out.Code, out.Body.String())
		}
		if strings.Contains(forwarded, "private-token") {
			t.Fatal("request header leaked")
		}
		backend.Close()
	}
}

func TestGatewayDoesNotAcknowledgeRejectedOrMalformedBatches(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; http.Error(w, "PRIVATE backend details", 503) }))
	defer backend.Close()
	handler := &Handler{Target: backend.URL}
	for _, test := range []struct {
		body   string
		status int
	}{{fixture, 503}, {fixture + `{"bad":`, 400}, {"", 400}} {
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, httptest.NewRequest(http.MethodPost, "/services/collector/event", strings.NewReader(test.body)))
		if out.Code != test.status || strings.Contains(out.Body.String(), "PRIVATE") {
			t.Fatalf("bad ack: %d %s", out.Code, out.Body.String())
		}
	}
	if calls != 1 {
		t.Fatal("partial malformed batch was forwarded")
	}
	if handler.failed.Load() != 1 || handler.accepted.Load() != 0 {
		t.Fatal("incorrect forwarding counters")
	}
}

func TestPlainInfrastructureLogsHaveServiceAndLevel(t *testing.T) {
	for _, test := range []struct{ line, level string }{{"2026-10-20 07:00:00 UTC [27] LOG: checkpoint complete", "info"}, {"WARNING: retrying", "warn"}, {"ERROR: connection failed", "error"}} {
		row, err := Normalize(map[string]any{"time": json.Number("1792479900"), "event": map[string]any{"line": test.line, "attrs": map[string]any{"compose_service": "postgres"}}})
		if err != nil || row["service"] != "postgres" || row["level"] != test.level || !strings.HasPrefix(row["_msg"].(string), "[postgres]") {
			t.Fatalf("bad plain log: %v %v", row, err)
		}
	}
}
