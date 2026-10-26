package integrations

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"openalbert/apps/backend/internal/domain"
	"path/filepath"
	"strings"
	"time"
)

type STTConfig struct {
	BaseURL, APIKey, Model, Revision string
	HTTPClient                       *http.Client
}
type STT struct {
	config STTConfig
	client *http.Client
}

func NewSTT(c STTConfig) (*STT, error) {
	base, err := providerURL(c.BaseURL)
	if err != nil {
		return nil, errors.New("STT_BASE_URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.Revision) == "" {
		return nil, errors.New("STT_MODEL and STT_REVISION are required")
	}
	c.BaseURL = base
	return &STT{config: c, client: httpClient(c.HTTPClient, 10*time.Minute)}, nil
}
func (s *STT) Profile() string {
	return s.config.BaseURL + "|" + s.config.Model + "|" + s.config.Revision
}
func (s *STT) Transcribe(ctx context.Context, name string, data []byte) (out domain.SpeechText, err error) {
	if len(data) == 0 || len(data) > 20<<20 {
		return out, errors.New("audio size is outside the allowed range")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err = w.WriteField("model", s.config.Model); err != nil {
		return
	}
	if err = w.WriteField("response_format", "json"); err != nil {
		return
	}
	p, err := w.CreateFormFile("file", filepath.Base(name))
	if err != nil {
		return out, err
	}
	if _, err = p.Write(data); err != nil {
		return out, err
	}
	if err = w.Close(); err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.BaseURL+"/audio/transcriptions", &body)
	if err != nil {
		return out, errors.New("invalid STT request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if s.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.config.APIKey)
	}
	var payload struct {
		domain.SpeechText
		Text *string `json:"text"`
	}
	err = request(ctx, s.client, req, &payload, 1<<20)
	if err != nil {
		return
	}
	if payload.Text == nil {
		return out, errors.New("STT response is missing text")
	}
	out = payload.SpeechText
	out.Text = *payload.Text
	return
}
