package desktopapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

func fixtureM4A(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/silence.m4a")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func box(kind string, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	copy(out[4:8], kind)
	copy(out[8:], body)
	return out
}
func mediaTrack(kind string) []byte {
	handler := make([]byte, 12)
	copy(handler[8:], kind)
	return box("trak", box("mdia", box("hdlr", handler)))
}
func mp4Fixture(tracks ...string) []byte {
	brand := append([]byte("M4A \x00\x00\x00\x00"), []byte("mp42isom")...)
	out := box("ftyp", brand)
	var contents []byte
	for _, kind := range tracks {
		contents = append(contents, mediaTrack(kind)...)
	}
	out = append(out, box("moov", contents)...)
	return append(out, box("mdat", []byte{1, 2, 3, 4})...)
}

func TestM4AMIMEUsesAudioTracksRatherThanClaimedTypeOrExtension(t *testing.T) {
	real := fixtureM4A(t)
	if got := http.DetectContentType(real); got != "video/mp4" {
		t.Fatalf("fixture must exercise standard sniffer fallback, got %s", got)
	}
	for _, claimed := range []string{"audio/mp4", "video/mp4", "application/octet-stream", "audio/mp4; codecs=mp4a.40.2", ""} {
		mime, err := attachmentMIME(real, claimed)
		if err != nil || mime != "audio/mp4" {
			t.Fatalf("real M4A type %q: %s %v", claimed, mime, err)
		}
	}
	video := mp4Fixture("soun", "vide")
	if mime, err := attachmentMIME(video, "video/mp4"); err != nil || mime != "video/mp4" {
		t.Fatal("real video mislabeled", mime, err)
	}
	for name, data := range map[string][]byte{"video": video, "opaque": {0, 1, 2, 3}, "header only": box("ftyp", []byte("M4A \x00\x00\x00\x00mp42isom")), "missing handler": mp4Fixture("meta"), "truncated": real[:len(real)-1], "forged suffix": append([]byte("not m4a"), real...), "invalid short box": {0, 0, 0, 1, 'f', 't', 'y', 'p'}, "invalid size": {255, 255, 255, 255, 'f', 't', 'y', 'p'}} {
		t.Run(name, func(t *testing.T) {
			if _, err := attachmentMIME(data, "audio/mp4"); err == nil {
				t.Fatal("invalid/audio-video container accepted as voice")
			}
		})
	}
	// Extended and parent-sized boxes must stay bounded and parse deterministically.
	extended := make([]byte, 16)
	binary.BigEndian.PutUint32(extended, 1)
	copy(extended[4:8], "free")
	binary.BigEndian.PutUint64(extended[8:], 16)
	if !audioMP4(append(extended, mp4Fixture("soun")...)) {
		t.Fatal("valid extended-size box rejected")
	}
	parentSized := mp4Fixture("soun")
	// The last mdat consumes the remainder of its parent when its size is zero.
	binary.BigEndian.PutUint32(parentSized[len(parentSized)-12:len(parentSized)-8], 0)
	if !audioMP4(parentSized) {
		t.Fatal("valid parent-sized box rejected")
	}
	malformed := append([]byte(nil), extended...)
	binary.BigEndian.PutUint64(malformed[8:], ^uint64(0))
	if audioMP4(append(malformed, real...)) {
		t.Fatal("overflowing box accepted")
	}
}

