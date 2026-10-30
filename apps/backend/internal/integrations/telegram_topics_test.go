package integrations

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramPollReplyThreadsAndForumTopics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/getUpdates" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":5822,"from":{"id":7},"chat":{"id":-20,"type":"supergroup"},"text":"Original message"}},
			{"update_id":2,"message":{"message_id":5823,"from":{"id":8},"chat":{"id":-20,"type":"supergroup"},"message_thread_id":5822,"text":"Reply","reply_to_message":{"message_id":5822,"text":"Original message","from":{"id":7}}}},
			{"update_id":3,"message":{"message_id":5824,"from":{"id":7},"chat":{"id":-20,"type":"supergroup"},"message_thread_id":5822,"text":"Another reply","reply_to_message":{"message_id":5823,"text":"Reply","from":{"id":8}}}},
			{"update_id":4,"message":{"message_id":5825,"from":{"id":7},"chat":{"id":-20,"type":"supergroup","is_forum":true},"message_thread_id":3,"is_topic_message":true,"text":"Forum topic","reply_to_message":{"message_id":5820,"text":"Earlier topic message"}}},
			{"update_id":5,"message":{"message_id":5826,"from":{"id":7},"chat":{"id":-20,"type":"supergroup","is_forum":true},"message_thread_id":4,"is_topic_message":true,"text":"Another forum topic"}}
		]}`)
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 5 {
		t.Fatalf("poll failed: %+v %v", updates, err)
	}
	for i, want := range []struct {
		scope  string
		thread int
		reply  int64
	}{
		{"telegram:group:-20:topic:0", 0, 0},
		{"telegram:group:-20:topic:0", 0, 5822},
		{"telegram:group:-20:topic:0", 0, 5823},
		{"telegram:group:-20:topic:3", 3, 5820},
		{"telegram:group:-20:topic:4", 4, 0},
	} {
		in := updates[i].Incoming
		if in == nil {
			t.Fatalf("message %d was ignored", i)
		}
		if in.Conversation.ID != want.scope || in.Conversation.ThreadID != want.thread {
			t.Errorf("message %d: scope %q, delivery thread %d; want %q, %d", i, in.Conversation.ID, in.Conversation.ThreadID, want.scope, want.thread)
		}
		if want.reply != 0 && (in.ReplyTo == nil || in.ReplyTo.MessageID != want.reply || in.ReplyTo.Text == "") {
			t.Errorf("message %d lost reply context: %+v", i, in.ReplyTo)
		}
	}
}
