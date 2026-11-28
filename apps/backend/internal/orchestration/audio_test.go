package orchestration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
)

type workflowSpeech struct {
	calls atomic.Int32
	stop  bool
}

func TestConfiguredSTTEndpointFeedsAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer stt-only" {
			t.Error("incorrect independent endpoint")
		}
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			t.Error(e)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("model") != "nemotron-3.5" || r.FormValue("response_format") != "json" {
			t.Error("incorrect speech request")
		}
		_, _ = io.WriteString(w, `{"text":"Не покупай яблоки"}`)
	}))
	defer server.Close()
	a, repo, _, conv := newFixture(t)
	speech, e := integrations.NewSTT(integrations.STTConfig{BaseURL: server.URL + "/v1", APIKey: "stt-only", Model: "nemotron-3.5", Revision: "v1"})
	if e != nil {
		t.Fatal(e)
	}
	a.STT = speech
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "voice", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", Data: []byte("opus")}}}, 2)
	a.Model = &scriptedModel{run: func(_ int, msg []domain.Message) (domain.Message, error) {
		found := false
		for _, m := range msg {
			found = found || strings.Contains(m.Content, "Не покупай яблоки")
		}
		if !found {
			t.Error("HTTP transcript did not reach LLM")
		}
		return finish("понял"), nil
	}}
	r := newTestRuntime(a, nil, time.Second, 2)
	if _, err := r.runTurn(context.Background(), agent.RunInput{Key: "http", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"voice"}}, 2); err != nil {
		t.Fatal(err)
	}
}

func (s *workflowSpeech) Profile() string { return "test-v1" }
func (s *workflowSpeech) Transcribe(ctx context.Context, _ string, _ []byte) (domain.SpeechText, error) {
	s.calls.Add(1)
	if s.stop {
		<-ctx.Done()
		return domain.SpeechText{}, ctx.Err()
	}
	return domain.SpeechText{Text: "Купи два яблока"}, nil
}

func TestNewTextDuringAudioIsIncludedBeforeModel(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	speech := &workflowSpeech{}
	a.STT = speech
	a.STTModel = "test"
	ctx := context.Background()
	_, _ = repo.AcceptIncoming(ctx, domain.Incoming{ID: "voice", Conversation: conv, Attachments: []domain.Attachment{{Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("audio")}}}, 2)
	_, _ = repo.AcceptIncoming(ctx, domain.Incoming{ID: "clarification", Conversation: conv, Text: "Точнее, три яблока"}, 3)
	a.Model = &scriptedModel{run: func(_ int, msg []domain.Message) (domain.Message, error) {
		all := ""
		for _, m := range msg {
			all += m.Content
		}
		if !strings.Contains(all, "Купи два яблока") || !strings.Contains(all, "Точнее, три яблока") {
			t.Error("model missed transcript or clarification")
		}
		return finish("готово"), nil
	}}
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 3)
	var sent atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, _ any) {
			if name == "TranscribeAudio" && sent.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "clarification"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "voice"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if speech.calls.Load() != 1 || !sent.Load() {
		t.Fatalf("speech calls=%d", speech.calls.Load())
	}
}

func TestStopDuringAudioCancelsWithoutModelOrResurrection(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.STT = &workflowSpeech{stop: true}
	ctx := context.Background()
	_, _ = repo.AcceptIncoming(ctx, domain.Incoming{ID: "voice", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", Data: []byte("x")}}}, 2)
	_, _ = repo.AcceptIncoming(ctx, domain.Incoming{ID: "stop-audio", Conversation: conv, Text: "/stop"}, 3)
	model := &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		t.Error("cancelled audio reached LLM")
		return finish("wrong"), nil
	}}
	a.Model = model
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 3)
	var sent atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, _ any) {
			if name == "TranscribeAudio" && sent.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "stop-audio", Stop: true})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "voice"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	pending, _ := repo.PendingIncoming(ctx, 100)
	for _, m := range pending {
		if m.ID == "voice" || m.ID == "stop-audio" {
			t.Fatal("cancelled input resurrected")
		}
	}
	if model.count() != 0 {
		t.Fatal("model was called")
	}
}

func TestPermanentSTTFailureFallsBack(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "voice", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", Data: []byte("x")}}}, 2)
	a.STT = &workflowSpeech{}
	a.Model = &scriptedModel{run: func(_ int, msg []domain.Message) (domain.Message, error) {
		all := ""
		for _, m := range msg {
			all += m.Content
		}
		if !strings.Contains(all, "Audio transcription unavailable") {
			t.Error("missing fallback")
		}
		return finish("нужен текст"), nil
	}}
	r := newTestRuntime(a, nil, time.Second, 2)
	r.hooks = &activityHooks{
		transcribe: func(context.Context, agent.AudioInput, func(context.Context, agent.AudioInput) (domain.AudioResult, error)) (domain.AudioResult, error) {
			return domain.AudioResult{}, domain.NewNonRetryableError("bad audio", "stt_http_422", nil)
		},
	}
	if _, err := r.runTurn(context.Background(), agent.RunInput{Key: "turn", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"voice"}}, 2); err != nil {
		t.Fatal(err)
	}
}
