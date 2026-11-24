package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

const voiceAudioLimit = 4 << 20

type TTSConfig struct {
	BaseURL, APIKey, Model, Voice string
	HTTPClient                    *http.Client
}

type TTS struct {
	config TTSConfig
	client *http.Client
}

func NewTTS(c TTSConfig) (*TTS, error) {
	base, err := providerURL(c.BaseURL)
	if err != nil || strings.ContainsAny(c.BaseURL, "?#") {
		return nil, errors.New("TTS_BASE_URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	c.Model, c.Voice = strings.TrimSpace(c.Model), strings.TrimSpace(c.Voice)
	if c.Model == "" || c.Voice == "" {
		return nil, errors.New("TTS_MODEL and TTS_VOICE are required")
	}
	c.BaseURL = base
	return &TTS{config: c, client: httpClient(c.HTTPClient, 30*time.Second)}, nil
}

func (s *TTS) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > domain.MaxVoiceReplyRunes {
		return nil, errors.New("TTS input is empty or exceeds the voice reply limit")
	}
	payload, err := json.Marshal(map[string]string{
		"input": text, "model": s.config.Model, "voice": s.config.Voice, "response_format": "opus",
	})
	if err != nil {
		return nil, errors.New("invalid TTS request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.BaseURL+"/audio/speech", bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("invalid TTS request")
	}
	req.Header.Set("Content-Type", "application/json")
	if s.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.config.APIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("TTS request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{Source: "TTS", Status: resp.StatusCode}
	}
	audio, err := limitedRead(resp.Body, voiceAudioLimit)
	if err != nil {
		return nil, err
	}
	if !validOggOpus(audio) {
		return nil, errors.New("TTS response is not complete Ogg/Opus audio")
	}
	return audio, nil
}

var _ domain.SpeechSynthesizer = (*TTS)(nil)
