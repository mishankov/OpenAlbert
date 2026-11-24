package integrations

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

const voiceFixtureHeaderSize = 47

func testVoiceAudio(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/voice.ogg")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestTTSRequestsIndependentProviderAndOpus(t *testing.T) {
	const input = "Привет! 🎧"
	audio := testVoiceAudio(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/audio/speech" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Authorization") != "Bearer tts-only" {
			t.Error("wrong TTS request metadata")
		}
		var fields map[string]string
		if err := json.NewDecoder(req.Body).Decode(&fields); err != nil || fields["input"] != input || fields["model"] != "local" || fields["voice"] != "dmitri" || fields["response_format"] != "opus" {
			t.Error("TTS request fields changed")
		}
		w.Header().Set("Content-Type", "audio/ogg")
		_, _ = w.Write(audio)
	}))
	defer server.Close()
	tts, err := NewTTS(TTSConfig{BaseURL: server.URL + "/v1/", APIKey: "tts-only", Model: "local", Voice: "dmitri"})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := tts.Synthesize(context.Background(), input)
	if err != nil || !bytes.Equal(generated, audio) {
		t.Fatalf("TTS response changed: %v", err)
	}
}

func TestTTSRejectsInvalidResponseAndRedactsProviderErrors(t *testing.T) {
	audio := testVoiceAudio(t)
	endPage := bytes.LastIndex(audio, []byte("OggS"))
	withoutEnd := bytes.Clone(audio)
	withoutEnd[endPage+5] &^= 4
	binary.LittleEndian.PutUint32(withoutEnd[endPage+22:], oggChecksum(withoutEnd[endPage:]))
	withoutAudio := bytes.Clone(audio[:endPage])
	withoutAudio[voiceFixtureHeaderSize+5] |= 4
	binary.LittleEndian.PutUint32(withoutAudio[voiceFixtureHeaderSize+22:], oggChecksum(withoutAudio[voiceFixtureHeaderSize:]))
	differentCodec := bytes.Replace(audio, []byte("OpusHead"), []byte("NotOpus!"), 1)
	binary.LittleEndian.PutUint32(differentCodec[22:], oggChecksum(differentCodec[:voiceFixtureHeaderSize]))
	corruptAudio := bytes.Clone(audio)
	corruptAudio[len(corruptAudio)-1] ^= 1
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"error", http.StatusUnauthorized, []byte("provider echoed tts-secret")},
		{"different codec", http.StatusOK, differentCodec},
		{"identification header only", http.StatusOK, audio[:voiceFixtureHeaderSize]},
		{"no audio packet", http.StatusOK, withoutAudio},
		{"no end flag", http.StatusOK, withoutEnd},
		{"incomplete last page", http.StatusOK, audio[:len(audio)-1]},
		{"checksum mismatch", http.StatusOK, corruptAudio},
		{"oversized", http.StatusOK, append(bytes.Clone(audio), make([]byte, voiceAudioLimit)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			}))
			defer server.Close()
			tts, err := NewTTS(TTSConfig{BaseURL: server.URL, APIKey: "tts-secret", Model: "local", Voice: "dmitri"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tts.Synthesize(context.Background(), "Привет"); err == nil || strings.Contains(err.Error(), "tts-secret") {
				t.Fatal("invalid TTS response accepted or provider secret exposed")
			} else if tc.status != http.StatusOK {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != tc.status {
					t.Fatalf("status lost: %v", err)
				}
			}
		})
	}
}

func TestTTSSkipsInvalidInputAndProviderRedirects(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Path == "/other" {
			t.Error("TTS followed a redirect")
			_, _ = io.WriteString(w, "unexpected")
			return
		}
		http.Redirect(w, req, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	tts, err := NewTTS(TTSConfig{BaseURL: server.URL, APIKey: "tts-secret", Model: "local", Voice: "dmitri"})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{" ", string([]byte{0xff}), strings.Repeat("я", domain.MaxVoiceReplyRunes+1)} {
		if _, err := tts.Synthesize(context.Background(), input); err == nil {
			t.Fatal("invalid TTS input accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input reached the provider")
	}
	if _, err := tts.Synthesize(context.Background(), strings.Repeat("я", domain.MaxVoiceReplyRunes)); err == nil || requests.Load() != 1 {
		t.Fatal("valid input did not reach the provider exactly once or redirect succeeded")
	}
}
