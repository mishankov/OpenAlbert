package integrations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramAddressingUsesEntitiesAndThisBotsIdentity(t *testing.T) {
	cases := []struct {
		name, message string
		addressed     bool
	}{
		{"plain conversation", `{"text":"Всем привет"}`, false},
		{"name in prose", `{"text":"Альберт, привет"}`, false},
		{"raw tag without entity", `{"text":"@AlbertBot"}`, false},
		{"UTF16 mention after emoji", `{"text":"😀 @aLbertBot","entities":[{"type":"mention","offset":3,"length":10}]}`, true},
		{"other bot", `{"text":"@OtherBot","entities":[{"type":"mention","offset":0,"length":9}]}`, false},
		{"username prefix", `{"text":"@AlbertBotOther","entities":[{"type":"mention","offset":0,"length":15}]}`, false},
		{"caption mention", `{"caption":"@AlbertBot","caption_entities":[{"type":"mention","offset":0,"length":10}]}`, true},
		{"reply to this bot", `{"text":"А подробнее?","reply_to_message":{"message_id":1,"from":{"id":42,"is_bot":true,"username":"AlbertBot"}}}`, true},
		{"reply to another bot", `{"text":"А подробнее?","reply_to_message":{"message_id":1,"from":{"id":43,"is_bot":true,"username":"OtherBot"}}}`, false},
		{"reply to human", `{"text":"А подробнее?","reply_to_message":{"message_id":1,"from":{"id":9,"username":"AlbertBot"}}}`, false},
		{"text mention", `{"text":"Бот","entities":[{"type":"text_mention","offset":0,"length":3,"user":{"id":42}}]}`, true},
		{"invalid range", `{"text":"@AlbertBot","entities":[{"type":"mention","offset":-1,"length":10},{"type":"mention","offset":0,"length":9999999}]}`, false},
	}
	var updates []any
	for i, tc := range cases {
		var message map[string]any
		if err := json.Unmarshal([]byte(tc.message), &message); err != nil {
			t.Fatal(err)
		}
		message["chat"] = map[string]any{"id": -20, "type": "supergroup"}
		message["from"] = map[string]any{"id": 8}
		updates = append(updates, map[string]any{"update_id": i + 1, "message": message})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/getUpdates" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	tg.username, tg.botID = "AlbertBot", 42
	got, err := tg.Poll(context.Background(), 0)
	if err != nil || len(got) != len(cases) {
		t.Fatalf("poll failed: %d updates, %v", len(got), err)
	}
	for i, tc := range cases {
		if got[i].Incoming == nil || got[i].Incoming.AddressedToBot != tc.addressed {
			t.Errorf("%s: wrong addressing metadata: %+v", tc.name, got[i].Incoming)
		}
	}
}
