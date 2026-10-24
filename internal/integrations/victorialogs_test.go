package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/internal/domain"
)

func TestLogReaderEscapesFiltersBoundsRequestsAndKeepsStructuredResults(t *testing.T) {
	injected := `turn" OR * | fields secret`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/select/logsql/query" {
			t.Errorf("unexpected endpoint: %s %s", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		q := r.Form.Get("query")
		for _, filter := range []string{`service:="openalbert"`, `conversation_id:="personal:7"`, "turn_id:=" + strconv.Quote(injected)} {
			if !strings.Contains(q, filter) {
				t.Errorf("missing exact escaped filter %q in %q", filter, q)
			}
		}
		if r.Form.Get("timeout") != "4s" || r.Form.Get("start") == "" || r.Form.Get("end") == "" {
			t.Error("query missing limits")
		}
		encoder := json.NewEncoder(w)
		if strings.Contains(q, "| stats") {
			encoder.Encode(map[string]string{"operation": "llm.complete", "result": "error", "error_code": "http_400", "count": "30"})
			return
		}
		if r.Form.Get("limit") != "3" || !strings.Contains(q, "| fields "+logFields) {
			t.Error("event projection/limit missing")
		}
		for _, stamp := range []string{"2026-10-22T10:00:00.01Z", "2026-10-22T10:00:00.010001Z", "2026-10-22T10:00:00.02Z"} {
			encoder.Encode(map[string]string{"_time": stamp, "conversation_id": "personal:7", "operation": "llm.complete", "trace_id": "trace", "inspector_url": "http://localhost:8080/inspect/#/session/s?trace=trace", "error_code": "http_400", "private_payload": "never return this"})
		}
	}))
	defer server.Close()
	reader, err := NewVictoriaLogs(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.Search(context.Background(), domain.LogFilter{ConversationID: "personal:7", TurnID: injected, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !result.Truncated || result.Summary[0].Count != 30 || len(result.Events) != 2 {
		t.Fatalf("bad bounded result: %+v", result)
	}
	if result.Events[0]["_time"] != "2026-10-22T10:00:00.02Z" || result.Events[1]["_time"] != "2026-10-22T10:00:00.010001Z" {
		t.Fatal("wrong newest-first ordering")
	}
	if result.Until.Sub(result.Since) != time.Hour || result.Events[0]["inspector_url"] == "" {
		t.Fatal("defaults or diagnostic links lost")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "private_payload") || strings.Contains(string(raw), "never return this") {
		t.Fatal("unexpected field exposed")
	}
}

func TestLogReaderRejectsForeignMalformedAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"foreign", 200, `{"_time":"2026-10-22T10:00:00Z","conversation_id":"personal:8","_msg":"private-secret"}`},
		{"bad time", 200, `{"_time":"invalid","conversation_id":"personal:7"}`},
		{"malformed", 200, `{"private-secret":`},
		{"oversized", 200, strings.Repeat("private-secret", 100000)},
		{"HTTP error", 503, "private-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm()
				if strings.Contains(r.Form.Get("query"), "| stats") {
					fmt.Fprintln(w, `{"operation":"llm.complete","result":"error","count":"1"}`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			reader, _ := NewVictoriaLogs(server.URL, nil)
			result, err := reader.Search(context.Background(), domain.LogFilter{ConversationID: "personal:7"})
			if err == nil || strings.Contains(err.Error(), "private-secret") || len(result.Events) != 0 {
				t.Fatalf("unsafe failure: %+v %v", result, err)
			}
		})
	}
}

func TestLogReaderOutputBudgetAndInvalidFilters(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		r.ParseForm()
		encoder := json.NewEncoder(w)
		if strings.Contains(r.Form.Get("query"), "| stats") {
			for i := 0; i < 100; i++ {
				encoder.Encode(map[string]string{"operation": strings.Repeat("я", 256) + strconv.Itoa(i), "result": "error", "error_code": strings.Repeat("к", 128), "count": "2"})
			}
			return
		}
		for i := 0; i < 50; i++ {
			row := map[string]string{}
			for _, field := range strings.Split(logFields, ",") {
				row[field] = strings.Repeat("я", 100)
			}
			row["_time"] = "2026-10-22T10:00:00Z"
			row["conversation_id"] = "personal:7"
			encoder.Encode(row)
		}
	}))
	defer server.Close()
	reader, _ := NewVictoriaLogs(server.URL, nil)
	result, err := reader.Search(context.Background(), domain.LogFilter{ConversationID: "personal:7", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if len(raw) > 24000 || !result.Truncated || !result.SummaryTruncated {
		t.Fatalf("budget not enforced: %d %+v", len(raw), result)
	}
	before := calls.Load()
	for _, f := range []domain.LogFilter{{}, {ConversationID: "personal:7", Limit: 51}, {ConversationID: "personal:7", Since: time.Now(), Until: time.Now().Add(25 * time.Hour)}, {ConversationID: "personal:7", TurnID: "bad\nfilter"}} {
		if _, err := reader.Search(context.Background(), f); err == nil {
			t.Fatalf("invalid filter accepted: %+v", f)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid filters reached backend")
	}
}

func TestLogReaderDoesNotFollowRedirectsAndHonorsCancellation(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	reader, _ := NewVictoriaLogs(redirect.URL, nil)
	if _, err := reader.Search(context.Background(), domain.LogFilter{ConversationID: "personal:7"}); err == nil || destinationCalls.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.Search(ctx, domain.LogFilter{ConversationID: "personal:7"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not honored: %v", err)
	}
	for _, base := range []string{"file:///tmp/private-secret", "http://user:private-secret@localhost", "http://localhost/?token=private-secret"} {
		if _, err := NewVictoriaLogs(base, nil); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("unsafe endpoint validation")
		}
	}
	if reader, err := NewVictoriaLogs("", nil); err != nil || reader != nil {
		t.Fatal("disabled diagnostics not supported")
	}
}
