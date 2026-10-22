package orchestration

import (
	"context"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"io"
	"jarvis/internal/agent"
	"jarvis/internal/domain"
	"jarvis/internal/integrations"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type workflowSpeech struct {
	calls int
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
	env := newEnv(a)
	env.ExecuteWorkflow(func(ctx workflow.Context) (agent.Turn, error) {
		return runTurn(options(ctx), agent.RunInput{Key: "http", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"voice"}}, 2)
	})
	requireWorkflowOK(t, env)
}

func (s *workflowSpeech) Profile() string { return "test-v1" }
func (s *workflowSpeech) Transcribe(ctx context.Context, _ string, _ []byte) (domain.SpeechText, error) {
	s.calls++
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
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "voice", Conversation: conv, Attachments: []domain.Attachment{{Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("audio")}}}, 2)
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "clarification", Conversation: conv, Text: "Точнее, три яблока"}, 3)
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
	env := newEnv(a)
	sent := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "TranscribeAudio" && !sent {
			sent = true
			env.SignalWorkflow(MessageSignal, Event{ID: "clarification"})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "voice"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 3})
	requireWorkflowOK(t, env)
	if speech.calls != 1 || !sent {
		t.Fatalf("speech calls=%d", speech.calls)
	}
}

func TestStopDuringAudioCancelsWithoutModelOrResurrection(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.STT = &workflowSpeech{stop: true}
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "voice", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", Data: []byte("x")}}}, 2)
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "stop-audio", Conversation: conv, Text: "/stop"}, 3)
	model := &scriptedModel{run: func(_ int, _ []domain.Message) (domain.Message, error) {
		t.Error("cancelled audio reached LLM")
		return finish("wrong"), nil
	}}
	a.Model = model
	env := newEnv(a)
	sent := false
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == "TranscribeAudio" && !sent {
			sent = true
			env.SignalWorkflow(MessageSignal, Event{ID: "stop-audio", Stop: true})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "voice"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 3})
	requireWorkflowOK(t, env)
	pending, _ := repo.PendingIncoming(context.Background(), 100)
	for _, m := range pending {
		if m.ID == "voice" || m.ID == "stop-audio" {
			t.Fatal("cancelled input resurrected")
		}
	}
	if model.count() != 0 {
		t.Fatal("model was called")
	}
}

func TestPermanentSTTFailureFallsBackAndLegacyTurnReplays(t *testing.T) {
	for _, legacy := range []bool{false, true} {
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
		env := newEnv(a)
		if legacy {
			env.OnGetVersion("audio-preparation/turn", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
		} else {
			env.OnActivity("TranscribeAudio", mock.Anything, mock.Anything).Return(domain.AudioResult{}, temporal.NewNonRetryableApplicationError("bad audio", "stt_http_422", nil)).Once()
		}
		env.ExecuteWorkflow(func(ctx workflow.Context) (agent.Turn, error) {
			return runTurn(options(ctx), agent.RunInput{Key: "turn", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"voice"}}, 2)
		})
		requireWorkflowOK(t, env)
	}
}
