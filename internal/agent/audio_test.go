package agent

import (
	"context"
	"errors"
	"go.temporal.io/sdk/temporal"
	"jarvis/internal/domain"
	"jarvis/internal/integrations"
	"strings"
	"testing"
	"time"
)

type fakeSpeech struct {
	profile string
	calls   int
	run     func(context.Context) (domain.SpeechText, error)
}

type unavailableAttachmentMessenger struct{ testMessenger }

func (unavailableAttachmentMessenger) Download(context.Context, string) ([]byte, error) {
	return nil, errors.New("download failed")
}
func TestNonAudioAttachmentDownloadFailureStillPreservesMessageText(t *testing.T) {
	a, repo, _, conv := fixture(t)
	a.Messenger = unavailableAttachmentMessenger{}
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "file", Conversation: conv, Text: "Текст сообщения", Attachments: []domain.Attachment{{Name: "note.txt", MIME: "text/plain", FileID: "missing"}}}, 2)
	_, e := a.Prepare(context.Background(), RunInput{Key: "r", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"file"}})
	if e != nil {
		t.Fatal(e)
	}
	messages, _ := repo.Messages(context.Background(), "s")
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "Текст сообщения") || !strings.Contains(messages[0].Content, "Attachment unavailable") {
		t.Fatal("attachment failure dropped message")
	}
}

func (s *fakeSpeech) Profile() string { return s.profile }
func (s *fakeSpeech) Transcribe(ctx context.Context, _ string, _ []byte) (domain.SpeechText, error) {
	s.calls++
	if s.run != nil {
		return s.run(ctx)
	}
	return domain.SpeechText{Text: "Позвони Анне завтра в 15:00", DurationSeconds: 3}, nil
}

func TestAudioCacheSurvivesPrepareSessionsAndPreservesGroupAuthor(t *testing.T) {
	a, repo, _, _ := fixture(t)
	spy := &tracedRepo{Repository: repo}
	a.Repo = spy
	speech := &fakeSpeech{profile: "weights-v1"}
	a.STT = speech
	a.STTModel = "nemotron"
	ctx := context.Background()
	conv := domain.Conversation{ID: "telegram:group:-20:topic:3", ChatID: -20, ThreadID: 3, OwnerID: 42}
	accept := func(id string) {
		t.Helper()
		_, e := repo.AcceptIncoming(ctx, domain.Incoming{ID: id, Conversation: conv, AuthorName: "Анна", Attachments: []domain.Attachment{{Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("same-audio")}}}, 2)
		if e != nil {
			t.Fatal(e)
		}
	}
	accept("voice1")
	in := RunInput{Key: "run", SessionID: "session", ConversationID: conv.ID, IncomingIDs: []string{"voice1"}}
	jobs, e := a.PrepareAudio(ctx, in)
	if e != nil || len(jobs) != 1 {
		t.Fatalf("jobs %+v %v", jobs, e)
	}
	art, ok, e := repo.Artifact(ctx, conv.ID, jobs[0].ArtifactID)
	if e != nil || !ok || string(art.Data) != "same-audio" || art.SessionID != "session" {
		t.Fatal("original missing before STT")
	}
	r, e := a.TranscribeAudio(ctx, jobs[0])
	if e != nil || r.AuthorID != 42 || r.AuthorName != "Анна" || r.Status != "completed" {
		t.Fatalf("result %+v %v", r, e)
	}
	if _, e = a.TranscribeAudio(ctx, jobs[0]); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Prepare(ctx, in); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Prepare(ctx, in); e != nil {
		t.Fatal(e)
	}
	messages, _ := repo.Messages(ctx, in.SessionID)
	if len(messages) != 1 || messages[0].AuthorID != 42 || messages[0].SourceMessageID != "voice1" || !strings.Contains(messages[0].Content, r.Text) {
		t.Fatalf("author or transcript lost: %+v", messages)
	}
	accept("voice2")
	in.SessionID = "another-session"
	in.IncomingIDs = []string{"voice2"}
	jobs, e = a.PrepareAudio(ctx, in)
	if e != nil || len(jobs) != 1 {
		t.Fatal(e)
	}
	r, e = a.TranscribeAudio(ctx, jobs[0])
	if e != nil || !r.Cached || speech.calls != 1 || len(spy.traces) != 2 {
		t.Fatalf("cache made a new call: %+v %v calls=%d traces=%d", r, e, speech.calls, len(spy.traces))
	}
	// A changed processing profile must not reuse previous weights' result.
	speech.profile = "weights-v2"
	accept("voice3")
	in.IncomingIDs = []string{"voice3"}
	jobs, _ = a.PrepareAudio(ctx, in)
	_, e = a.TranscribeAudio(ctx, jobs[0])
	if e != nil || speech.calls != 2 {
		t.Fatal("profile did not invalidate cache")
	}
	conv.ID = "personal:42"
	conv.Private = true
	conv.ChatID = 42
	accept("private-voice")
	in.ConversationID = conv.ID
	in.IncomingIDs = []string{"private-voice"}
	jobs, _ = a.PrepareAudio(ctx, in)
	_, e = a.TranscribeAudio(ctx, jobs[0])
	if e != nil || speech.calls != 3 {
		t.Fatal("cache crossed conversation scope")
	}
}

func TestAudioErrorClassificationAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		err      error
		nonretry bool
	}{{&integrations.HTTPError{Status: 422}, true}, {&integrations.HTTPError{Status: 401}, true}, {&integrations.HTTPError{Status: 503}, false}, {errors.New("PRIVATE_BODY"), false}} {
		err := speechError(context.Background(), tc.err)
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || app.NonRetryable() != tc.nonretry || strings.Contains(err.Error(), "PRIVATE_BODY") {
			t.Fatalf("classification %v", err)
		}
	}
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	_, _ = repo.AcceptIncoming(ctx, domain.Incoming{ID: "audio", Conversation: conv, Attachments: []domain.Attachment{{Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("x")}}}, 2)
	c, cancel := context.WithCancel(ctx)
	a.STT = &fakeSpeech{profile: "v1", run: func(ctx context.Context) (domain.SpeechText, error) { cancel(); return domain.SpeechText{}, ctx.Err() }}
	in := RunInput{Key: "cancel", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"audio"}}
	jobs, _ := a.PrepareAudio(ctx, in)
	if _, e := a.TranscribeAudio(c, jobs[0]); !errors.Is(e, context.Canceled) {
		t.Fatalf("not cancelled: %v", e)
	}
	if e := a.RecordAudioFailure(ctx, AudioFailure{Input: jobs[0], Code: "cancelled"}); e != nil {
		t.Fatal(e)
	}
	r, _, _ := a.loadAudio(ctx, "s", "audio", jobs[0].ArtifactID)
	if r.Status != "cancelled" {
		t.Fatalf("lost cancellation %+v", r)
	}
	// The cancelled owner released its lease; another message can retry safely.
	a.STT = &fakeSpeech{profile: "v1"}
	_, _ = repo.AcceptIncoming(ctx, domain.Incoming{ID: "retry-audio", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", Data: []byte("x")}}}, 3)
	in.IncomingIDs = []string{"retry-audio"}
	jobs, _ = a.PrepareAudio(ctx, in)
	if _, e := a.TranscribeAudio(ctx, jobs[0]); e != nil {
		t.Fatal(e)
	}
}

func TestAudioDisabledAndMetadataLimitKeepOriginal(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		a, repo, _, conv := fixture(t)
		if !disabled {
			a.STT = &fakeSpeech{profile: "v1"}
		}
		_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "audio", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", DurationSeconds: 301, Data: []byte("x")}}}, 2)
		in := RunInput{Key: "r", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"audio"}}
		jobs, e := a.PrepareAudio(context.Background(), in)
		if e != nil || len(jobs) != 0 {
			t.Fatal("invalid audio scheduled")
		}
		r, ok, e := a.loadAudio(context.Background(), "s", "audio", ID("audio", "0"))
		if e != nil || !ok || r.ErrorCode == "" {
			t.Fatal("missing failure projection")
		}
	}
}

func TestTranscriptionRetryAfterProjectionWriteFailureUsesDurableCache(t *testing.T) {
	a, repo, _, conv := fixture(t)
	speech := &fakeSpeech{profile: "v1"}
	a.STT = speech
	_, _ = repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "audio", Conversation: conv, Attachments: []domain.Attachment{{Name: "v.ogg", MIME: "audio/ogg", Data: []byte("x")}}}, 2)
	jobs, _ := a.PrepareAudio(context.Background(), RunInput{Key: "r", SessionID: "s", ConversationID: conv.ID, IncomingIDs: []string{"audio"}})
	// Simulate a crash after durable cache commit but before attachment projection.
	_, e := a.TranscribeAudio(context.Background(), jobs[0])
	if e != nil {
		t.Fatal(e)
	}
	_ = a.saveAudio(context.Background(), domain.AudioResult{SessionID: "s", IncomingID: "audio", ArtifactID: jobs[0].ArtifactID, Status: "running", QueuedAt: time.Now()})
	r, e := a.TranscribeAudio(context.Background(), jobs[0])
	if e != nil || !r.Cached || speech.calls != 1 {
		t.Fatal("recomputed cached audio after crash")
	}
}
