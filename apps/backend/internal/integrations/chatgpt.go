package integrations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
)

// ChatGPTControls handles owner-only settings before persistence or model input.
// Even rejected callbacks are consumed here; OAuth codes never enter chat history.
type ChatGPTControls struct {
	Billing *BillingModel
	OwnerID int64
}

func chatGPTCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	command, _, _ := strings.Cut(fields[0], "@")
	return command == "/chatgpt" || strings.Contains(text, "http://127.0.0.1:1455/auth/callback")
}
func (t *Telegram) PrepareChatGPTReply(ctx context.Context, in domain.Incoming) string {
	text := "ChatGPT settings are only available in the owner's private chat."
	if t.ChatGPT != nil && in.Conversation.Private && in.Conversation.OwnerID == t.ChatGPT.OwnerID {
		text = t.ChatGPT.reply(ctx, in.Text)
	}
	return text
}
func (t *Telegram) HandleChatGPT(ctx context.Context, in domain.Incoming) error {
	return t.SendChatGPTReply(ctx, in.Conversation, t.PrepareChatGPTReply(ctx, in))
}
func (t *Telegram) SendChatGPTReply(ctx context.Context, conv domain.Conversation, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, chunk := range messagefmt.Split(messagefmt.Message{Text: text}, messagefmt.TextLimit) {
		payload := map[string]any{"chat_id": conv.ChatID, "text": chunk.Text, "link_preview_options": map[string]bool{"is_disabled": true}}
		if conv.ThreadID > 0 {
			payload["message_thread_id"] = conv.ThreadID
		}
		for _, line := range strings.Split(chunk.Text, "\n") {
			if strings.HasPrefix(line, "https://auth.openai.com/api/accounts/authorize?") {
				payload["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "Continue with ChatGPT", "url": line}}}}
			}
		}
		for attempt := 0; ; attempt++ {
			err := t.api(ctx, "sendMessage", payload, nil)
			if err == nil {
				break
			}
			var api *TelegramError
			if attempt >= 2 || !errors.As(err, &api) || !api.Rejected || (api.Code != 429 && api.Code < 500) {
				return err
			}
			delay := time.Duration(attempt+1) * time.Second
			if api.RetryAfter > delay {
				delay = api.RetryAfter
			}
			if delay > 3*time.Second {
				return err
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}
func (c *ChatGPTControls) reply(ctx context.Context, text string) string {
	b := c.Billing
	fields := strings.Fields(text)
	action := "status"
	if len(fields) > 1 {
		action = fields[1]
	}
	if strings.HasPrefix(text, "http://127.0.0.1:1455/auth/callback") {
		action = "callback"
		fields = []string{"/chatgpt", "callback", text}
	}
	if action == "model" && b.Mode == "profiles" {
		return "Models are managed in models.toml in profiles mode. Set model on the profile referenced by routes.chat, routes.background or routes.analysis, then restart the server. Use /chatgpt models to view the account catalog."
	}
	if b.Plan == nil && action != "status" {
		return "ChatGPT plan usage is disabled. Add an openai_chatgpt profile to models.toml, route a role to it and use OPENAI_AUTH_MODE=profiles, then restart the server. Legacy API-only configurations can use OPENAI_AUTH_MODE=siwc or auto."
	}
	switch action {
	case "login":
		link, err := b.Plan.Begin()
		if err != nil {
			return err.Error()
		}
		message := "Continue with ChatGPT (/chatgpt login)\n" + link + "\n\nOpen this link in your browser. After approval the loopback page may fail to load. Within ten minutes send /chatgpt callback followed by the complete URL from its address bar. Plan usage shares your ChatGPT allowance."
		if b.Mode == "auto" {
			message += " In auto mode a fallback request is charged to your API key."
		} else if b.Mode == "profiles" {
			message += " Each role uses its configured profile and model from models.toml; sign-in does not change routing."
		}
		return message
	case "callback":
		if len(fields) != 3 {
			return "Use /chatgpt callback <complete loopback URL>."
		}
		if err := b.Plan.Finish(ctx, fields[2]); err != nil {
			return "ChatGPT sign-in failed. " + err.Error()
		}
		b.Reconnected()
		return "ChatGPT sign-in saved on the server.\n" + c.reply(ctx, "/chatgpt status")
	case "models":
		models, err := b.Plan.Models(ctx)
		if err != nil {
			return err.Error()
		}
		var lines []string
		for _, m := range models {
			lines = append(lines, m.DisplayName+" — "+m.Slug)
		}
		if b.Mode == "profiles" {
			return "ChatGPT account catalog (set model on the relevant profile in models.toml, then restart the server):\n" + strings.Join(lines, "\n")
		}
		return "ChatGPT models (select with /chatgpt model <slug>):\n" + strings.Join(lines, "\n")
	case "model":
		if len(fields) != 3 {
			return "Use /chatgpt model <slug> from /chatgpt models."
		}
		if err := b.Plan.SetModel(ctx, fields[2]); err != nil {
			return err.Error()
		}
		return "ChatGPT model saved."
	case "status":
		s := b.Status()
		last := "never"
		if s.Last429 != nil {
			last = s.Last429.Format(time.RFC3339)
		}
		retry := ""
		if s.PlanIneligible {
			retry = "\nChatGPT plan usage is unavailable for this account or workspace. No automatic retry; check eligibility before signing in again."
		}
		if s.RetryAt != nil {
			retry = "\nLocal fallback cooldown until " + s.RetryAt.Format(time.RFC3339) + " (not a plan reset time)."
		}
		models := "Model: " + s.SIWC.Model
		commands := "/chatgpt login · /chatgpt models · /chatgpt model <slug> · /chatgpt status"
		if s.Mode == "profiles" {
			roles := make([]string, 0, len(s.Profiles))
			for role := range s.Profiles {
				roles = append(roles, string(role))
			}
			sort.Strings(roles)
			lines := []string{"Configured role routes:"}
			for _, role := range roles {
				route := s.Profiles[domain.ModelRole(role)]
				lines = append(lines, fmt.Sprintf("%s → %s; model: %s; auth: %s", role, route.Profile, route.Model, route.AuthMode))
			}
			lines = append(lines, "Models are managed in models.toml; restart the server after changes.")
			models = strings.Join(lines, "\n")
			commands = "/chatgpt login · /chatgpt models · /chatgpt status"
		}
		return fmt.Sprintf("ChatGPT settings\nMode: %s; active lane: %s\nSigned in: %t; plan usage enabled: %t\n%s\nRequests: ChatGPT %d; API key %d (since server start)\nLast 429: %s\nLast error: %s; HTTP %d; request ID: %s%s\nUsage: https://chatgpt.com/settings/usage\nCommands: %s", s.Mode, s.ActiveLane, s.SIWC.Connected, s.SIWC.PlanEnabled, models, s.SIWCRequests, s.APIRequests, last, s.LastErrorCode, s.LastHTTPStatus, s.LastRequestID, retry, commands)
	default:
		return "Use /chatgpt status, /chatgpt login, /chatgpt models, or /chatgpt model <slug>."
	}
}
