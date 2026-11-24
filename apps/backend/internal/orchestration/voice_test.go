package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
)

type voiceSynthesizer func(context.Context, string) ([]byte, error)

func (s voiceSynthesizer) Synthesize(ctx context.Context, text string) ([]byte, error) {
	return s(ctx, text)
}

func testVoiceAudio(t *testing.T) []byte {
	t.Helper()
	audio, err := os.ReadFile("../integrations/testdata/voice.ogg")
	if err != nil {
		t.Fatal(err)
	}
	return audio
}

func deliverVoiceQueue(t *testing.T, r *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	for range 3 {
		if err := r.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if r.voice != nil {
			select {
			case <-r.voice.done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
}

func TestVoiceReplyWorkflow(t *testing.T) {
	const answer = "Встреча завтра в полдень."
	for _, tc := range []struct {
		name     string
		conv     domain.Conversation
		steps    int
		codemode bool
	}{
		{"private", domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}, 4, false},
		{"private topic", domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true, ThreadID: 28}, 4, false},
		{"forum finalization", domain.Conversation{ID: "telegram:group:-11:topic:44", ChatID: -11, OwnerID: 7, ThreadID: 44}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a, repo, _, _ := newFixture(t)
			conv := tc.conv
			a.Codemode = tc.codemode
			var modelCalls atomic.Int32
			a.Model = codemodeTestModel(func(messages []domain.Message, tools []domain.Tool) domain.Message {
				if !strings.Contains(messages[0].Content, "A voice input alone is not a request for a voice reply") {
					t.Error("model lacked the explicit opt-in rule")
				}
				step := modelCalls.Add(1)
				if tc.codemode {
					if step == 1 {
						return calls(script("research", `return "ready"`))
					}
					return calls(script("reply", `return await tools.send_voice({text:"`+answer+`"})`))
				}
				if step > 1 {
					return finish("")
				}
				found := false
				for _, tool := range tools {
					found = found || tool.Name == "send_voice"
				}
				if !found {
					t.Error("voice reply tool unavailable")
				}
				return calls(call("reply", "send_voice", map[string]string{"text": answer}))
			})
			const incomingID = "telegram:8000"
			const messageID = 51
			if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: incomingID, Conversation: conv, Text: "Ответь голосом", TelegramMessageID: messageID, ReplyTo: &domain.ReplyContext{MessageID: 12}}, 8001); err != nil {
				t.Fatal(err)
			}
			if conv.Private && conv.ThreadID != 0 {
				otherTopic := conv
				otherTopic.ThreadID++
				if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "telegram:8001", Conversation: otherTopic, TelegramMessageID: 52}, 8002); err != nil {
					t.Fatal(err)
				}
			}
			env := newEnv(a)
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: incomingID}) }, 0)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: tc.steps})
			requireWorkflowOK(t, env)

			history, err := repo.ConversationHistory(ctx, conv.ID, "завтра в полдень", 0, 10)
			if err != nil || len(history) != 1 || history[0].Content != answer || len(history[0].ToolCalls) != 1 || history[0].ToolCalls[0].Name != "send_voice" {
				t.Fatalf("voice transcript missing from history: %+v %v", history, err)
			}
			var sent atomic.Int32
			realTTS := os.Getenv("OPENALBERT_TEST_TTS_BASE_URL")
			fixture := testVoiceAudio(t)
			telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent.Add(1)
				if !strings.HasSuffix(r.URL.Path, "/sendVoice") {
					t.Errorf("wrong Telegram method: %s", r.URL.Path)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					return
				}
				wantTopic := ""
				if conv.ThreadID != 0 {
					wantTopic = strconv.Itoa(conv.ThreadID)
				}
				var replyTo struct {
					MessageID int64 `json:"message_id"`
				}
				if r.FormValue("chat_id") != strconv.FormatInt(conv.ChatID, 10) || r.FormValue("message_thread_id") != wantTopic || json.Unmarshal([]byte(r.FormValue("reply_parameters")), &replyTo) != nil || replyTo.MessageID != messageID {
					t.Errorf("wrong Telegram route: %+v", r.MultipartForm.Value)
				}
				file, _, err := r.FormFile("voice")
				if err != nil {
					t.Error(err)
					return
				}
				defer file.Close()
				content, err := io.ReadAll(file)
				if err != nil || (realTTS == "" && !bytes.Equal(content, fixture)) || (realTTS != "" && (!bytes.HasPrefix(content, []byte("OggS")) || len(content) < 1000)) {
					t.Errorf("voice upload changed: %v", err)
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":65}}`)
			}))
			defer telegram.Close()
			tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "bot", BaseURL: telegram.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-11}})
			if err != nil {
				t.Fatal(err)
			}
			r := &Runtime{Repo: repo, Telegram: tg, Logger: a.Logger, TTS: voiceSynthesizer(func(_ context.Context, text string) ([]byte, error) {
				if text != answer {
					t.Errorf("TTS input = %q", text)
				}
				return fixture, nil
			})}
			if realTTS != "" {
				r.TTS, err = integrations.NewTTS(integrations.TTSConfig{BaseURL: realTTS, Model: "piper", Voice: "ru_RU-dmitri-medium"})
				if err != nil {
					t.Fatal(err)
				}
			}
			deliverVoiceQueue(t, r)
			if sent.Load() != 1 {
				t.Fatalf("sent %d Telegram messages, want one voice note", sent.Load())
			}
		})
	}
}

