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

type controlStub struct {
	*billingStub
	starts int
}

func (p *controlStub) Begin() (string, error) {
	p.starts++
	return "https://auth.openai.com/api/accounts/authorize?state=synthetic", nil
}
func TestTelegramChatGPTControlsInterceptSecretsAndRequireOwner(t *testing.T) {
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":{"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"/chatgpt callback http://127.0.0.1:1455/auth/callback?code=secret&state=secret","document":{"file_id":"never-download"},"reply_to_message":{"photo":[{"file_id":"never-download"}]}}},{"update_id":2,"message":{"from":{"id":8},"chat":{"id":-20,"type":"supergroup"},"text":"/chatgpt login"}}]}`)
			return
		}
		data, _ := io.ReadAll(r.Body)
		sent = append(sent, string(data))
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "test", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
	if err != nil {
		t.Fatal(err)
	}
	plan := &controlStub{billingStub: &billingStub{}}
	tg.ChatGPT = &ChatGPTControls{OwnerID: 7, Billing: &BillingModel{Mode: "auto", Plan: plan}}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 2 {
		t.Fatal(err)
	}
	if in := updates[0].Incoming; in == nil || in.Command != "chatgpt" || len(in.Attachments) != 0 || in.ReplyTo != nil {
		t.Fatal("callback can enter history/media processing")
	}
	if err = tg.HandleChatGPT(context.Background(), *updates[1].Incoming); err != nil {
		t.Fatal(err)
	}
	if plan.starts != 0 {
		t.Fatal("group participant changed owner's billing")
	}
	owner := domain.Incoming{Conversation: domain.Conversation{ChatID: 7, OwnerID: 7, Private: true}, Text: "/chatgpt login"}
	if err = tg.HandleChatGPT(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(sent[len(sent)-1]), &body)
	if plan.starts != 1 || !strings.Contains(string(body["reply_markup"]), "Continue with ChatGPT") || strings.Contains(strings.Join(sent, ""), "secret") {
		t.Fatal("missing sign-in button or leaked callback")
	}
}

func TestReplyToCallbackCaptionOrRichMessageIsScrubbed(t *testing.T) {
	for _, format := range []string{"caption", "rich_message"} {
		t.Run(format, func(t *testing.T) {
			callback := "http://127.0.0.1:1455/auth/callback?code=secret-code&state=secret-state"
			reply := map[string]any{"message_id": 10, "document": map[string]string{"file_id": "do-not-download", "mime_type": "image/png"}}
			if format == "caption" {
				reply[format] = callback
			} else {
				reply[format] = map[string]any{"blocks": []any{map[string]any{"type": "paragraph", "text": callback}}}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{map[string]any{"update_id": 3, "message": map[string]any{"from": map[string]int{"id": 7}, "chat": map[string]any{"id": 7, "type": "private"}, "text": "thanks", "reply_to_message": reply, "quote": map[string]any{"text": callback}}}}})
			}))
			defer server.Close()
			tg, err := NewTelegram(TelegramConfig{Token: "test", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
			if err != nil {
				t.Fatal(err)
			}
			updates, err := tg.Poll(context.Background(), 0)
			if err != nil || len(updates) != 1 {
				t.Fatal(err)
			}
			in := updates[0].Incoming
			raw, _ := json.Marshal(in)
			if in == nil || in.Text != "thanks" || in.ReplyTo != nil || in.Quote != nil || len(in.Attachments) != 0 || strings.Contains(string(raw), "secret-") {
				t.Fatalf("callback leaked in %s", raw)
			}
		})
	}
}
