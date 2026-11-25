package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
)

// isWorkspaceGroup reports whether conv belongs to the configured owner workspace supergroup.
func (a *Activities) isWorkspaceGroup(conv domain.Conversation) bool {
	return a.WorkspaceGroupID < 0 && !conv.Private && conv.ChatID == a.WorkspaceGroupID
}

// isOwnerWorkspace strictly requires the configured owner workspace supergroup
// AND an authenticated sender matching OwnerUserID.
func (a *Activities) isOwnerWorkspace(conv domain.Conversation, senderID int64) bool {
	return a.OwnerUserID > 0 && a.isWorkspaceGroup(conv) && senderID == a.OwnerUserID
}

// isOwnerAuthorized reports whether conv is an owner-privileged context:
// either the owner's private chat or an owner-authenticated workspace topic.
func (a *Activities) isOwnerAuthorized(conv domain.Conversation) bool {
	return a.isOwnerPrivate(conv) || a.isOwnerWorkspace(conv, conv.OwnerID)
}

func (a *Activities) ownerPersonalScope() string {
	if a.OwnerUserID <= 0 {
		return ""
	}
	return fmt.Sprintf("personal:%d", a.OwnerUserID)
}

func (a *Activities) memoryWriteScope(conv domain.Conversation) string {
	if a.isOwnerWorkspace(conv, conv.OwnerID) {
		return a.ownerPersonalScope()
	}
	return conv.ID
}

func (a *Activities) workspaceTransport() domain.WorkspaceTransport {
	if a.Workspace != nil {
		return a.Workspace
	}
	if t, ok := a.Messenger.(domain.WorkspaceTransport); ok {
		return t
	}
	return nil
}

func (a *Activities) sanitizeWorkspaceTaskConversation(ctx context.Context, conv domain.Conversation, taskID string) (domain.Conversation, error) {
	if taskID == "" || !a.isWorkspaceGroup(conv) || a.Repo == nil {
		return conv, nil
	}
	t, found, err := a.Repo.Task(ctx, conv.ID, taskID)
	if err != nil {
		return conv, err
	}
	if !found || !t.OwnerAuthorized {
		conv.OwnerID = 0
	}
	return conv, nil
}

func (a *Activities) isOwnerWorkspaceSession(ctx context.Context, conv domain.Conversation, session domain.Session, sessTrigger string) (bool, error) {
	if a.OwnerUserID <= 0 || !a.isWorkspaceGroup(conv) || a.Repo == nil {
		return false, nil
	}
	messages, err := a.Repo.Messages(ctx, session.ID)
	if err != nil {
		return false, err
	}
	verifiedOwnerMsg := false
	for _, m := range messages {
		if m.ParentToolCallID != "" || m.Role != "user" {
			continue
		}
		if m.SourceMessageID != "" {
			if m.AuthorID != a.OwnerUserID {
				return false, nil
			}
			in, found, err := a.Repo.Incoming(ctx, m.SourceMessageID)
			if err != nil {
				return false, err
			}
			if found && in.Conversation.OwnerID != a.OwnerUserID {
				return false, nil
			}
			verifiedOwnerMsg = true
			continue
		}
		if m.AuthorID > 0 && m.AuthorID != a.OwnerUserID {
			return false, nil
		}
	}
	if verifiedOwnerMsg {
		return true, nil
	}
	if sessTrigger == "task" {
		raw, ok, err := a.Repo.LoadCheckpoint(ctx, "turn-done/"+session.ID)
		if err != nil {
			return false, err
		}
		if ok {
			var done Turn
			if json.Unmarshal(raw, &done) == nil && done.Input.TaskID != "" {
				t, found, err := a.Repo.Task(ctx, conv.ID, done.Input.TaskID)
				if err != nil {
					return false, err
				}
				return found && t.OwnerAuthorized, nil
			}
		}
	}
	return false, nil
}

