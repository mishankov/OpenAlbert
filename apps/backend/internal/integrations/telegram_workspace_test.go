package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

func TestTelegramWorkspacePollOwnerOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botsecret/getUpdates" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[
			{"update_id":1,"message":{"message_id":10,"from":{"id":7,"first_name":"Owner"},"chat":{"id":-100555,"type":"supergroup","title":"Workspace"},"message_thread_id":42,"is_topic_message":true,"text":"Owner topic msg"}},
			{"update_id":2,"message":{"message_id":11,"from":{"id":7,"first_name":"Owner"},"chat":{"id":-100555,"type":"supergroup","title":"Workspace"},"text":"Owner control msg"}},
			{"update_id":3,"message":{"message_id":12,"from":{"id":8,"first_name":"AllowedOther"},"chat":{"id":-100555,"type":"supergroup","title":"Workspace"},"message_thread_id":42,"is_topic_message":true,"text":"Non-owner allowed user"}},
			{"update_id":4,"message":{"message_id":13,"from":{"id":999,"first_name":"Stranger"},"chat":{"id":-100555,"type":"supergroup","title":"Workspace"},"text":"Stranger in workspace"}},
			{"update_id":5,"message":{"message_id":14,"from":{"id":7,"first_name":"BotOwner","is_bot":true},"chat":{"id":-100555,"type":"supergroup","title":"Workspace"},"text":"Bot impersonating owner"}},
			{"update_id":6,"message":{"message_id":15,"from":{"id":7,"first_name":"Owner"},"chat":{"id":-100555,"type":"group","title":"WrongType"},"text":"Non-supergroup chat"}},
			{"update_id":7,"message":{"message_id":16,"from":{"id":999,"first_name":"Member"},"chat":{"id":-100777,"type":"supergroup","title":"RegularGroup"},"text":"Regular group member"}}
		]}`)
	}))
	defer server.Close()

	// Note: WorkspaceGroupID (-100555) is NOT repeated in AllowedGroupIDs; NewTelegram auto-allows it.
	tg, err := NewTelegram(TelegramConfig{
		Token:            "secret",
		BaseURL:          server.URL,
		OwnerUserID:      7,
		AllowedUserIDs:   []int64{7, 8},
		AllowedGroupIDs:  []int64{-100777},
		WorkspaceGroupID: -100555,
		AllowBotMessages: true, // Even when enabled globally, bots must be ignored in workspace.
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 7 {
		t.Fatalf("poll failed: len=%d err=%v", len(updates), err)
	}
	// Update 1: owner topic message accepted.
	if updates[0].Incoming == nil || updates[0].Incoming.Conversation.ID != "telegram:group:-100555:topic:42" || updates[0].Incoming.Conversation.ThreadID != 42 || updates[0].Incoming.Conversation.OwnerID != 7 {
		t.Fatalf("unexpected update 0: %+v", updates[0].Incoming)
	}
	// Update 2: owner general/#Control message accepted.
	if updates[1].Incoming == nil || updates[1].Incoming.Conversation.ID != "telegram:group:-100555:topic:0" || updates[1].Incoming.Conversation.ThreadID != 0 {
		t.Fatalf("unexpected update 1: %+v", updates[1].Incoming)
	}
	// Updates 3-6: non-owner, stranger, bot, and non-supergroup in workspace MUST be ignored.
	for _, idx := range []int{2, 3, 4, 5} {
		if updates[idx].Incoming != nil {
			t.Fatalf("update %d should have been ignored in workspace supergroup: %+v", idx, updates[idx].Incoming)
		}
	}
	// Update 7: regular allowed group still accepts non-owner participants.
	if updates[6].Incoming == nil || updates[6].Incoming.Conversation.ID != "telegram:group:-100777:topic:0" {
		t.Fatalf("regular group message unexpectedly dropped: %+v", updates[6].Incoming)
	}
}

func TestTelegramWorkspaceBotAPI(t *testing.T) {
	type call struct {
		method  string
		payload map[string]any
	}
	var mu sync.Mutex
	var calls []call
	var nextEditMode string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botsecret/")
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		calls = append(calls, call{method: method, payload: payload})
		editMode := nextEditMode
		nextEditMode = ""
		mu.Unlock()

		switch method {
		case "createForumTopic":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_thread_id":77,"name":"Dev Sprint","icon_color":7322096}}`)
		case "editForumTopic":
			if editMode == "not_modified" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "closeForumTopic", "reopenForumTopic":
			if editMode == "not_modified" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case "sendMessage":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":901}}`)
		case "editMessageText":
			switch editMode {
			case "not_modified":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
				return
			case "missing":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":901}}`)
		case "pinChatMessage", "answerCallbackQuery":
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("unexpected Telegram method: %s", method)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	tg, err := NewTelegram(TelegramConfig{
		Token:            "secret",
		BaseURL:          server.URL,
		OwnerUserID:      7,
		AllowedUserIDs:   []int64{7},
		WorkspaceGroupID: -100555,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// CreateForumTopic
	topic, err := tg.CreateForumTopic(ctx, -100555, "Dev Sprint", 7322096)
	if err != nil || topic.ThreadID != 77 || topic.Name != "Dev Sprint" {
		t.Fatalf("CreateForumTopic failed: %+v %v", topic, err)
	}
	if _, err := tg.CreateForumTopic(ctx, -100999, "Unallowed", 0); err == nil {
		t.Fatal("expected unallowed chat to be rejected")
	}

	// EditForumTopic normal + TOPIC_NOT_MODIFIED idempotency
	if err := tg.EditForumTopic(ctx, -100555, 77, "🟢 Dev Sprint"); err != nil {
		t.Fatalf("EditForumTopic failed: %v", err)
	}
	mu.Lock()
	nextEditMode = "not_modified"
	mu.Unlock()
	if err := tg.EditForumTopic(ctx, -100555, 77, "🟢 Dev Sprint"); err != nil {
		t.Fatalf("EditForumTopic TOPIC_NOT_MODIFIED should be idempotent, got: %v", err)
	}

	// CloseForumTopic + ReopenForumTopic
	if err := tg.CloseForumTopic(ctx, -100555, 77); err != nil {
		t.Fatalf("CloseForumTopic failed: %v", err)
	}
	mu.Lock()
	nextEditMode = "not_modified"
	mu.Unlock()
	if err := tg.CloseForumTopic(ctx, -100555, 77); err != nil {
		t.Fatalf("CloseForumTopic TOPIC_NOT_MODIFIED should be idempotent: %v", err)
	}
	if err := tg.ReopenForumTopic(ctx, -100555, 77); err != nil {
		t.Fatalf("ReopenForumTopic failed: %v", err)
	}

	// SendMessageWithButtons
	conv := domain.Conversation{ID: "telegram:group:-100555:topic:77", ChatID: -100555, ThreadID: 77, OwnerID: 7}
	markup := &domain.InlineKeyboardMarkup{
		InlineKeyboard: [][]domain.InlineButton{
			{{Text: "Approve", CallbackData: "t3:approve:1"}, {Text: "Deny", CallbackData: "t3:deny:1"}},
		},
	}
	msgID, err := tg.SendMessageWithButtons(ctx, conv, "Ready to deploy?", []domain.TextEntity{{Type: "bold", Offset: 0, Length: 5}}, markup)
	if err != nil || msgID != 901 {
		t.Fatalf("SendMessageWithButtons failed: id=%d err=%v", msgID, err)
	}
	// Reject >64-byte callback_data
	badMarkup := &domain.InlineKeyboardMarkup{
		InlineKeyboard: [][]domain.InlineButton{{{Text: "TooLong", CallbackData: strings.Repeat("a", 65)}}},
	}
	if _, err := tg.SendMessageWithButtons(ctx, conv, "hi", nil, badMarkup); err == nil {
		t.Fatal("expected >64-byte callback_data to fail validation")
	}

	// PinChatMessage
	if err := tg.PinChatMessage(ctx, -100555, 901, true); err != nil {
		t.Fatalf("PinChatMessage failed: %v", err)
	}

	// EditMessageText normal, not_modified, missing
	if err := tg.EditMessageText(ctx, -100555, 901, "Updated board", markup); err != nil {
		t.Fatalf("EditMessageText failed: %v", err)
	}
	mu.Lock()
	nextEditMode = "not_modified"
	mu.Unlock()
	if err := tg.EditMessageText(ctx, -100555, 901, "Updated board", nil); err != nil {
		t.Fatalf("EditMessageText not_modified should be idempotent: %v", err)
	}
	mu.Lock()
	nextEditMode = "missing"
	mu.Unlock()
	err = tg.EditMessageText(ctx, -100555, 901, "Deleted board", nil)
	var missing interface{ IsMessageMissing() bool }
	if err == nil || !errors.As(err, &missing) || !missing.IsMessageMissing() {
		t.Fatalf("expected IsMessageMissing error, got: %v", err)
	}

	// AnswerCallbackQuery with >200 rune truncation
	if err := tg.AnswerCallbackQuery(ctx, "cb-1", strings.Repeat("я", 250), true); err != nil {
		t.Fatalf("AnswerCallbackQuery failed: %v", err)
	}
	mu.Lock()
	last := calls[len(calls)-1]
	mu.Unlock()
	if last.method != "answerCallbackQuery" || last.payload["callback_query_id"] != "cb-1" || last.payload["show_alert"] != true {
		t.Fatalf("unexpected answerCallbackQuery payload: %+v", last)
	}
	if got := []rune(last.payload["text"].(string)); len(got) != 200 {
		t.Fatalf("expected callback answer truncated to 200 runes, got %d", len(got))
	}
}

func TestTelegramCallbackQueryPollingAndRouter(t *testing.T) {
	var answered []map[string]any
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/botsecret/") {
		case "getUpdates":
			_, _ = io.WriteString(w, `{"ok":true,"result":[
				{"update_id":10,"callback_query":{"id":"cb-owner","from":{"id":7},"message":{"message_id":50,"message_thread_id":9,"is_topic_message":true,"chat":{"id":-100555,"type":"supergroup"}},"data":"t3:approve:abc"}},
				{"update_id":11,"callback_query":{"id":"cb-intruder","from":{"id":8},"message":{"message_id":50,"message_thread_id":9,"is_topic_message":true,"chat":{"id":-100555,"type":"supergroup"}},"data":"t3:approve:abc"}},
				{"update_id":12,"callback_query":{"id":"cb-dm","from":{"id":7},"message":{"message_id":51,"chat":{"id":7,"type":"private"}},"data":"t3:detail:xyz"}},
				{"update_id":13,"callback_query":{"id":"cb-widget","from":{"id":7},"message":{"message_id":52,"chat":{"id":7,"type":"private"}},"data":"{\"k\":0,\"s\":1,\"v\":1}"}}
			]}`)
		case "answerCallbackQuery":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			answered = append(answered, body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	tg, err := NewTelegram(TelegramConfig{
		Token:            "secret",
		BaseURL:          server.URL,
		OwnerUserID:      7,
		AllowedUserIDs:   []int64{7, 8},
		WorkspaceGroupID: -100555,
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := tg.Poll(context.Background(), 0)
	if err != nil || len(updates) != 4 {
		t.Fatalf("poll failed: %d %v", len(updates), err)
	}
	for i, u := range updates {
		if u.CallbackQuery == nil {
			t.Fatalf("update %d missing CallbackQuery", i)
		}
	}
	if updates[0].CallbackQuery.ThreadID != 9 || updates[0].CallbackQuery.ChatID != -100555 || updates[0].CallbackQuery.UserID != 7 {
		t.Fatalf("unexpected parsed callback query: %+v", updates[0].CallbackQuery)
	}

	router := NewCallbackRouter(7, -100555, tg.AnswerCallbackQuery)
	var seen []string
	router.RegisterPrefixFunc("t3:", func(_ context.Context, q domain.CallbackQuery) (domain.CallbackResult, error) {
		seen = append(seen, "generic:"+q.Data)
		return domain.CallbackResult{Text: "Handled"}, nil
	})
	router.RegisterPrefixFunc("t3:approve:", func(_ context.Context, q domain.CallbackQuery) (domain.CallbackResult, error) {
		seen = append(seen, "approve:"+q.Data)
		return domain.CallbackResult{Text: "Approved!"}, nil
	})

	// 1. Owner workspace callback -> longest prefix wins ("t3:approve:")
	if !router.Handles(*updates[0].CallbackQuery) {
		t.Fatal("expected router to handle t3:approve callback")
	}
	if err := router.Dispatch(context.Background(), *updates[0].CallbackQuery); err != nil {
		t.Fatalf("owner workspace callback failed: %v", err)
	}
	// 2. Non-owner (even allowed user 8) in workspace -> rejected, handler NOT invoked, spinner answered
	if !router.Handles(*updates[1].CallbackQuery) {
		t.Fatal("expected router to own workspace callback")
	}
	if err := router.Dispatch(context.Background(), *updates[1].CallbackQuery); !errors.Is(err, ErrCallbackUnauthorized) {
		t.Fatalf("expected ErrCallbackUnauthorized for non-owner, got %v", err)
	}
	// 3. Owner DM callback matching registered prefix -> routed to "t3:"
	if !router.Handles(*updates[2].CallbackQuery) {
		t.Fatal("expected router to handle owner DM t3: callback")
	}
	if err := router.Dispatch(context.Background(), *updates[2].CallbackQuery); err != nil {
		t.Fatalf("owner DM callback failed: %v", err)
	}
	// 4. Non-matching private widget callback -> NOT handled by router (passes to Widgets.Handle)
	if router.Handles(*updates[3].CallbackQuery) {
		t.Fatal("private widget callback should fall through to Widgets controller")
	}
	// 5. Unregistered callback in workspace supergroup -> owned by router, rejected cleanly with ErrCallbackUnhandled
	unhandled := domain.CallbackQuery{ID: "cb-unhandled", UserID: 7, ChatID: -100555, MessageID: 55, Data: "unknown:1"}
	if !router.Handles(unhandled) {
		t.Fatal("router should own all workspace supergroup callbacks")
	}
	if err := router.Dispatch(context.Background(), unhandled); !errors.Is(err, ErrCallbackUnhandled) {
		t.Fatalf("expected ErrCallbackUnhandled, got %v", err)
	}
	// 6. Owner clicking t3: button in an unrelated non-workspace group -> rejected
	otherGroup := domain.CallbackQuery{ID: "cb-other-group", UserID: 7, ChatID: -100999, MessageID: 60, Data: "t3:approve:abc"}
	if err := router.Dispatch(context.Background(), otherGroup); !errors.Is(err, ErrCallbackUnauthorized) {
		t.Fatalf("expected ErrCallbackUnauthorized for unrelated group, got %v", err)
	}

	if len(seen) != 2 || seen[0] != "approve:t3:approve:abc" || seen[1] != "generic:t3:detail:xyz" {
		t.Fatalf("unexpected routed callbacks: %v", seen)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 5 {
		t.Fatalf("expected all 5 dispatched callbacks to be answered, got %d: %+v", len(answered), answered)
	}
}
