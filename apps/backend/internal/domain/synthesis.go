package domain

import "context"

// MaxVoiceReplyRunes bounds the text synthesized for one Telegram voice note.
const MaxVoiceReplyRunes = 1000

// SpeechSynthesizer returns Ogg/Opus audio independently of the chat model.
type SpeechSynthesizer interface {
	Synthesize(context.Context, string) ([]byte, error)
}
