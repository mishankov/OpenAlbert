package integrations

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

func TestRichMessagesKeepTableFormulaDetailsAndLongCodeUntouched(t *testing.T) {
	source := "# Comparison\n\n| | Polka | Raycast |\n|---|---|---|\n| Price | **Free** | $8–10 |\n\n$x^2$\n\n<details><summary>More</summary>\n\n||spoiler||\n\n</details>\n\n```go\n" + strings.Repeat("😀 // **literal** <tag>\n", 250) + "```"
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendRichMessage") {
			t.Errorf("wrong endpoint: %s", r.URL.Path)
		}
		var payload struct {
			ChatID   int64             `json:"chat_id"`
			Rich     map[string]string `json:"rich_message"`
			Text     string            `json:"text"`
			Entities json.RawMessage   `json:"entities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.ChatID != 7 || payload.Rich["markdown"] != source || payload.Text != "" || len(payload.Entities) > 0 {
			t.Error("rich content changed or converted to legacy entities")
		}
		count++
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tg.Send(context.Background(), domain.Conversation{ChatID: 7, OwnerID: 7, Private: true}, source); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("rich message split unexpectedly")
	}
}
