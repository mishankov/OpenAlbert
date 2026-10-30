package agent

import (
	"context"
	"errors"

	"openalbert/apps/backend/internal/domain"
)

func (a *Activities) groupRulesAccess(in ToolInput) error {
	if !a.isOwnerPrivate(in.Conversation) || in.Background || in.TaskID != "" {
		return errors.New("GROUP.md tools require the owner's interactive private conversation")
	}
	return nil
}

func (a *Activities) groupRulesTarget(ctx context.Context, in ToolInput, conversationID string) (domain.Conversation, error) {
	if err := a.groupRulesAccess(in); err != nil {
		return domain.Conversation{}, err
	}
	if conversationID == "" {
		return domain.Conversation{}, errors.New("select an exact conversation_id from conversation_list")
	}
	conv, found, err := a.Repo.Conversation(ctx, conversationID)
	if err != nil {
		return domain.Conversation{}, err
	}
	if !found || conv.Private || conv.ChatID >= 0 || conv.ThreadID < 0 {
		return domain.Conversation{}, errors.New("target must be a known Telegram group or topic")
	}
	for _, id := range a.AllowedGroupIDs {
		if id == conv.ChatID {
			return conv, nil
		}
	}
	return domain.Conversation{}, errors.New("group is not in TELEGRAM_ALLOWED_GROUP_IDS")
}
