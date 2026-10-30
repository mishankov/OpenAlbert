package integrations

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTelegramCommandMenuAndKeyboardInput(t *testing.T) {
	configured := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = io.WriteString(w, `{"ok":true,"result":{"username":"OpenAlbertBot"}}`)
		case strings.HasSuffix(r.URL.Path, "/setMyCommands"):
			var payload struct {
				Commands []struct{ Command, Description string }
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if len(payload.Commands) != 2 || payload.Commands[0].Command != "new" || payload.Commands[1].Command != "stop" || payload.Commands[0].Description == "" {
				t.Errorf("commands not discoverable: %+v", payload)
			}
			configured = true
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			_, _ = io.WriteString(w, `{"ok":true,"result":[
				{"update_id":1,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"/new","reply_to_message":{"message_id":42,"photo":[{"file_id":"do-not-download"}]}}},
				{"update_id":2,"message":{"from":{"id":8},"chat":{"id":-20,"type":"supergroup"},"message_thread_id":3,"is_topic_message":true,"text":"/new@openalbertbot","reply_to_message":{"message_id":42,"photo":[{"file_id":"do-not-download"}]}}},
				{"update_id":3,"message":{"from":{"id":8},"chat":{"id":-20,"type":"supergroup"},"text":"/new@OtherBot","reply_to_message":{"message_id":42,"photo":[{"file_id":"do-not-download"}]}}},
				{"update_id":4,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"/newspaper"}},
				{"update_id":5,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"caption":"/new","document":{"file_id":"doc"}}},
				{"update_id":6,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"Please explain /new"}},
				{"update_id":7,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"/new topic"}},
				{"update_id":8,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"/stop","reply_to_message":{"message_id":42,"photo":[{"file_id":"do-not-download"}]}}}
			]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tg.ConfigureCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || !configured || len(updates) != 8 {
		t.Fatalf("command setup/poll failed: %+v %v", updates, err)
	}
	if updates[0].Incoming.Command != "new" || len(updates[0].Incoming.Attachments) != 0 || updates[1].Incoming.Command != "new" || len(updates[1].Incoming.Attachments) != 0 || updates[1].Incoming.Conversation.ThreadID != 3 {
		t.Fatalf("typed/menu commands differ or lost topic: %+v", updates)
	}
	if updates[2].Incoming != nil || updates[2].Offset != 4 {
		t.Fatal("command to another bot was accepted or its offset lost")
	}
	if updates[7].Incoming == nil || updates[7].Incoming.Command != "stop" || len(updates[7].Incoming.Attachments) != 0 {
		t.Fatal("stop command downloaded the referenced image or lost its command")
	}
	for _, update := range updates[3:7] {
		if update.Incoming == nil || update.Incoming.Command != "" {
			t.Fatalf("ordinary text or attachment triggered a command: %+v", update)
		}
	}
}
