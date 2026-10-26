package integrations

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
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
