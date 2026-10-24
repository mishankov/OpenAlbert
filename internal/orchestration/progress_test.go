package orchestration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"openalbert/internal/domain"
)

func TestProgressEditsOneMessageAndDeletesAfterEveryReplyPart(t *testing.T) {
	ctx := context.Background()
	start := time.Now().UTC()
	var methods, texts []string
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		var body struct {
			ChatID    int64               `json:"chat_id"`
			ThreadID  int                 `json:"message_thread_id"`
			MessageID int64               `json:"message_id"`
			Text      string              `json:"text"`
			Silent    bool                `json:"disable_notification"`
			Entities  []domain.TextEntity `json:"entities"`
			ParseMode string              `json:"parse_mode"`
			Preview   struct {
				Disabled bool `json:"is_disabled"`
			} `json:"link_preview_options"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		methods = append(methods, method)
		texts = append(texts, body.Text)
		if body.ChatID != 7 {
			t.Error("wrong chat")
		}
		if method != "deleteMessage" && (len(body.Entities) != 1 || body.Entities[0].Type != "italic" || body.Entities[0].Offset != 0 || body.Entities[0].Length != len(utf16.Encode([]rune(body.Text))) || body.ParseMode != "") {
			t.Error("the whole literal status must be italic, including emoji")
		}
		if method == "sendMessage" {
			if !body.Silent || !body.Preview.Disabled || body.ThreadID != 3 {
				t.Error("status must be quiet and topic-scoped")
			}
		} else if body.MessageID != 42 {
			t.Error("status message was replaced")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	conv.ThreadID = 3
	p := domain.Progress{TurnID: "turn", Conversation: conv, StartedAt: start}
	if err := repo.StartProgress(ctx, p); err != nil {
		t.Fatal(err)
	}
	tick := func(d time.Duration) {
		t.Helper()
		if err := r.ProgressOnce(ctx, start.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	tick(29 * time.Second)
	if len(methods) != 0 {
		t.Fatal("status shown for a quick request")
	}
	tick(30 * time.Second)
	if err := repo.ReportProgress(ctx, "turn", 1, "Нашёл характеристики. Проверяю цены."); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReportProgress(ctx, "turn", 2, "🔎 Сравниваю найденные цены."); err != nil {
		t.Fatal(err)
	}
	tick(89 * time.Second)
	if len(methods) != 1 {
		t.Fatal("status updated too frequently")
	}
	// A fresh runtime using the same repository must edit the saved ID.
	restarted := &Runtime{Repo: repo, Telegram: r.Telegram, Logger: r.Logger}
	if err := restarted.ProgressOnce(ctx, start.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || texts[1] != "🔎 Сравниваю найденные цены." {
		t.Fatalf("updates not coalesced: %v %v", methods, texts)
	}
	tick(150 * time.Second) // Identical text must not cause an API call.
	if len(methods) != 2 {
		t.Fatal("identical status resent")
	}
	for _, id := range []string{"reply-text", "reply-file"} {
		if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: id, Conversation: conv, TurnID: "turn", Text: "result"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.EndProgress(ctx, "turn", "waiting_delivery"); err != nil {
		t.Fatal(err)
	}
	tick(151 * time.Second)
	if err := acknowledgeProgressPart(ctx, repo, "reply-text", "sent"); err != nil {
		t.Fatal(err)
	}
	tick(152 * time.Second)
	if len(methods) != 2 {
		t.Fatal("status removed before all parts delivered")
	}
	if err := acknowledgeProgressPart(ctx, repo, "reply-file", "sent"); err != nil {
		t.Fatal(err)
	}
	tick(153 * time.Second)
	tick(154 * time.Second)
	if got := strings.Join(methods, ","); got != "sendMessage,editMessageText,deleteMessage" {
		t.Fatalf("wrong status lifecycle: %s", got)
	}
}

func TestProgressFailureAndStopStayInSameMessage(t *testing.T) {
	for _, tc := range []struct{ status, text string }{{"failed", "Не удалось завершить запрос."}, {"cancelled", "Остановлено."}, {"delivery_failed", "Не удалось подтвердить доставку всего ответа."}} {
		t.Run(tc.status, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			var methods []string
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
				methods = append(methods, method)
				if method == "editMessageText" {
					var body struct {
						Text     string              `json:"text"`
						ID       int                 `json:"message_id"`
						Entities []domain.TextEntity `json:"entities"`
					}
					_ = json.NewDecoder(req.Body).Decode(&body)
					if body.Text != tc.text || body.ID != 42 {
						t.Error("wrong terminal status")
					}
					if len(body.Entities) != 1 || body.Entities[0].Type != "italic" || body.Entities[0].Offset != 0 || body.Entities[0].Length != len(utf16.Encode([]rune(body.Text))) {
						t.Error("terminal status must remain italic")
					}
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			})
			if err := repo.StartProgress(ctx, domain.Progress{TurnID: "turn", Conversation: conv, StartedAt: now.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if err := r.ProgressOnce(ctx, now); err != nil {
				t.Fatal(err)
			}
			status := tc.status
			if status == "delivery_failed" {
				status = "waiting_delivery"
				if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "reply", Conversation: conv, TurnID: "turn", Text: "result"}); err != nil {
					t.Fatal(err)
				}
				if err := acknowledgeProgressPart(ctx, repo, "reply", "unknown"); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.EndProgress(ctx, "turn", status); err != nil {
				t.Fatal(err)
			}
			if err := repo.ReportProgress(ctx, "turn", 100, "stale update"); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := r.ProgressOnce(ctx, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Join(methods, ","); got != "sendMessage,editMessageText" {
				t.Fatalf("terminal status replaced/deleted/repeated: %s", got)
			}
		})
	}
}

func TestProgressNetworkFailuresNeverCreateAnotherStatus(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		code           int
		edit           bool
		want           string
	}{
		{"lost initial acknowledgement", `{"ok":true,`, 200, false, "sendMessage"},
		{"initial rate limit", `{"ok":false,"error_code":429,"parameters":{"retry_after":120}}`, 429, false, "sendMessage,sendMessage"},
		{"lost edit acknowledgement", `{"ok":true,`, 200, true, "sendMessage,editMessageText,editMessageText"},
		{"unchanged edit", `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`, 400, true, "sendMessage,editMessageText"},
		{"deleted status", `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`, 400, true, "sendMessage,editMessageText"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			var methods []string
			failed := false
			r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
				methods = append(methods, method)
				if !failed && ((tc.edit && method == "editMessageText") || !tc.edit) {
					failed = true
					w.WriteHeader(tc.code)
					_, _ = io.WriteString(w, tc.response)
					return
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			})
			if err := repo.StartProgress(ctx, domain.Progress{TurnID: "turn", Conversation: conv, StartedAt: now.Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if err := r.ProgressOnce(ctx, now); err != nil {
				t.Fatal(err)
			}
			if err := repo.ReportProgress(ctx, "turn", 1, "Проверяю цены."); err != nil {
				t.Fatal(err)
			}
			for _, d := range []time.Duration{time.Minute, 61 * time.Second, 119 * time.Second, 120 * time.Second, 3 * time.Minute} {
				if err := r.ProgressOnce(ctx, now.Add(d)); err != nil {
					t.Fatal(err)
				}
			}
			if got := strings.Join(methods, ","); got != tc.want {
				t.Fatalf("wrong retry lifecycle: %s", got)
			}
		})
	}
}

func TestFinishedQuickRequestNeverCreatesStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	calls := 0
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	for _, status := range []string{"waiting_delivery", "failed", "cancelled"} {
		if err := repo.StartProgress(ctx, domain.Progress{TurnID: status, Conversation: conv, StartedAt: now}); err != nil {
			t.Fatal(err)
		}
		if status == "waiting_delivery" {
			if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "reply", Conversation: conv, TurnID: status, Text: "answer"}); err != nil {
				t.Fatal(err)
			}
			if err := acknowledgeProgressPart(ctx, repo, "reply", "sent"); err != nil {
				t.Fatal(err)
			}
		}
		if err := repo.EndProgress(ctx, status, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ProgressOnce(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("completed request created a late status")
	}
}

func acknowledgeProgressPart(ctx context.Context, repo domain.Repository, id, state string) error {
	if err := repo.SetOutgoingState(ctx, id, "sending"); err != nil {
		return err
	}
	return repo.SetOutgoingState(ctx, id, state)
}

func TestProgressCreatedWhileTurnFinishesIsDeletedOnlyAfterActualDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	var durable domain.Repository
	var conv domain.Conversation
	var methods []string
	r, repo, conversation := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		methods = append(methods, method)
		if method == "sendMessage" {
			// Finish can run while the first status POST is in flight.
			if err := durable.EnqueueOutgoing(ctx, domain.Outgoing{ID: "reply", TurnID: "turn", Conversation: conv, Text: "Готовый ответ"}); err != nil {
				t.Error(err)
			}
			if err := durable.EndProgress(ctx, "turn", "waiting_delivery"); err != nil {
				t.Error(err)
			}
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	durable, conv = repo, conversation
	if err := repo.StartProgress(ctx, domain.Progress{TurnID: "turn", Conversation: conv, StartedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := r.ProgressOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := r.ProgressOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 {
		t.Fatal("status removed before actual delivery")
	}
	if err := r.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.ProgressOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(methods, ","); got != "sendMessage,sendRichMessage,deleteMessage" {
		t.Fatalf("out-of-order completion: %s", got)
	}
}

func TestProgressDeletionCanRetryWithoutRepeatingAnswer(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	deletes := 0
	var methods []string
	r, repo, conv := deliveryRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		methods = append(methods, method)
		if method == "deleteMessage" {
			deletes++
			if deletes == 1 {
				_, _ = io.WriteString(w, `{"ok":true,`)
				return
			}
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	if err := repo.StartProgress(ctx, domain.Progress{TurnID: "turn", Conversation: conv, StartedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := r.ProgressOnce(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.EnqueueOutgoing(ctx, domain.Outgoing{ID: "reply", TurnID: "turn", Conversation: conv, Text: "Готовый ответ"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.EndProgress(ctx, "turn", "waiting_delivery"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeliverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, time.Second, 15 * time.Second, 16 * time.Second} {
		if err := r.ProgressOnce(ctx, now.Add(d)); err != nil {
			t.Fatal(err)
		}
		if err := r.DeliverOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(methods, ","); got != "sendMessage,sendRichMessage,deleteMessage,deleteMessage" {
		t.Fatalf("cleanup repeated the answer/status: %s", got)
	}
}