func uploadMedia(t *testing.T, h http.Handler, data []byte, mime, token string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="voice-fixture.m4a"`)
	header.Set("Content-Type", mime)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/attachments", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type voiceSpeech struct {
	data  []byte
	name  string
	calls int
}

func (s *voiceSpeech) Profile() string { return "desktop-voice-fixture" }
func (s *voiceSpeech) Transcribe(ctx context.Context, name string, data []byte) (domain.SpeechText, error) {
	s.calls++
	s.name = name
	s.data = append([]byte(nil), data...)
	return domain.SpeechText{Text: "Распознанное поручение", DurationSeconds: 0.1}, nil
}

func TestM4AUploadDispatchesToExistingSTTAndRetainsUUIDRetry(t *testing.T) {
	repo := storage.NewMemory()
	api := newAPI(t, repo)
	h := api.Handler()
	audio := fixtureM4A(t)
	ctx := context.Background()
	w := uploadMedia(t, h, audio, "audio/mp4", testToken)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	uploaded := decode[struct {
		Attachment Attachment `json:"attachment"`
	}](t, w).Attachment
	if uploaded.MIME != "audio/mp4" || uploaded.SizeBytes != int64(len(audio)) {
		t.Fatal("invalid audio metadata", uploaded)
	}
	uuid := id()
	body := fmt.Sprintf(`{"client_message_id":%q,"text":"Сделай это","attachment_ids":[%q]}`, uuid, uploaded.ID)
	for i := 0; i < 2; i++ {
		w = call(t, h, "POST", "/api/v1/messages", body)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	incoming, ok, err := repo.Incoming(ctx, "desktop:"+uuid)
	if err != nil || !ok || len(incoming.Attachments) != 1 || incoming.Attachments[0].Kind != "audio" || incoming.Attachments[0].ArtifactID != uploaded.ID {
		t.Fatal("voice inbox lost routing metadata", incoming, err)
	}
	speech := &voiceSpeech{}
	// Messenger intentionally nil: uploaded artifacts must never use Telegram Download.
	activities := &agent.Activities{Repo: repo, Knowledge: api.Memory.(domain.Knowledge), STT: speech, STTModel: "fixture", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	run := agent.RunInput{Key: "voice-run", SessionID: "voice-session", ConversationID: api.scope(), IncomingIDs: []string{incoming.ID}}
	jobs, err := activities.PrepareAudio(ctx, run)
	if err != nil || len(jobs) != 1 || jobs[0].ArtifactID != uploaded.ID {
		t.Fatal("M4A did not enter existing STT", jobs, err)
	}
	for i := 0; i < 2; i++ {
		result, err := activities.TranscribeAudio(ctx, jobs[0])
		if err != nil || result.Text != "Распознанное поручение" {
			t.Fatal("STT result", result, err)
		}
	}
	if speech.calls != 1 || speech.name != "voice-fixture.m4a" || !bytes.Equal(speech.data, audio) {
		t.Fatal("audio changed or STT duplicated")
	}
	turn, err := activities.Prepare(ctx, run)
	if err != nil || turn.Conversation.DeliveryChannel != "desktop" {
		t.Fatal("voice response lost desktop route", err)
	}
	internalMessages, err := repo.Messages(ctx, run.SessionID)
	if err != nil || len(internalMessages) != 1 || !strings.Contains(internalMessages[0].Content, "Распознанное поручение") {
		t.Fatal("transcript did not reach agent", internalMessages, err)
	}
	timeline := call(t, h, "GET", "/api/v1/messages", "")
	if timeline.Code != 200 || strings.Contains(timeline.Body.String(), "Распознанное поручение") || strings.Contains(timeline.Body.String(), "base64") || !strings.Contains(timeline.Body.String(), `"mime":"audio/mp4"`) {
		t.Fatal("public history leaked audio processing or lost playable file", timeline.Body.String())
	}
	page := decode[struct {
		Messages []Message `json:"messages"`
	}](t, timeline)
	if len(page.Messages) != 1 {
		t.Fatal("UUID retry duplicated voice message")
	}
	w = call(t, h, "GET", "/api/v1/attachments/"+uploaded.ID+"/content", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "audio/mp4" || !bytes.Equal(w.Body.Bytes(), audio) {
		t.Fatal("authenticated voice playback download changed bytes")
	}
}

func TestVoiceUploadRejectsUnauthenticatedOversizedAndFalseAudio(t *testing.T) {
	api := newAPI(t, storage.NewMemory())
	h := api.Handler()
	for _, test := range []struct {
		name   string
		data   []byte
		token  string
		status int
	}{{"unauthenticated", fixtureM4A(t), "", 401}, {"oversized", make([]byte, MaxAttachmentBytes+1), testToken, 413}, {"video posing as audio", mp4Fixture("soun", "vide"), testToken, 400}, {"malformed container", []byte("m4a"), testToken, 400}} {
		t.Run(test.name, func(t *testing.T) {
			w := uploadMedia(t, h, test.data, "audio/mp4", test.token)
			if w.Code != test.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	files, err := api.Repo.ListArtifacts(context.Background(), api.scope())
	if err != nil || len(files) != 0 {
		t.Fatal("rejected upload persisted audio", files, err)
	}
}

func FuzzAudioMP4NeverPanics(f *testing.F) {
	real, err := os.ReadFile("testdata/silence.m4a")
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{real, mp4Fixture("soun", "vide"), {}, {0, 0, 0, 0, 'f', 't', 'y', 'p'}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxAttachmentBytes {
			return
		}
		_, _ = attachmentMIME(data, "audio/mp4")
	})
}