func (a *Activities) workspaceToolDefinitions() []domain.Tool {
	if a.WorkspaceGroupID >= 0 || a.workspaceTransport() == nil {
		return nil
	}
	return []domain.Tool{
		{
			Name: "workspace_post",
			Description: "Post a formatted Markdown message with optional inline keyboard buttons ({text, callback_data}) " +
				"to a forum topic in the owner's Workspace Supergroup, its #Control (General) topic, or the owner's private DM. " +
				"Omit conversation_id to post to #Control (topic:0). " +
				"Available in owner private chat, owner workspace supergroup, background tasks, and coding sessions when TELEGRAM_WORKSPACE_GROUP_ID is configured.",
			Parameters: schema(
				`"conversation_id":{"type":"string","description":"Optional target conversation ID: a workspace topic (telegram:group:<id>:topic:<thread_id>), #Control (topic:0), or the owner's private chat (personal:<owner_id>)."},`+
					`"text":{"type":"string","description":"Formatted Markdown message text (1–32000 UTF-8 bytes)."},`+
					`"buttons":{"type":"array","description":"Optional inline keyboard buttons. Pass a flat list of {text, callback_data} objects (arranged up to 3 per row) or explicit rows of button arrays.","items":{"type":"object","properties":{"text":{"type":"string"},"callback_data":{"type":"string"},"url":{"type":"string"}},"required":["text"],"additionalProperties":false}}`,
				"text",
			),
		},
		{
			Name: "topic_create",
			Description: "Create a new forum topic in the owner's Workspace Supergroup and register its conversation scope " +
				"(telegram:group:<chat_id>:topic:<thread_id>). Optional icon_color accepts Telegram's RGB integers " +
				"(7322096, 16766590, 13338331, 9367192, 16749490, 16478047). Returns conversation_id, chat_id, thread_id, and title.",
			Parameters: schema(
				`"title":{"type":"string","description":"Topic title (1–128 characters)."},`+
					`"icon_color":{"type":"integer","minimum":0,"description":"Optional Telegram forum topic icon color RGB integer."}`,
				"title",
			),
		},
		{
			Name:        "topic_update",
			Description: "Rename and/or close or reopen an existing forum topic in the owner's Workspace Supergroup. Provide at least title or status (\"open\" or \"closed\").",
			Parameters: schema(
				`"conversation_id":{"type":"string","description":"Workspace topic conversation ID (telegram:group:<chat_id>:topic:<thread_id>)."},`+
					`"title":{"type":"string","description":"Optional new topic title (1–128 characters)."},`+
					`"status":{"type":"string","enum":["open","closed"],"description":"Optional topic status transition: open (reopen) or closed."}`,
				"conversation_id",
			),
		},
	}
}

func (a *Activities) workspaceToolAllowed(name string, turn Turn) bool {
	if !a.isOwnerAuthorized(turn.Conversation) || turn.Input.Initiative || a.WorkspaceGroupID >= 0 || a.workspaceTransport() == nil {
		return false
	}
	return isWorkspaceTool(name)
}

func (a *Activities) workspaceToolAccess(in ToolInput) error {
	if !a.isOwnerAuthorized(in.Conversation) || in.Background {
		return errors.New("workspace tools are only available in authorized owner conversations and their scheduled tasks")
	}
	if a.WorkspaceGroupID >= 0 {
		return errors.New("TELEGRAM_WORKSPACE_GROUP_ID is not configured")
	}
	if a.workspaceTransport() == nil {
		return errors.New("Telegram workspace transport is unavailable")
	}
	return nil
}

func isThreadMissingError(err error) bool {
	var tm interface{ IsThreadMissing() bool }
	return errors.As(err, &tm) && tm.IsThreadMissing()
}

func isWorkspaceTool(name string) bool {
	return name == "topic_create" || name == "topic_update" || name == "workspace_post"
}

func decodeStrictJSON(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > 100000 || trimmed[0] != '{' {
		return errors.New("invalid tool JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid tool JSON")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("invalid tool JSON")
	}
	return nil
}

func validateTopicTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" || !utf8.ValidString(title) || strings.ContainsRune(title, 0) || utf8.RuneCountInString(title) > 128 {
		return "", errors.New("topic title must be valid UTF-8 and 1–128 characters")
	}
	return title, nil
}

