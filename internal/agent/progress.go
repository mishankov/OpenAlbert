package agent

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"jarvis/internal/domain"
)

func (a *Activities) reportProgress(ctx context.Context, in ToolInput, text string) (any, error) {
	if in.Background || in.TaskID != "" {
		return nil, errors.New("progress updates are only available for an interactive user request")
	}
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) || !visibleMessageText(text) || utf8.RuneCountInString(text) > 600 {
		return nil, errors.New("progress must be visible plain text, at most 600 characters")
	}
	if err := a.Repo.ReportProgress(ctx, in.RunKey, in.Step, text); err != nil {
		return nil, err
	}
	return map[string]string{"state": "recorded", "note": "Updates one status message when due. Continue the task; send the final answer with send_message."}, nil
}

func (a *Activities) startProgress(ctx context.Context, in RunInput, conv domain.Conversation) {
	if in.TaskID != "" || in.Initiative || len(in.IncomingIDs) == 0 {
		return
	}
	if err := a.Repo.StartProgress(ctx, domain.Progress{TurnID: in.Key, Conversation: conv, StartedAt: in.StartedAt}); err != nil && a.Logger != nil {
		a.Logger.Warn("Progress could not be initialized", "operation", "progress.start", "turn_id", in.Key)
	}
}
