package integrations

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

func TestWidgetCompressionMatchesTelegramBoundary(t *testing.T) {
	html, err := os.ReadFile("testdata/widget-zlib-boundary.html")
	if err != nil {
		t.Fatal(err)
	}
	// Generated from deterministic base64 data and measured with Python zlib
	// 1.2.12: 4097 bytes. Go's implementation undercounts the same input.
	var legacy bytes.Buffer
	z, _ := zlib.NewWriterLevel(&legacy, 9)
	_, _ = z.Write(html)
	_ = z.Close()
	if legacy.Len() > 4096 {
		t.Fatal("fixture no longer distinguishes compressors")
	}
	size, err := widgetCompressedSize(string(html))
	if err != nil || size != 4097 {
		t.Fatalf("zlib reference mismatch: size=%d error=%v", size, err)
	}
	if err := ValidateWidget(domain.Widget{HTML: string(html)}); err == nil {
		t.Fatal("server-oversized HTML accepted locally")
	}
}

func TestWidgetAndFallbackSendsKeepPrivateTopic(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p["message_thread_id"] != float64(13) {
			t.Errorf("topic lost: %v", p)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer server.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	conv := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true, ThreadID: 13}
	if _, err := tg.SendWidget(context.Background(), conv, domain.Widget{HTML: "ok", Caption: "ok"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.SendWidgetFallback(context.Background(), conv, "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestWidgetBudgetsAreValidatedBeforeSending(t *testing.T) {
	noise := make([]byte, 6000)
	_, _ = rand.New(rand.NewSource(1)).Read(noise)
	for _, tc := range []struct {
		name   string
		widget domain.Widget
		valid  bool
	}{
		{"boundary", domain.Widget{HTML: strings.Repeat("x", 32768), ChatData: strings.Repeat("я", 512), Caption: strings.Repeat("😀", 512)}, true},
		{"empty", domain.Widget{}, false},
		{"raw bytes", domain.Widget{HTML: strings.Repeat("я", 16385)}, false},
		{"compressed bytes", domain.Widget{HTML: base64.StdEncoding.EncodeToString(noise)}, false},
		{"state bytes", domain.Widget{HTML: "<b>ok</b>", PublicData: strings.Repeat("я", 513)}, false},
		{"invalid UTF8", domain.Widget{HTML: string([]byte{255})}, false},
		{"NUL HTML", domain.Widget{HTML: "a\x00b"}, false},
		{"NUL state", domain.Widget{HTML: "ok", ChatData: "\x00"}, false},
		{"caption", domain.Widget{HTML: "ok", Caption: strings.Repeat("😀", 513)}, false},
		{"height", domain.Widget{HTML: "ok", Height: 601}, false},
		{"width", domain.Widget{HTML: "ok", Width: -1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWidget(tc.widget); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; t.Error("invalid widget reached API") }))
	defer server.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "test", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	conv := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}
	if _, err := tg.SendWidget(context.Background(), conv, domain.Widget{HTML: strings.Repeat("x", 32769)}, nil); err == nil {
		t.Fatal("oversized widget accepted")
	}
	conv.Private = false
	if _, err := tg.SendWidget(context.Background(), conv, domain.Widget{HTML: "ok"}, nil); err == nil || !strings.Contains(err.Error(), "только в личке") {
		t.Fatalf("group accepted: %v", err)
	}
	if calls != 0 {
		t.Fatal(calls)
	}
}

func TestWidgetEditsPreserveOmittedAndClearEmptyState(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if !strings.HasSuffix(r.URL.Path, "/editMessageWidgetData") || p["chat_data"] != "" || p["message_id"] != float64(42) {
			t.Errorf("wrong edit: %v", p)
		}
		if _, ok := p["public_data"]; ok {
			t.Error("omitted state was overwritten")
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "test", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	conv := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}
	empty := ""
	if err := tg.EditWidgetData(context.Background(), conv, 42, domain.WidgetData{ChatData: &empty}); err != nil {
		t.Fatal(err)
	}
	if err := tg.EditWidgetData(context.Background(), conv, 42, domain.WidgetData{}); err == nil {
		t.Fatal("empty edit accepted")
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestPollIncludesWidgetAndCallbackQueriesWithoutModelInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Allowed []string `json:"allowed_updates"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if strings.Join(p.Allowed, ",") != "message,widget_query,callback_query" {
			t.Errorf("updates: %v", p)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[
		{"update_id":1,"widget_query":{"id":"tap","from":{"id":7},"chat":{"id":7,"type":"private"},"message_id":42,"type":{"type":"data"},"data":"{}"}},
		{"update_id":2,"callback_query":{"id":"button","from":{"id":7},"message":{"message_id":42,"chat":{"id":7,"type":"private"}},"data":"{}"}},
		{"update_id":3,"widget_query":{"id":"unauthorized","from":{"id":8},"chat":{"id":8,"type":"private"},"message_id":42,"type":{"type":"data"},"data":"{}"}},
		{"update_id":4,"widget_query":{"id":"wallet","from":{"id":7},"chat":{"id":7,"type":"private"},"message_id":42,"type":{"type":"sign_data"},"data":"{}"}}
		]}`))
	}))
	defer server.Close()
	tg, _ := NewTelegram(TelegramConfig{Token: "test", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 4 {
		t.Fatalf("%+v %v", updates, err)
	}
	for i, u := range updates {
		if u.Incoming != nil || u.WidgetQuery == nil || u.Offset != int64(i+2) {
			t.Fatalf("query entered inbox or lost offset: %+v", u)
		}
	}
	if !updates[0].WidgetQuery.DataType || !updates[1].WidgetQuery.Callback || updates[2].WidgetQuery.DataType || updates[3].WidgetQuery.DataType {
		t.Fatalf("query scopes: %+v", updates)
	}
}

func TestWidgetAvailabilityModesAndExpiry(t *testing.T) {
	for _, mode := range []string{"auto", "on", "off"} {
		t.Run(mode, func(t *testing.T) {
			probes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes++
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: INPUT_MEDIA_WIDGET_NOT_ALLOWED"}`))
			}))
			defer server.Close()
			tg, err := NewTelegram(TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7, 8}, Widgets: mode})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if i == 2 {
					tg.widgetMu.Lock()
					tg.widgetUnavailableUntil = time.Now().Add(-time.Second)
					tg.widgetMu.Unlock()
				}
				// The cache applies to all conversations served by this bot.
				conv := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}
				if i == 1 {
					conv.ID = "personal:8"
					conv.ChatID = 8
					conv.OwnerID = 8
				}
				_, err := tg.SendWidget(context.Background(), conv, domain.Widget{HTML: "ok", Caption: "ok"}, nil)
				var rejected *WidgetFallbackError
				if !errors.As(err, &rejected) {
					t.Fatalf("missing fallback: %v", err)
				}
				wantReason := "api_not_supported"
				if mode == "off" {
					wantReason = "disabled"
				}
				if rejected.Reason != wantReason {
					t.Fatal(rejected.Reason)
				}
				if mode != "off" && (rejected.APIError == nil || rejected.APIError.HTTPStatus != 400) {
					t.Fatal("lost API diagnostic")
				}
			}
			want := 2
			if mode == "on" {
				want = 3
			}
			if mode == "off" {
				want = 0
			}
			if probes != want {
				t.Fatalf("probes=%d want=%d", probes, want)
			}
		})
	}
	if _, err := NewTelegram(TelegramConfig{Widgets: "invalid", Token: "synthetic", AllowedUserIDs: []int64{7}}); err == nil {
		t.Fatal("invalid widgets mode accepted")
	}
}

