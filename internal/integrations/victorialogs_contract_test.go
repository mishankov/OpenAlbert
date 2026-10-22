package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"jarvis/internal/domain"
)

func TestVictoriaLogsContract(t *testing.T) {
	base := os.Getenv("JARVIS_TEST_VICTORIALOGS_URL")
	if base == "" {
		t.Skip("set JARVIS_TEST_VICTORIALOGS_URL to a disposable VictoriaLogs instance")
	}
	reader, err := NewVictoriaLogs(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := fmt.Sprintf("logs-contract-%d", time.Now().UnixNano())
	until := time.Now().UTC()
	since := until.Add(-5 * time.Minute)
	stamp := until.Add(-time.Minute).Format(time.RFC3339Nano)
	quoted := `past" OR * | fields _msg`
	rows := []map[string]any{
		{"service": "jarvis", "conversation_id": scope, "turn_id": "past-turn", "operation": "llm.complete", "result": "error", "error_code": "http_400", "_msg": "Request rejected", "_time": stamp},
		{"service": "jarvis", "conversation_id": scope, "turn_id": "past-turn", "operation": "telegram.deliver", "result": "success", "delivery_state": "sent", "_msg": "Reply delivered", "_time": stamp},
		{"service": "jarvis", "conversation_id": scope, "turn_id": quoted, "operation": "tool.execute", "result": "error", "error_code": "http_404", "_msg": "Quoted identifier", "_time": stamp},
		{"service": "jarvis", "conversation_id": scope + "-foreign", "turn_id": "past-turn", "operation": "llm.complete", "result": "error", "_msg": "FOREIGN_PRIVATE_CONTENT", "_time": stamp},
		{"service": "postgres", "conversation_id": scope, "turn_id": "past-turn", "operation": "llm.complete", "result": "error", "_msg": "OTHER_SERVICE_CONTENT", "_time": stamp},
		{"service": "jarvis", "turn_id": "past-turn", "operation": "llm.complete", "result": "error", "_msg": "UNSCOPED_CONTENT", "_time": stamp},
		{"service": "jarvis", "conversation_id": scope, "turn_id": "past-turn", "operation": "llm.complete", "result": "error", "_msg": "OLD_CONTENT", "_time": until.Add(-time.Hour).Format(time.RFC3339Nano)},
	}
	var body bytes.Buffer
	for _, row := range rows {
		if err := json.NewEncoder(&body).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Post(strings.TrimRight(base, "/")+"/insert/jsonline?_stream_fields=service", "application/stream+json", &body)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("ingestion failed: %d", resp.StatusCode)
	}
	f := domain.LogFilter{ConversationID: scope, Since: since, Until: until, TurnID: "past-turn"}
	var result domain.LogSearch
	for attempt := 0; attempt < 30; attempt++ {
		result, err = reader.Search(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Events) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(result.Events) != 2 || len(result.Summary) != 2 {
		t.Fatalf("unexpected scoped results: %+v", result)
	}
	encoded, _ := json.Marshal(result)
	for _, private := range []string{"FOREIGN_PRIVATE_CONTENT", "OTHER_SERVICE_CONTENT", "UNSCOPED_CONTENT", "OLD_CONTENT"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("scope/time filter leaked %s", private)
		}
	}
	f.TurnID = quoted
	result, err = reader.Search(context.Background(), f)
	if err != nil || len(result.Events) != 1 || result.Events[0]["_msg"] != "Quoted identifier" {
		t.Fatalf("exact filter escaped incorrectly: %+v %v", result, err)
	}
	f.TurnID = `missing" OR * | fields _msg`
	result, err = reader.Search(context.Background(), f)
	if err != nil || len(result.Events) != 0 || len(result.Summary) != 0 {
		t.Fatalf("injected filter changed the query: %+v %v", result, err)
	}
}
