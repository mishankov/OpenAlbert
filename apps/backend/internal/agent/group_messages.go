package agent

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

func (a *Activities) sendGroupMessage(ctx context.Context, in ToolInput, p toolArgs, key string, now time.Time) (any, error) {
	if !a.isOwnerPrivate(in.Conversation) || in.Background || in.TaskID != "" {
		return nil, errors.New("group posting is only allowed from the owner's interactive private chat")
	}
	if p.ConversationID == "" {
		return nil, errors.New("conversation_id is required; select a group or topic from conversation_list")
	}
	target, found, err := a.Repo.Conversation(ctx, p.ConversationID)
	if err != nil {
		return nil, err
	}
	if !found || target.Private || target.ChatID >= 0 || target.ThreadID < 0 {
		return nil, errors.New("target must be a known Telegram group or topic")
	}
	allowed := false
	for _, id := range a.AllowedGroupIDs {
		allowed = allowed || id == target.ChatID
	}
	if !allowed {
		return nil, errors.New("group is not in TELEGRAM_ALLOWED_GROUP_IDS")
	}
	if strings.TrimSpace(p.Text) == "" || strings.TrimFunc(p.Text, func(r rune) bool { return unicode.IsSpace(r) || unicode.In(r, unicode.Cf) }) == "" {
		return nil, errors.New("group message must not be empty")
	}
	if !utf8.ValidString(p.Text) || len(p.Text) > 32000 {
		return nil, errors.New("group message must be valid UTF-8 and at most 32000 bytes")
	}
	if p.ArtifactID != "" {
		return nil, errors.New("send_group_message supports text only")
	}
	// Group OwnerID is the latest sender; delivery is authorized by the owner who requested this post.
	target.OwnerID = a.OwnerUserID
	out := domain.Outgoing{ID: key + "/group", Conversation: target, Text: p.Text, State: "pending", RecordInHistory: true,
		TurnID: in.RunKey, SessionID: in.SessionID, TaskID: in.TaskID, Trigger: in.TriggerName(), StartedAt: in.StartedAt,
		ReceivedAt: in.ReceivedAt, EnqueuedAt: now, PartCount: 1}
	if err := a.Repo.EnqueueOutgoing(ctx, out); err != nil {
		return nil, err
	}
	return map[string]string{"delivery_id": out.ID, "state": "queued", "conversation_id": target.ID}, nil
}