func TestWidgetRejectionDiagnosticsAndCacheClassification(t *testing.T) {
	for _, tc := range []struct {
		description, reason  string
		status, code, probes int
	}{
		{"Bad Request: INPUT_MEDIA_WIDGET_NOT_ALLOWED", "api_not_supported", 400, 400, 1},
		{"Not Found", "api_not_supported", 404, 404, 1},
		{"Bad Request: INPUT_MEDIA_WIDGET_HTML_INVALID", "validation", 400, 400, 2},
		{"Bad Request: chat not found", "api_rejected", 200, 400, 2},
	} {
		t.Run(tc.reason+tc.description, func(t *testing.T) {
			var logs bytes.Buffer
			probes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes++
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": tc.code, "description": tc.description})
			}))
			defer server.Close()
			tg, _ := NewTelegram(TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
			conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
			for i := 0; i < 2; i++ {
				_, err := tg.SendWidget(context.Background(), conv, domain.Widget{HTML: "<b>private-content</b>", ChatData: "private-state", Caption: "caption"}, nil)
				var rejected *WidgetFallbackError
				if !errors.As(err, &rejected) || rejected.Reason != tc.reason || rejected.APIError.HTTPStatus != tc.status || rejected.APIError.Code != tc.code || rejected.APIError.Description != tc.description {
					t.Fatalf("diagnostic: %+v %v", rejected, err)
				}
			}
			if probes != tc.probes {
				t.Fatalf("probes=%d", probes)
			}
			var event map[string]any
			if err := json.Unmarshal(bytes.Split(logs.Bytes(), []byte("\n"))[0], &event); err != nil {
				t.Fatal(err)
			}
			if event["operation"] != "telegram.widget.send" || event["method"] != "sendWidget" || event["api_http_status"] != float64(tc.status) || event["api_description"] != tc.description || event["chat_id"] != float64(7) || event["html_bytes"] != float64(22) || event["html_compressed_bytes"] == nil {
				t.Fatalf("missing diagnostic event: %v", event)
			}
			if strings.Contains(logs.String(), "private-content") || strings.Contains(logs.String(), "private-state") {
				t.Fatal("payload leaked into logs")
			}
		})
	}
}

func TestTelegramDiagnosticRedactsTokenAndBoundsText(t *testing.T) {
	tg, _ := NewTelegram(TelegramConfig{Token: "synthetic-secret", AllowedUserIDs: []int64{7}})
	description := tg.safeAPIDescription("https://api.telegram.org/botsynthetic-secret/sendWidget " + strings.Repeat("я", 1024))
	if strings.Contains(description, "synthetic-secret") || len(description) > 1024 || !utf8.ValidString(description) {
		t.Fatal("unsafe API diagnostic")
	}
}