func parseWorkspaceButtons(raw json.RawMessage) (*domain.InlineKeyboardMarkup, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var rows [][]domain.InlineButton
	// Accept either explicit 2D rows or a flat array of button objects.
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		rows = nil
		var flat []domain.InlineButton
		if err2 := json.Unmarshal(trimmed, &flat); err2 != nil {
			return nil, errors.New("buttons must be an array of {text, callback_data} objects or rows of buttons")
		}
		for i := 0; i < len(flat); i += 3 {
			end := min(i+3, len(flat))
			rows = append(rows, flat[i:end])
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}
	total := 0
	for _, row := range rows {
		if len(row) == 0 {
			return nil, errors.New("button row must not be empty")
		}
		for _, btn := range row {
			total++
			if total > 100 {
				return nil, errors.New("buttons exceed Telegram limit of 100")
			}
			if strings.TrimSpace(btn.Text) == "" || !utf8.ValidString(btn.Text) || utf8.RuneCountInString(btn.Text) > 64 {
				return nil, errors.New("button text must be valid UTF-8 and 1–64 characters")
			}
			hasData := btn.CallbackData != ""
			hasURL := btn.URL != ""
			if hasData == hasURL {
				return nil, errors.New("each button must set exactly one of callback_data or url")
			}
			if hasData && (!utf8.ValidString(btn.CallbackData) || len(btn.CallbackData) > 64) {
				return nil, errors.New("button callback_data must be valid UTF-8 and 1–64 bytes")
			}
			if hasURL {
				u, err := url.Parse(btn.URL)
				if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
					return nil, errors.New("button url must be an HTTP(S) URL")
				}
			}
		}
	}
	return &domain.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

