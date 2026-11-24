package integrations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
)

func TestArtifactMediaMethodAndCaptionValidation(t *testing.T) {
	var pngData, jpegData, wideData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&wideData, image.NewRGBA(image.Rect(0, 0, 100, 1))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, field string
		data                []byte
		caption             string
		maxBytes            int64
		wantError           bool
	}{
		{"png", "sendPhoto", "photo", pngData.Bytes(), "Описание", 0, false},
		{"jpeg without caption", "sendPhoto", "photo", jpegData.Bytes(), "", 0, false},
		{"wide image as file", "sendDocument", "document", wideData.Bytes(), "Панорама", 0, false},
		{"document with caption", "sendDocument", "document", []byte("document"), "Описание", 0, false},
		{"invalid image as file", "sendDocument", "document", []byte("not an image"), "Описание", 0, false},
		{"photo size fallback", "sendDocument", "document", append(bytes.Clone(pngData.Bytes()), make([]byte, 10<<20)...), "Большой файл", 0, false},
		{"caption too long", "", "", pngData.Bytes(), strings.Repeat("я", 1025), 0, true},
		{"invalid caption", "", "", pngData.Bytes(), string([]byte{0xff}), 0, true},
		{"file too large", "", "", pngData.Bytes(), "Описание", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				if !strings.HasSuffix(req.URL.Path, "/"+tc.method) {
					t.Error("wrong media method")
				}
				if err := req.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					return
				}
				defer req.MultipartForm.RemoveAll()
				file, header, err := req.FormFile(tc.field)
				if err != nil {
					t.Error(err)
					return
				}
				data, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil || !bytes.Equal(data, tc.data) || header.Filename != "image.png" || req.FormValue("caption") != tc.caption {
					t.Error("media or caption changed")
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			}))
			defer server.Close()
			tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, MaxFileBytes: tc.maxBytes})
			if err != nil {
				t.Fatal(err)
			}
			id, err := tg.SendArtifact(context.Background(), domain.Conversation{OwnerID: 7, ChatID: 7, Private: true}, domain.Artifact{Name: "../image.png", Data: tc.data}, messagefmt.Message{Text: tc.caption})
			if tc.wantError {
				if err == nil || calls != 0 {
					t.Fatal("invalid media reached Telegram")
				}
			} else if err != nil || id != "42" || calls != 1 {
				t.Fatalf("media delivery failed: %v, calls=%d", err, calls)
			}
		})
	}
}

func TestTelegramVoiceReplyUsesTheIncomingMessageID(t *testing.T) {
	const replyID = 83
	audio := testVoiceAudio(t)
	for _, tc := range []struct {
		name, chatType string
		chatID         int64
		threadID       int
	}{
		{"private chat", "private", 7, 0},
		{"forum topic", "supergroup", -20, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/botsecret/getUpdates":
					_, _ = fmt.Fprintf(w, `{"ok":true,"result":[{"update_id":10,"message":{"message_id":%d,"date":100,"from":{"id":7},"chat":{"id":%d,"type":%q},"message_thread_id":%d,"is_topic_message":%t,"text":"reply by voice","reply_to_message":{"message_id":42}}}]}`, replyID, tc.chatID, tc.chatType, tc.threadID, tc.threadID > 0)
				case "/botsecret/sendVoice":
					if req.Method != http.MethodPost {
						t.Error("wrong Telegram upload method")
					}
					if err := req.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						return
					}
					defer req.MultipartForm.RemoveAll()
					thread := ""
					if tc.threadID > 0 {
						thread = strconv.Itoa(tc.threadID)
					}
					if req.FormValue("chat_id") != strconv.FormatInt(tc.chatID, 10) || req.FormValue("message_thread_id") != thread || req.FormValue("reply_parameters") != `{"message_id":83}` {
						t.Error("Telegram voice reply lost source message, chat or topic")
					}
					file, header, err := req.FormFile("voice")
					if err != nil {
						t.Error(err)
						return
					}
					defer file.Close()
					data, err := io.ReadAll(file)
					if err != nil || !bytes.Equal(data, audio) || header.Filename != "voice.ogg" || header.Header.Get("Content-Type") != "audio/ogg" {
						t.Error("wrong voice attachment")
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":123}}`)
				default:
					t.Error("wrong Bot API route")
				}
			}))
			defer server.Close()
			tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}, AllowedGroupIDs: []int64{-20}})
			if err != nil {
				t.Fatal(err)
			}
			updates, err := tg.Poll(context.Background(), 0)
			if err != nil || len(updates) != 1 || updates[0].Incoming == nil {
				t.Fatalf("missing incoming update: %v", err)
			}
			in := updates[0].Incoming
			if in.ID != "telegram:10" || in.TelegramMessageID != replyID || in.ReplyTo == nil || in.ReplyTo.MessageID != 42 {
				t.Fatal("incoming update lost identifiers")
			}
			if _, err := tg.SendVoice(context.Background(), in.Conversation, in.TelegramMessageID, audio[:voiceFixtureHeaderSize]); err == nil {
				t.Fatal("voice upload accepted an identification header without audio")
			}
			id, err := tg.SendVoice(context.Background(), in.Conversation, in.TelegramMessageID, audio)
			if err != nil || id != "123" {
				t.Fatalf("voice upload failed: %v", err)
			}
		})
	}
}

func TestTelegramVoiceDeliveryFailureClassification(t *testing.T) {
	audio := testVoiceAudio(t)
	for _, tc := range []struct {
		name, reply string
		status      int
		unknown     bool
	}{
		{"explicit rejection", `{"ok":false,"error_code":400,"description":"error for secret"}`, http.StatusBadRequest, false},
		{"response lost", `{"ok":true,`, http.StatusOK, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer server.Close()
			tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = tg.SendVoice(context.Background(), domain.Conversation{OwnerID: 7, ChatID: 7, Private: true}, 83, audio)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("failed delivery was accepted or leaked a credential")
			}
			var unknown *TelegramDeliveryError
			if (errors.As(err, &unknown) && unknown.Unknown) != tc.unknown {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if !tc.unknown {
				var rejected *TelegramError
				if !errors.As(err, &rejected) || !rejected.Rejected || strings.Contains(rejected.Description, "secret") {
					t.Fatal("explicit Telegram failure lost or leaked diagnostics")
				}
			}
		})
	}
}
