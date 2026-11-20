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
	"openalbert/apps/backend/internal/integrations/siwc"
)

type controlStub struct {
	*billingStub
	starts, finishes, selections int
}

func (p *controlStub) Begin() (string, error) {
	p.starts++
	return "https://auth.openai.com/api/accounts/authorize?state=synthetic", nil
}
func (p *controlStub) Finish(context.Context, string) error {
	p.finishes++
	return nil
}
func (p *controlStub) Models(context.Context) ([]siwc.Model, error) {
	return []siwc.Model{{Slug: "catalog-model", DisplayName: "Account catalog model"}}, nil
}
func (p *controlStub) SetModel(context.Context, string) error {
	p.selections++
	return nil
}

type controlRoutes struct {
	*billingStub
}

func (m *controlRoutes) ProfileRoutes() map[domain.ModelRole]ProfileRouteStatus {
	return map[domain.ModelRole]ProfileRouteStatus{
		domain.RoleChat:       {Profile: "subscription_chat", Model: "chat-model", AuthMode: "siwc"},
		domain.RoleBackground: {Profile: "api_background", Model: "background-model", AuthMode: "api_key"},
		domain.RoleAnalysis:   {Profile: "subscription_analysis", Model: "analysis-model", AuthMode: "siwc"},
	}
}

func TestTelegramProfileControlsKeepModelsInConfiguration(t *testing.T) {
	plan := &controlStub{billingStub: &billingStub{}}
	c := &ChatGPTControls{Billing: &BillingModel{Mode: "profiles", Plan: plan, API: &controlRoutes{billingStub: &billingStub{}}}}
	ctx := context.Background()
	for _, command := range []string{"/chatgpt model replacement", "/chatgpt model"} {
		reply := c.reply(ctx, command)
		if !strings.Contains(reply, "models.toml") || strings.Contains(reply, "saved") || plan.selections != 0 {
			t.Fatalf("global model mutation was not rejected: %q; selections=%d", reply, plan.selections)
		}
	}
	login := c.reply(ctx, "/chatgpt login")
	if plan.starts != 1 || !strings.Contains(login, "https://auth.openai.com/") || strings.Contains(login, "fallback") {
		t.Fatalf("profile sign-in advertises a billing switch or failed: %q", login)
	}
	catalog := c.reply(ctx, "/chatgpt models")
	if !strings.Contains(catalog, "catalog-model") || !strings.Contains(catalog, "models.toml") || strings.Contains(catalog, "/chatgpt model <slug>") {
		t.Fatalf("catalog does not explain profile model management: %q", catalog)
	}
	status := c.reply(ctx, "/chatgpt status")
	for _, expected := range []string{"active lane: profiles", "analysis → subscription_analysis; model: analysis-model; auth: siwc", "background → api_background; model: background-model; auth: api_key", "chat → subscription_chat; model: chat-model; auth: siwc", "models.toml"} {
		if !strings.Contains(status, expected) {
			t.Fatalf("status missing %q: %s", expected, status)
		}
	}
	if strings.Index(status, "analysis →") > strings.Index(status, "background →") || strings.Index(status, "background →") > strings.Index(status, "chat →") || strings.Contains(status, "Model: test") || strings.Contains(status, "/chatgpt model <slug>") {
		t.Fatalf("status is unsorted or advertises the global account model: %s", status)
	}
	callback := c.reply(ctx, "/chatgpt callback http://127.0.0.1:1455/auth/callback?code=secret&state=secret")
	if plan.finishes != 1 || !strings.Contains(callback, "sign-in saved") || strings.Contains(callback, "code=secret") || strings.Contains(callback, "fallback") {
		t.Fatalf("profile callback failed or leaked credentials: %q", callback)
	}
}

func TestTelegramAPIOnlyProfilesStillShowRoutesAndConnectionInstructions(t *testing.T) {
	c := &ChatGPTControls{Billing: &BillingModel{Mode: "profiles", API: &controlRoutes{billingStub: &billingStub{}}}}
	if status := c.reply(context.Background(), "/chatgpt status"); !strings.Contains(status, "background-model") {
		t.Fatalf("API-only status hidden behind sign-in availability: %q", status)
	}
	if reply := c.reply(context.Background(), "/chatgpt login"); !strings.Contains(reply, "openai_chatgpt") || !strings.Contains(reply, "models.toml") {
		t.Fatalf("disabled connection instructions omit profile setup: %q", reply)
	}
	if reply := c.reply(context.Background(), "/chatgpt model replacement"); !strings.Contains(reply, "managed in models.toml") {
		t.Fatalf("API-only profiles do not explain model management: %q", reply)
	}
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
