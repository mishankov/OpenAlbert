package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"openalbert/internal/domain"
)

// Delivery is a property of this turn, never of the shared conversation.
func (a *Activities) replyConversation(ctx context.Context, conv domain.Conversation, ids []string) (domain.Conversation, error) {
	conv.DeliveryChannel = ""
	for i := len(ids) - 1; i >= 0; i-- {
		in, ok, err := a.Repo.Incoming(ctx, ids[i])
		if err != nil {
			return conv, err
		}
		if !ok {
			continue
		}
		if in.Conversation.ID != conv.ID {
			return conv, fmt.Errorf("incoming scope mismatch")
		}
		if in.Source == "desktop" {
			conv.DeliveryChannel = "desktop"
		}
		break
	}
	return conv, nil
}
func (a *Activities) failureConversation(ctx context.Context, conv domain.Conversation, key string) (domain.Conversation, error) {
	raw, ok, err := a.Repo.LoadCheckpoint(ctx, "turn-done/"+key)
	if err != nil {
		return conv, err
	}
	if !ok {
		return conv, nil
	}
	var turn Turn
	if err = json.Unmarshal(raw, &turn); err != nil {
		return conv, err
	}
	return a.replyConversation(ctx, conv, turn.Input.IncomingIDs)
}
