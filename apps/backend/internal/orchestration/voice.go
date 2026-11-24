package orchestration

import (
	"context"
	"errors"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

type voiceSynthesis struct {
	id       string
	done     chan struct{}
	audio    []byte
	fallback domain.VoiceFallback
}

// Only the delivery loop owns the slot. One in-flight synthesis keeps memory
// bounded while unrelated conversations and progress can continue sending.
func (r *Runtime) voiceReady(ctx context.Context, out domain.Outgoing) ([]byte, domain.VoiceFallback, bool) {
	if r.TTS == nil {
		return nil, domain.VoiceDisabled, true
	}
	if r.voice == nil {
		job := &voiceSynthesis{id: out.ID, done: make(chan struct{})}
		r.voice = job
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ctx := telemetry.WithFields(ctx, map[string]any{"conversation_id": out.Conversation.ID, "turn_id": out.TurnID, "delivery_id": out.ID})
			job.audio, job.fallback = r.synthesizeVoice(ctx, out.Text)
			close(job.done)
		}()
	}
	if r.voice.id != out.ID {
		return nil, "", false
	}
	select {
	case <-r.voice.done:
		job := r.voice
		r.voice = nil
		return job.audio, job.fallback, true
	default:
		return nil, "", false
	}
}

func (r *Runtime) synthesizeVoice(ctx context.Context, text string) ([]byte, domain.VoiceFallback) {
	event := telemetry.Start(ctx, r.Logger, "tts.synthesize")
	audio, err := r.TTS.Synthesize(event.Context(), text)
	if err == nil && len(audio) == 0 {
		err = errors.New("TTS returned empty audio")
	}
	event.End(err)
	if err != nil {
		return nil, domain.VoiceSynthesisFailed
	}
	return audio, ""
}

func voiceFallbackText(reason domain.VoiceFallback) string {
	switch reason {
	case domain.VoiceDisabled:
		return "Синтез речи отключён. Ответ текстом."
	case domain.VoiceSynthesisFailed:
		return "Не удалось синтезировать голосовой ответ. Ответ текстом."
	case domain.VoiceRejected:
		return "Telegram отклонил голосовое сообщение. Ответ текстом."
	default:
		return "Не удалось подтвердить отправку голосового сообщения. Ответ текстом."
	}
}