func TestVoiceReplyTextFallback(t *testing.T) {
	const answer = "Ответ с **буквальными знаками**."
	for _, tc := range []struct {
		name                       string
		synth                      voiceSynthesizer
		headerOnly                 bool
		status                     int
		voiceResponse, explanation string
	}{
		{name: "disabled", explanation: "Синтез речи отключён"},
		{name: "provider error", synth: func(context.Context, string) ([]byte, error) { return nil, errors.New("provider failed") }, explanation: "Не удалось синтезировать"},
		{name: "header-only provider response", headerOnly: true, explanation: "Не удалось синтезировать"},
		{name: "Telegram rejection", status: 403, voiceResponse: `{"ok":false,"error_code":403}`, explanation: "Telegram отклонил"},
		{name: "unknown acknowledgement", status: 502, voiceResponse: `{"ok":false,"error_code":429}`, explanation: "Не удалось подтвердить"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var voices, texts atomic.Int32
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "/sendVoice"):
					voices.Add(1)
					if tc.status == 0 {
						t.Error("Telegram called without synthesized audio")
						return
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.voiceResponse)
				case strings.HasSuffix(req.URL.Path, "/sendMessage"):
					texts.Add(1)
					var payload struct {
						Text string `json:"text"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || !strings.Contains(payload.Text, tc.explanation) || !strings.HasSuffix(payload.Text, answer) {
						t.Errorf("unexpected fallback: %q %v", payload.Text, err)
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":19}}`)
				default:
					t.Errorf("unexpected Telegram request: %s", req.URL.Path)
				}
			})
			var wantVoices int32
			if tc.status != 0 {
				wantVoices = 1
				fixture := testVoiceAudio(t)
				r.TTS = voiceSynthesizer(func(context.Context, string) ([]byte, error) { return fixture, nil })
			} else if tc.headerOnly {
				header := testVoiceAudio(t)[:47]
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(header) }))
				t.Cleanup(provider.Close)
				var err error
				r.TTS, err = integrations.NewTTS(integrations.TTSConfig{BaseURL: provider.URL, Model: "piper", Voice: "dmitri"})
				if err != nil {
					t.Fatal(err)
				}
			} else if tc.synth != nil {
				r.TTS = tc.synth
			}
			ctx := context.Background()
			if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "voice", Conversation: conv, VoiceReplyTo: 10, Text: answer}); err != nil {
				t.Fatal(err)
			}
			deliverVoiceQueue(t, r)
			if voices.Load() != wantVoices || texts.Load() != 1 {
				t.Fatalf("voice/text uploads = %d/%d, want %d/1", voices.Load(), texts.Load(), wantVoices)
			}
		})
	}
}

func TestSlowVoiceSynthesisDoesNotBlockOtherConversations(t *testing.T) {
	var texts atomic.Int32
	r, repo, private := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/sendRichMessage") {
			t.Errorf("unexpected request while synthesis is held: %s", req.URL.Path)
		}
		texts.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":19}}`)
	})
	started := make(chan struct{})
	r.TTS = voiceSynthesizer(func(ctx context.Context, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer func() { cancel(); r.Wait() }()
	group := domain.Conversation{ID: "telegram:group:-11:topic:44", ChatID: -11, OwnerID: 7, ThreadID: 44}
	if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "voice", Conversation: group, VoiceReplyTo: 10, Text: "Голосовой ответ"}); err != nil {
		t.Fatal(err)
	}
	for i := range 49 {
		enqueue(t, repo, group, fmt.Sprintf("next-%d", i), "Wait for the first group reply", "")
	}
	enqueue(t, repo, private, "text", "Deliver while the group waits", "")
	if err := r.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("synthesis did not start")
	}
	if texts.Load() != 1 || ctx.Err() != nil {
		t.Fatal("unrelated text delivery waited for synthesis")
	}
	if got := strings.Join(repo.states, ","); got != "text:sending,text:sent" {
		t.Fatalf("outgoing order changed while speech was pending: %s", got)
	}
}