func (a *Activities) executeWorkspaceTool(ctx context.Context, in ToolInput) (any, error) {
	if err := a.workspaceToolAccess(in); err != nil {
		return nil, err
	}
	transport := a.workspaceTransport()
	switch in.Call.Name {
	case "topic_create":
		var p struct {
			Title     string `json:"title"`
			IconColor int    `json:"icon_color"`
		}
		if err := decodeStrictJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		title, err := validateTopicTitle(p.Title)
		if err != nil {
			return nil, err
		}
		if p.IconColor < 0 {
			return nil, errors.New("icon_color must be nonnegative")
		}
		if a.Repo == nil {
			return nil, errors.New("repository unavailable")
		}
		topic, err := transport.CreateForumTopic(ctx, a.WorkspaceGroupID, title, p.IconColor)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(topic.Name)
		if name == "" {
			name = title
		}
		conv := domain.Conversation{
			ID:       fmt.Sprintf("telegram:group:%d:topic:%d", a.WorkspaceGroupID, topic.ThreadID),
			Title:    name,
			ChatID:   a.WorkspaceGroupID,
			ThreadID: topic.ThreadID,
			OwnerID:  a.OwnerUserID,
			Private:  false,
		}
		if err := a.Repo.SaveConversation(ctx, conv); err != nil {
			return nil, err
		}
		return map[string]any{
			"conversation_id": conv.ID,
			"chat_id":         conv.ChatID,
			"thread_id":       conv.ThreadID,
			"title":           conv.Title,
		}, nil

	case "topic_update":
		var p struct {
			ConversationID string `json:"conversation_id"`
			Title          string `json:"title"`
			Status         string `json:"status"`
		}
		if err := decodeStrictJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.ConversationID) == "" {
			return nil, errors.New("conversation_id is required")
		}
		if a.Repo == nil {
			return nil, errors.New("repository unavailable")
		}
		conv, found, err := a.Repo.Conversation(ctx, p.ConversationID)
		if err != nil {
			return nil, err
		}
		if !found || conv.Private || conv.ChatID != a.WorkspaceGroupID || conv.ThreadID <= 0 {
			return nil, errors.New("target must be a known forum topic in the owner workspace supergroup")
		}
		title := ""
		if p.Title != "" {
			title, err = validateTopicTitle(p.Title)
			if err != nil {
				return nil, err
			}
		}
		if p.Status != "" && p.Status != "open" && p.Status != "closed" {
			return nil, errors.New("status must be open or closed")
		}
		if title == "" && p.Status == "" {
			return nil, errors.New("provide title and/or status")
		}
		if title != "" {
			if err := transport.EditForumTopic(ctx, conv.ChatID, conv.ThreadID, title); err != nil {
				return nil, err
			}
			conv.Title = title
			conv.TitleFromChat = false
			conv.OwnerID = a.OwnerUserID
			if err := a.Repo.SaveConversation(ctx, conv); err != nil {
				return nil, err
			}
		}
		switch p.Status {
		case "closed":
			if err := transport.CloseForumTopic(ctx, conv.ChatID, conv.ThreadID); err != nil {
				return nil, err
			}
		case "open":
			if err := transport.ReopenForumTopic(ctx, conv.ChatID, conv.ThreadID); err != nil {
				return nil, err
			}
		}
		out := map[string]any{
			"conversation_id": conv.ID,
			"chat_id":         conv.ChatID,
			"thread_id":       conv.ThreadID,
			"title":           conv.Title,
		}
		if p.Status != "" {
			out["status"] = p.Status
		}
		return out, nil

	case "workspace_post":
		var p struct {
			ConversationID string          `json:"conversation_id"`
			Text           string          `json:"text"`
			Buttons        json.RawMessage `json:"buttons"`
		}
		if err := decodeStrictJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		if !visibleMessageText(p.Text) {
			return nil, errors.New("workspace_post text must not be empty")
		}
		if !utf8.ValidString(p.Text) || len(p.Text) > 32000 {
			return nil, errors.New("workspace_post text must be valid UTF-8 and at most 32000 bytes")
		}
		if (in.Background || in.TaskID != "") && backgroundPlaceholder(p.Text) {
			return nil, errors.New("background placeholder is not a notification")
		}
		markup, err := parseWorkspaceButtons(p.Buttons)
		if err != nil {
			return nil, err
		}
		target, err := a.resolveWorkspacePostTarget(ctx, strings.TrimSpace(p.ConversationID))
		if err != nil {
			return nil, err
		}
		parts := messagefmt.Split(messagefmt.Markdown(p.Text), messagefmt.TextLimit)
		if len(parts) == 0 {
			return nil, errors.New("workspace_post text must not be empty")
		}
		var lastMessageID int64
		recreated := false
		for i, part := range parts {
			var partMarkup *domain.InlineKeyboardMarkup
			if i == len(parts)-1 {
				partMarkup = markup
			}
			msgID, err := transport.SendMessageWithButtons(ctx, target, part.Text, part.Entities, partMarkup)
			if err != nil && i == 0 && !recreated && target.ThreadID > 0 && a.isWorkspaceGroup(target) && isThreadMissingError(err) {
				oldConvID := target.ID
				title := strings.TrimSpace(target.Title)
				if title == "" {
					title = "🔄 Задача"
				}
				if validTitle, vErr := validateTopicTitle(title); vErr == nil {
					title = validTitle
				} else {
					title = "🔄 Задача"
				}
				topic, createErr := transport.CreateForumTopic(ctx, a.WorkspaceGroupID, title, 0)
				if createErr != nil {
					return nil, createErr
				}
				name := strings.TrimSpace(topic.Name)
				if name == "" {
					name = title
				}
				target = domain.Conversation{
					ID:       fmt.Sprintf("telegram:group:%d:topic:%d", a.WorkspaceGroupID, topic.ThreadID),
					Title:    name,
					ChatID:   a.WorkspaceGroupID,
					ThreadID: topic.ThreadID,
					OwnerID:  a.OwnerUserID,
					Private:  false,
				}
				if a.Repo != nil {
					if saveErr := a.Repo.SaveConversation(ctx, target); saveErr != nil {
						return nil, saveErr
					}
				}
				if a.T3 != nil {
					if threads, listErr := a.T3.ListThreads(ctx, domain.T3ThreadFilter{ConversationID: oldConvID}); listErr == nil {
						for _, th := range threads {
							_, _ = a.T3.RebindThreadConversation(ctx, th.ThreadID, target.ID)
						}
					}
				}
				recreated = true
				msgID, err = transport.SendMessageWithButtons(ctx, target, part.Text, part.Entities, partMarkup)
			}
			if err != nil {
				return nil, err
			}
			lastMessageID = msgID
		}
		out := map[string]any{
			"state":           "sent",
			"message_id":      lastMessageID,
			"conversation_id": target.ID,
			"chat_id":         target.ChatID,
			"thread_id":       target.ThreadID,
			"parts":           len(parts),
		}
		if recreated {
			out["recreated"] = true
		}
		return out, nil

	default:
		return nil, errors.New("unknown workspace tool")
	}
}

