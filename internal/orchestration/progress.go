package orchestration

import (
	"context"
	"errors"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/integrations"
)

// This runs in the same loop as final deliveries: an in-flight status creation
// cannot arrive after the final reply. All network calls have a short timeout.
func (r *Runtime) ProgressOnce(ctx context.Context, now time.Time) error {
	rows, err := r.Repo.DueProgress(ctx, now, 50)
	if err != nil {
		return err
	}
	for _, row := range rows {
		next := row.Delivery
		next.State = "sending"
		p, claimed, err := r.Repo.SetProgressDelivery(ctx, row.TurnID, row.Delivery.Version, next)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		if err = r.deliverProgress(ctx, p, now); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) deliverProgress(ctx context.Context, p domain.Progress, now time.Time) error {
	next := p.Delivery
	next.State = "ready"
	next.RetryAt = time.Time{}
	text := p.Text
	terminal := p.Outcome != "active" && p.Outcome != "waiting"
	switch p.Outcome {
	case "cancelled":
		text = "Остановлено."
	case "failed":
		text = "Не удалось завершить запрос."
	case "delivery_failed":
		text = "Не удалось подтвердить доставку всего ответа."
	}
	var err error
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	switch {
	case p.Outcome == "waiting": // Reply queued or Finish raced with the claim.
	case terminal && next.MessageID == "":
		next.State = "done" // A fast request never gets a status, including failures.
	case p.Outcome == "delivered":
		err = r.Telegram.DeleteProgress(c, p.Conversation, next.MessageID)
		if err == nil {
			next.State = "done"
		}
	case next.MessageID == "":
		next.MessageID, err = r.Telegram.SendProgress(c, p.Conversation, text)
		if err == nil {
			next.LastText = text
			next.UpdatedAt = now
		}
	case text == next.LastText:
		next.UpdatedAt = now
		if terminal {
			next.State = "done"
		}
	default:
		err = r.Telegram.EditProgress(c, p.Conversation, next.MessageID, text)
		if err == nil {
			next.LastText = text
			next.UpdatedAt = now
			if terminal {
				next.State = "done"
			}
		}
	}
	cancel()
	if err != nil {
		var apiErr *integrations.TelegramError
		switch {
		case errors.As(err, &apiErr) && apiErr.Code == 429:
			delay := max(5*time.Second, apiErr.RetryAfter)
			next.RetryAt = now.Add(delay)
		case next.MessageID == "":
			// The first send might have succeeded. Never create a replacement.
			next.State = "disabled"
		case errors.As(err, &apiErr) && apiErr.Code < 500:
			next.State = "disabled" // Deleted/uneditable status: do not recreate it.
		default:
			next.RetryAt = now.Add(15 * time.Second) // Repeating an edit/delete uses the same ID.
		}
		r.record(ctx, "telegram.progress", err)
	}
	// Preserve the acknowledgement even on shutdown. A failed save leaves a
	// durable sending marker for the restart recovery rules.
	save, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer done()
	_, _, saveErr := r.Repo.SetProgressDelivery(save, p.TurnID, p.Delivery.Version, next)
	return saveErr
}
