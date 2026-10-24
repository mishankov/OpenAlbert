package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"openalbert/internal/domain"
	"openalbert/internal/integrations"
)

func TestDisabledSearchIsHiddenAndCannotBeInvoked(t *testing.T) {
	a, _, _, conv := fixture(t)
	for _, tool := range a.toolDefinitions() {
		if tool.Name == "web_search" {
			goto configured
		}
	}
	t.Fatal("configured provider search was hidden")
configured:
	search, err := integrations.NewSearch(integrations.YandexConfig{})
	if err != nil {
		t.Fatal(err)
	}
	a.Search = search
	readerFound := false
	for _, tool := range a.toolDefinitions() {
		if tool.Name == "web_search" {
			t.Fatal("disabled search exposed to model")
		}
		if tool.Name == "web_read" {
			readerFound = true
		}
	}
	if !readerFound {
		t.Fatal("public URL reading was hidden")
	}
	out, err := a.ToolCall(context.Background(), ToolInput{RunKey: "disabled", Conversation: conv, Call: domain.ToolCall{ID: "search", Name: "web_search", Arguments: json.RawMessage(`{"query":"news"}`)}})
	if err != nil || !strings.Contains(out, "disabled") {
		t.Fatalf("stale search call should return useful error: %q %v", out, err)
	}
	var payload map[string]any
	if json.Unmarshal([]byte(out), &payload) != nil || payload["error"] == nil {
		t.Fatal("disabled search did not return an error")
	}
}