func (a *Activities) resolveWorkspacePostTarget(ctx context.Context, conversationID string) (domain.Conversation, error) {
	if a.WorkspaceGroupID >= 0 {
		return domain.Conversation{}, errors.New("TELEGRAM_WORKSPACE_GROUP_ID is not configured")
	}
	personalID := a.ownerPersonalScope()
	controlID := fmt.Sprintf("telegram:group:%d:topic:0", a.WorkspaceGroupID)
	if conversationID == "" {
		conversationID = controlID
	}
	if personalID != "" && conversationID == personalID {
		return domain.Conversation{
			ID:      personalID,
			ChatID:  a.OwnerUserID,
			OwnerID: a.OwnerUserID,
			Private: true,
		}, nil
	}
	if conversationID == controlID {
		target := domain.Conversation{
			ID:       controlID,
			ChatID:   a.WorkspaceGroupID,
			ThreadID: 0,
			OwnerID:  a.OwnerUserID,
			Private:  false,
		}
		if a.Repo != nil {
			if existing, ok, err := a.Repo.Conversation(ctx, controlID); err == nil && ok {
				target.Title = existing.Title
			}
		}
		return target, nil
	}
	if a.Repo == nil {
		return domain.Conversation{}, errors.New("repository unavailable")
	}
	target, found, err := a.Repo.Conversation(ctx, conversationID)
	if err != nil {
		return domain.Conversation{}, err
	}
	if !found || target.Private || target.ChatID != a.WorkspaceGroupID || target.ThreadID < 0 {
		return domain.Conversation{}, errors.New("target must be a workspace topic, the workspace General topic, or the owner's private chat")
	}
	target.OwnerID = a.OwnerUserID
	return target, nil
}

// WorkspaceController provides reusable helpers for managing owner workspace
// forum topic prefixes and maintaining a pinned status board in the #Control topic.
type WorkspaceController struct {
	Repo             domain.Repository
	Transport        domain.WorkspaceTransport
	OwnerUserID      int64
	WorkspaceGroupID int64
	ControlThreadID  int // Defaults to 0 (Telegram General / #Control topic).

	mu sync.Mutex
}

// ControlConversation returns the conversation descriptor for the workspace #Control topic.
func (c *WorkspaceController) ControlConversation() (domain.Conversation, error) {
	if c.WorkspaceGroupID >= 0 || c.OwnerUserID <= 0 {
		return domain.Conversation{}, errors.New("workspace controller requires negative WorkspaceGroupID and positive OwnerUserID")
	}
	if c.ControlThreadID < 0 {
		return domain.Conversation{}, errors.New("control thread_id must be nonnegative")
	}
	return domain.Conversation{
		ID:       fmt.Sprintf("telegram:group:%d:topic:%d", c.WorkspaceGroupID, c.ControlThreadID),
		ChatID:   c.WorkspaceGroupID,
		ThreadID: c.ControlThreadID,
		OwnerID:  c.OwnerUserID,
		Private:  false,
	}, nil
}

func formatTopicTitle(prefix, baseTitle string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	baseTitle = strings.TrimSpace(baseTitle)
	if baseTitle == "" {
		return "", errors.New("topic title must not be empty")
	}
	if !utf8.ValidString(prefix) || !utf8.ValidString(baseTitle) || strings.ContainsRune(prefix, 0) || strings.ContainsRune(baseTitle, 0) {
		return "", errors.New("topic title must be valid UTF-8")
	}
	name := baseTitle
	if prefix != "" {
		if baseTitle != prefix && !strings.HasPrefix(baseTitle, prefix+" ") {
			name = prefix + " " + baseTitle
		}
	}
	runes := []rune(name)
	if len(runes) > 128 {
		name = strings.TrimSpace(string(runes[:128]))
	}
	return validateTopicTitle(name)
}

