package integrations

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

func TestBotMessagesFollowGroupAccessAndContentRules(t *testing.T) {
	for _, tc := range []struct {
		name, chat, content, command string
		accepted                     bool
	}{
		{"group text", `{"id":-20,"type":"group","title":"Team"}`, `"text":"Hello"`, "", true},
		{"topic text", `{"id":-20,"type":"supergroup","title":"Team"}`, `"text":"Hello","is_topic_message":true,"message_thread_id":3`, "", true},
		{"rich message", `{"id":-20,"type":"group"}`, `"rich_message":{"blocks":[{"type":"paragraph","text":"Hello"}]}`, "", true},
		{"attachment", `{"id":-20,"type":"group"}`, `"caption":"Picture","photo":[{"file_id":"photo-id","file_size":123}]`, "", true},
		{"new command", `{"id":-20,"type":"group"}`, `"text":"/new"`, "new", true},
		{"stop command", `{"id":-20,"type":"group"}`, `"text":"/stop"`, "stop", true},
		{"unknown group", `{"id":-21,"type":"group"}`, `"text":"Hello"`, "", false},
		{"channel", `{"id":-20,"type":"channel"}`, `"text":"Hello"`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var human *Update
			for _, senderIsBot := range []bool{false, true} {
				for _, allowBots := range []bool{false, true} {
					message := `{"message_id":42,"date":100,"from":{"id":99,"first_name":"Alex","username":"alex","is_bot":` + strconv.FormatBool(senderIsBot) + `},"chat":` + tc.chat + `,` + tc.content + `}`
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":7,"message":`+message+`}]}`)
					}))
					tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}, AllowBotMessages: allowBots})
					if err != nil {
						t.Fatal(err)
					}
					updates, err := tg.Poll(context.Background(), 0)
					server.Close()
					if err != nil || len(updates) != 1 || updates[0].Offset != 8 {
						t.Fatalf("poll did not advance offset: %+v, %v", updates, err)
					}
					got := updates[0]
					want := tc.accepted && (!senderIsBot || allowBots)
					if (got.Incoming != nil) != want {
						t.Fatalf("is_bot=%v allow_bots=%v: %+v", senderIsBot, allowBots, got)
					}
					if !want {
						continue
					}
					if got.Incoming.Command != tc.command || got.Incoming.Conversation.OwnerID != 99 || got.Incoming.AuthorUsername != "alex" {
						t.Fatalf("lost sender or command: %+v", got.Incoming)
					}
					if tc.name == "rich message" && got.Incoming.Text != "Hello" {
						t.Fatalf("rich message was not decoded: %q", got.Incoming.Text)
					}
					if tc.name == "attachment" && (len(got.Incoming.Attachments) != 1 || got.Incoming.Attachments[0].FileID != "photo-id") {
						t.Fatal("attachment was discarded")
					}
					if senderIsBot {
						if human == nil || !reflect.DeepEqual(got, *human) {
							t.Fatalf("bot and human messages differ: bot=%+v human=%+v", got, human)
						}
					} else {
						copy := got
						human = &copy
					}
				}
			}
		})
	}
}

func TestBotMessagesNeverOpenPrivateChats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":7,"message":{"date":100,"from":{"id":7,"is_bot":true},"chat":{"id":7,"type":"private"},"text":"Hello"}}]}`)
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowBotMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 1 || updates[0].Incoming != nil || updates[0].Offset != 8 {
		t.Fatalf("private bot message accepted: %+v, %v", updates, err)
	}
}
