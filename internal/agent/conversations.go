package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"jarvis/internal/domain"
)

// Group metadata records the latest sender, not the installation owner.
// Privilege comes from launch configuration and the authenticated private chat.
func (a *Activities) isOwnerPrivate(c domain.Conversation) bool {
	return a.OwnerUserID > 0 && c.Private && c.OwnerID == a.OwnerUserID && c.ChatID == a.OwnerUserID
}

func (a *Activities) readableConversations(ctx context.Context, current domain.Conversation) ([]domain.Conversation, error) {
	out := []domain.Conversation{current}
	if !a.isOwnerPrivate(current) {
		return out, nil
	}
	all, err := a.Repo.ListConversations(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		// Other allowed users' private conversations remain private.
		if !c.Private && c.ID != current.ID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (a *Activities) readScope(ctx context.Context, current domain.Conversation, requested string) (string, error) {
	if requested == "" || requested == current.ID {
		return current.ID, nil
	}
	if a.isOwnerPrivate(current) {
		target, ok, err := a.Repo.Conversation(ctx, requested)
		if err != nil {
			return "", err
		}
		if ok && !target.Private {
			return target.ID, nil
		}
	}
	return "", errors.New("conversation is not readable from this chat")
}

func (a *Activities) searchMemories(ctx context.Context, current domain.Conversation, p toolArgs) ([]domain.MemoryEntry, error) {
	scopes := []domain.Conversation{current}
	if p.ConversationID != "" {
		target, err := a.readScope(ctx, current, p.ConversationID)
		if err != nil {
			return nil, err
		}
		scopes = []domain.Conversation{{ID: target}}
	} else if a.isOwnerPrivate(current) {
		var err error
		scopes, err = a.readableConversations(ctx, current)
		if err != nil {
			return nil, err
		}
	}
	out := []domain.MemoryEntry{}
	for _, c := range scopes {
		entries, err := a.Knowledge.Search(ctx, c.ID, p.Query)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}

type historyPage struct {
	Messages      []domain.HistoryMessage `json:"messages"`
	NextBeforeSeq int64                   `json:"next_before_seq,omitempty"`
}

func (a *Activities) conversationHistory(ctx context.Context, current domain.Conversation, p toolArgs) (historyPage, error) {
	out := historyPage{Messages: []domain.HistoryMessage{}}
	target, err := a.readScope(ctx, current, p.ConversationID)
	if err != nil {
		return out, err
	}
	if p.BeforeSeq < 0 || p.Limit < 0 || p.Limit > 50 {
		return out, errors.New("invalid history cursor or limit")
	}
	rows, err := a.Repo.ConversationHistory(ctx, target, p.Query, p.BeforeSeq, p.Limit)
	if err != nil {
		return out, err
	}
	// Keep valid structured JSON below the Activity result cap; page the remainder.
	for _, row := range rows {
		if len(row.Content) > 18000 {
			prefix := row.Content[:18000]
			for !utf8.ValidString(prefix) {
				prefix = prefix[:len(prefix)-1]
			}
			row.Content, row.Truncated = strings.TrimSuffix(prefix, "\n")+"\n[truncated]", true
		}
		candidate := append(out.Messages, row)
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return out, err
		}
		if len(encoded) > 24000 && len(out.Messages) > 0 {
			break
		}
		for len(encoded) > 24000 {
			runes := []rune(row.Content)
			if len(runes) < 2 {
				return out, errors.New("history metadata exceeds response limit")
			}
			row.Content, row.Truncated = string(runes[:len(runes)/2]), true
			candidate = []domain.HistoryMessage{row}
			encoded, err = json.Marshal(candidate)
			if err != nil {
				return out, err
			}
		}
		out.Messages = candidate
		out.NextBeforeSeq = row.Seq
	}
	return out, nil
}
