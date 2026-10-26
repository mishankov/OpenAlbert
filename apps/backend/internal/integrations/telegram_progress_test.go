package integrations

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

func TestProgressValidationAndIdempotentDeletion(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if !strings.HasSuffix(req.URL.Path, "/deleteMessage") {
			t.Error("invalid request reached Telegram")
		}
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to delete not found"}`)
	}))
	defer server.Close()
	tg, err := NewTelegram(TelegramConfig{Token: "secret", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conv := domain.Conversation{ID: "private", OwnerID: 7, ChatID: 7, Private: true}
	if err = tg.DeleteProgress(ctx, conv, "42"); err != nil {
		t.Fatal("deleting an already removed status must succeed")
	}
	for _, id := range []string{"", "-1", "invalid"} {
		if err = tg.DeleteProgress(ctx, conv, id); err == nil {
			t.Fatal("invalid deletion accepted")
		}
		if err = tg.EditProgress(ctx, conv, id, "text"); err == nil {
			t.Fatal("invalid edit accepted")
		}
	}
	for _, text := range []string{"", string([]byte{0xff}), strings.Repeat("я", 601)} {
		if _, err = tg.SendProgress(ctx, conv, text); err == nil {
			t.Fatal("invalid status accepted")
		}
		if err = tg.EditProgress(ctx, conv, "42", text); err == nil {
			t.Fatal("invalid status edit accepted")
		}
	}
	conv.ChatID = 8
	if _, err = tg.SendProgress(ctx, conv, "text"); err == nil {
		t.Fatal("unauthorized status accepted")
	}
	if err = tg.EditProgress(ctx, conv, "42", "text"); err == nil {
		t.Fatal("unauthorized edit accepted")
	}
	if err = tg.DeleteProgress(ctx, conv, "42"); err == nil {
		t.Fatal("unauthorized deletion accepted")
	}
	if calls != 1 {
		t.Fatal("invalid/unauthorized requests reached Telegram")
	}
}