// UpdateTopicPrefix updates a workspace forum topic's title with the given prefix
// (e.g. "🟢", "⏳", "[done]") and persists the updated title if Repo is configured.
func (c *WorkspaceController) UpdateTopicPrefix(ctx context.Context, threadID int, prefix string, baseTitle string) (string, error) {
	if c.WorkspaceGroupID >= 0 || c.Transport == nil {
		return "", errors.New("workspace controller is not configured")
	}
	if threadID <= 0 {
		return "", errors.New("forum topic thread_id must be positive")
	}
	name, err := formatTopicTitle(prefix, baseTitle)
	if err != nil {
		return "", err
	}
	if err := c.Transport.EditForumTopic(ctx, c.WorkspaceGroupID, threadID, name); err != nil {
		return "", err
	}
	if c.Repo != nil {
		convID := fmt.Sprintf("telegram:group:%d:topic:%d", c.WorkspaceGroupID, threadID)
		conv, found, err := c.Repo.Conversation(ctx, convID)
		if err != nil {
			return name, err
		}
		if !found {
			conv = domain.Conversation{
				ID:       convID,
				ChatID:   c.WorkspaceGroupID,
				ThreadID: threadID,
				OwnerID:  c.OwnerUserID,
				Private:  false,
			}
		}
		conv.Title = name
		conv.TitleFromChat = false
		if c.OwnerUserID > 0 {
			conv.OwnerID = c.OwnerUserID
		}
		if err := c.Repo.SaveConversation(ctx, conv); err != nil {
			return name, err
		}
	}
	return name, nil
}

type pinnedStatusState struct {
	MessageID int64 `json:"message_id"`
	Pinned    bool  `json:"pinned"`
}

func (c *WorkspaceController) pinnedStatusKey() string {
	return fmt.Sprintf("workspace/pinned-status/%d/%d", c.WorkspaceGroupID, c.ControlThreadID)
}

// UpsertPinnedStatus creates and pins a status board message in the workspace
// #Control topic on first call, and edits it in place on subsequent calls.
func (c *WorkspaceController) UpsertPinnedStatus(ctx context.Context, text string) (int64, error) {
	return c.UpsertPinnedStatusWithButtons(ctx, text, nil)
}

// UpsertPinnedStatusWithButtons is like UpsertPinnedStatus with an optional inline keyboard.
func (c *WorkspaceController) UpsertPinnedStatusWithButtons(ctx context.Context, text string, replyMarkup *domain.InlineKeyboardMarkup) (int64, error) {
	if c.Transport == nil || c.Repo == nil {
		return 0, errors.New("workspace controller requires Transport and Repo")
	}
	conv, err := c.ControlConversation()
	if err != nil {
		return 0, err
	}
	if !visibleMessageText(text) || !utf8.ValidString(text) {
		return 0, errors.New("pinned status text must be non-empty valid UTF-8")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := c.pinnedStatusKey()
	var state pinnedStatusState
	if raw, ok, err := c.Repo.LoadCheckpoint(ctx, key); err != nil {
		return 0, err
	} else if ok {
		_ = json.Unmarshal(raw, &state)
	}

	if state.MessageID > 0 {
		err := c.Transport.EditMessageText(ctx, c.WorkspaceGroupID, state.MessageID, text, replyMarkup)
		if err == nil {
			if !state.Pinned {
				if pinErr := c.Transport.PinChatMessage(ctx, c.WorkspaceGroupID, state.MessageID, true); pinErr != nil {
					return state.MessageID, pinErr
				}
				state.Pinned = true
				raw, _ := json.Marshal(state)
				_ = c.Repo.SaveCheckpoint(ctx, key, raw)
			}
			return state.MessageID, nil
		}
		var missing interface{ IsMessageMissing() bool }
		if !errors.As(err, &missing) || !missing.IsMessageMissing() {
			return 0, err
		}
		// Pinned message was deleted; fall through to create a fresh pinned status message.
		state = pinnedStatusState{}
	}

	msgID, err := c.Transport.SendMessageWithButtons(ctx, conv, text, nil, replyMarkup)
	if err != nil {
		return 0, err
	}
	// Persist message_id before pinning so a failed pin does not duplicate messages on retry.
	state = pinnedStatusState{MessageID: msgID, Pinned: false}
	raw, _ := json.Marshal(state)
	if err := c.Repo.SaveCheckpoint(ctx, key, raw); err != nil {
		return msgID, err
	}
	if err := c.Transport.PinChatMessage(ctx, c.WorkspaceGroupID, msgID, true); err != nil {
		return msgID, err
	}
	state.Pinned = true
	raw, _ = json.Marshal(state)
	if err := c.Repo.SaveCheckpoint(ctx, key, raw); err != nil {
		return msgID, err
	}
	return msgID, nil
}
