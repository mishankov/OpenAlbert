package widgets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/storage"
)

type apiCall struct {
	method  string
	payload map[string]any
}
type testAPI struct {
	mu          sync.Mutex
	calls       []apiCall
	reject      int
	description string
	editError   bool
	rateMethod  string
}

func (a *testAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	_ = json.NewDecoder(r.Body).Decode(&payload)
	method := path.Base(r.URL.Path)
	a.mu.Lock()
	a.calls = append(a.calls, apiCall{method, payload})
	reject, editError := a.reject, a.editError
	limited := method == a.rateMethod
	if limited {
		a.rateMethod = ""
	}
	a.mu.Unlock()
	if limited {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`))
		return
	}
	if method == "sendWidget" && reject != 0 || strings.HasPrefix(method, "edit") && editError {
		code := reject
		if editError {
			code = 500
		}
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q}`, code, a.description)
		return
	}
	if method == "sendWidget" || method == "sendMessage" {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	} else {
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

type observedTransport struct {
	domain.WidgetTransport
	answer func(domain.WidgetQuery)
	edit   func()
}

func (o observedTransport) AnswerWidgetQuery(ctx context.Context, q domain.WidgetQuery, data string) error {
	err := o.WidgetTransport.AnswerWidgetQuery(ctx, q, data)
	if o.answer != nil {
		o.answer(q)
	}
	return err
}
func (o observedTransport) EditWidgetData(ctx context.Context, conv domain.Conversation, id int64, data domain.WidgetData) error {
	if o.edit != nil {
		o.edit()
	}
	return o.WidgetTransport.EditWidgetData(ctx, conv, id, data)
}

func TestQueryAnswerDoesNotWaitForBrowserCleanup(t *testing.T) {
	c, _, _, conv := setup(t)
	sendPanel(t, c, conv)
	answered, cleaning, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	c.Transport = observedTransport{WidgetTransport: c.Transport, answer: func(domain.WidgetQuery) { close(answered) }}
	c.PrepareCloseTask = func(string) func() { return func() { close(cleaning); <-release } }
	done := make(chan error, 1)
	go func() { done <- c.Handle(context.Background(), query(`{"k":0,"s":1,"v":1}`)) }()
	select {
	case <-answered:
	case <-time.After(time.Second):
		t.Fatal("cleanup blocked query answer")
	}
	select {
	case <-cleaning:
	case <-time.After(time.Second):
		t.Fatal("cleanup never started")
	}
	select {
	case <-done:
		t.Fatal("cleanup unexpectedly finished")
	default:
	}
}

func TestConcurrentRefreshReadsLatestStateAfterPreviousEdit(t *testing.T) {
	c, repo, api, conv := setup(t)
	_, err := repo.CreateTask(context.Background(), domain.Task{ID: "polka", ConversationID: conv.ID, Title: "Полка", Goal: "Updates", Status: "waiting", ScheduleSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(context.Background(), conv, "both", Request{Kind: "schedules", TaskIDs: []string{"nba", "polka"}}); err != nil {
		t.Fatal(err)
	}
	started, release, secondAnswered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var edits atomic.Int32
	c.Transport = observedTransport{WidgetTransport: c.Transport, answer: func(q domain.WidgetQuery) {
		if q.ID == "second" {
			close(secondAnswered)
		}
	}, edit: func() {
		if edits.Add(1) == 1 {
			close(started)
			<-release
		}
	}}
	done := make(chan error, 2)
	go func() { done <- c.Handle(context.Background(), query(`{"k":0,"s":1,"v":1}`)) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first edit did not start")
	}
	q := query(`{"k":1,"s":1,"v":1}`)
	q.ID = "second"
	go func() { done <- c.Handle(context.Background(), q) }()
	select {
	case <-secondAnswered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("serialization blocked query answer")
	}
	if edits.Load() != 1 {
		close(release)
		t.Fatal("refreshes were not serialized")
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var latest []item
	for _, call := range api.calls {
		if call.method == "editMessageWidgetData" {
			_ = json.Unmarshal([]byte(call.payload["chat_data"].(string)), &latest)
		}
	}
	if len(latest) != 2 || latest[0].Paused != 1 || latest[1].Paused != 1 {
		t.Fatalf("stale state rendered last: %+v", latest)
	}
}

func TestRateLimitResumesCorrectSendStageAfterCooldown(t *testing.T) {
	for _, stage := range []string{"sendWidget", "sendMessage"} {
		t.Run(stage, func(t *testing.T) {
			c, repo, api, conv := setup(t)
			api.rateMethod = stage
			if stage == "sendMessage" {
				api.reject = 400
				api.description = "Bad Request: INPUT_MEDIA_WIDGET_NOT_ALLOWED"
			}
			req := Request{Kind: "schedules", TaskIDs: []string{"nba"}}
			_, err := c.Send(context.Background(), conv, "limited", req)
			var limited *RateLimitError
			if !errors.As(err, &limited) || limited.RetryAfter != time.Second {
				t.Fatalf("lost retry delay: %v", err)
			}
			before := len(api.calls)
			if _, err := c.Send(context.Background(), conv, "limited", req); !errors.As(err, &limited) {
				t.Fatalf("429 became unknown: %v", err)
			}
			if len(api.calls) != before {
				t.Fatal("retried before RetryAfter")
			}
			key := "widget-send/" + conv.ID + "/limited"
			raw, _, _ := repo.LoadCheckpoint(context.Background(), key)
			var state sendState
			_ = json.Unmarshal(raw, &state)
			if state.Fallback != (stage == "sendMessage") {
				t.Fatal("retry forgot fallback stage")
			}
			if stage == "sendMessage" && (state.FallbackReason != "api_not_supported" || state.APIError == nil || state.APIError.Description != api.description) {
				t.Fatalf("retry lost fallback diagnostics: %+v", state)
			}
			state.RetryAt = time.Now().Add(-time.Second) // Advance the persisted cooldown without a wall-clock wait.
			if err := c.saveSend(context.Background(), key, state); err != nil {
				t.Fatal(err)
			}
			c = &Controller{Repo: repo, Transport: c.Transport}
			result, err := c.Send(context.Background(), conv, "limited", req)
			if err != nil || result.MessageID != 42 || result.Fallback != (stage == "sendMessage") {
				t.Fatalf("retry failed: %+v %v", result, err)
			}
			if len(api.calls) != before+1 || api.calls[before].method != stage {
				t.Fatal("retry repeated an earlier stage")
			}
		})
	}
}

func TestOverlappingSendAttemptsClaimOnlyOneDelivery(t *testing.T) {
	c, _, api, conv := setup(t)
	done := make(chan error, 12)
	for range 12 {
		go func() {
			_, err := c.Send(context.Background(), conv, "same-call", Request{Kind: "schedules", TaskIDs: []string{"nba"}})
			done <- err
		}()
	}
	succeeded := false
	for range 12 {
		if <-done == nil {
			succeeded = true
		}
	}
	if !succeeded || len(api.calls) != 1 {
		t.Fatalf("overlapping attempts: success=%v calls=%d", succeeded, len(api.calls))
	}
}
func setup(t *testing.T) (*Controller, *storage.Memory, *testAPI, domain.Conversation) {
	t.Helper()
	api := &testAPI{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	tg, err := integrations.NewTelegram(integrations.TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7, 8}})
	if err != nil {
		t.Fatal(err)
	}
	repo := storage.NewMemory()
	conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
	_, err = repo.CreateTask(context.Background(), domain.Task{ID: "nba", ConversationID: conv.ID, Title: "НБА <script>bad()</script>", Goal: "Scores", Status: "waiting", ScheduleSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{Repo: repo, Transport: tg}, repo, api, conv
}
func sendPanel(t *testing.T, c *Controller, conv domain.Conversation) Result {
	t.Helper()
	out, err := c.Send(context.Background(), conv, "panel", Request{Kind: "schedules", TaskIDs: []string{"nba"}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func query(data string) domain.WidgetQuery {
	return domain.WidgetQuery{ID: "query", UserID: 7, ChatID: 7, MessageID: 42, Private: true, DataType: true, Data: data}
}

func TestScheduleTapAndInlineFallbackShareVersionedAction(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprint(fallback), func(t *testing.T) {
			c, repo, api, conv := setup(t)
			if fallback {
				api.reject = 400
			}
			changed, closed := 0, 0
			c.TaskChanged = func(context.Context, string, string) error { changed++; return nil }
			c.PrepareCloseTask = func(string) func() { return func() { closed++ } }
			out := sendPanel(t, c, conv)
			if out.MessageID != 42 || out.Fallback != fallback {
				t.Fatalf("wrong result: %+v", out)
			}
			// A known delivery is replayed without sending another message.
			before := len(api.calls)
			sendPanel(t, c, conv)
			if len(api.calls) != before {
				t.Fatal("duplicate send")
			}
			payload := api.calls[before-1].payload
			markup := payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
			data := markup[0].([]any)[0].(map[string]any)["callback_data"].(string)
			if len(data) > 64 {
				t.Fatal("query exceeds byte budget")
			}
			// Reconstructing the controller proves that the allowlist survives restart.
			c = &Controller{Repo: repo, Transport: c.Transport, TaskChanged: c.TaskChanged, PrepareCloseTask: c.PrepareCloseTask}
			q := query(data)
			q.Callback = fallback
			if err := c.Handle(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			task, _, _ := repo.Task(context.Background(), conv.ID, "nba")
			if task.Status != "paused" || task.Version != 2 || changed != 1 || closed != 1 {
				t.Fatalf("mutation: %+v changed=%d closed=%d", task, changed, closed)
			}
			answerMethod := "answerWidgetQuery"
			if fallback {
				answerMethod = "answerCallbackQuery"
			}
			if api.calls[before].method != answerMethod {
				t.Fatal("answer must precede Telegram edits")
			}
			last := api.calls[len(api.calls)-1]
			if last.method != "editMessageCaption" && !fallback || last.method != "editMessageText" && fallback {
				t.Fatalf("wrong refresh: %+v", last)
			}
			// A repeated tap sets the requested state rather than toggling it back.
			if err := c.Handle(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			task, _, _ = repo.Task(context.Background(), conv.ID, "nba")
			if task.Version != 2 {
				t.Fatal("replayed tap mutated task")
			}
			// An old resume request must not override the newer pause.
			q.Data = `{"k":0,"s":0,"v":1}`
			if err := c.Handle(context.Background(), q); err == nil {
				t.Fatal("stale request accepted")
			}
			q.Data = `{"k":0,"s":0,"v":2}`
			if err := c.Handle(context.Background(), q); err != nil {
				t.Fatal(err)
			}
			task, _, _ = repo.Task(context.Background(), conv.ID, "nba")
			if task.Status != "queued" || task.Version != 3 {
				t.Fatalf("resume: %+v", task)
			}
		})
	}
}

func TestQueriesAlwaysAnswerButCannotEscapeRegisteredScope(t *testing.T) {
	c, repo, api, conv := setup(t)
	sendPanel(t, c, conv)
	for _, tc := range []struct {
		name   string
		mutate func(*domain.WidgetQuery)
	}{
		{"other user", func(q *domain.WidgetQuery) { q.UserID = 8 }},
		{"other chat", func(q *domain.WidgetQuery) { q.UserID = 8; q.ChatID = 8 }},
		{"group", func(q *domain.WidgetQuery) { q.Private = false }},
		{"unregistered message", func(q *domain.WidgetQuery) { q.MessageID = 43 }},
		{"wallet or unauthorized", func(q *domain.WidgetQuery) { q.DataType = false }},
		{"unregistered task index", func(q *domain.WidgetQuery) { q.Data = `{"k":1,"s":1,"v":1}` }},
		{"invalid status", func(q *domain.WidgetQuery) { q.Data = `{"k":0,"s":2,"v":1}` }},
		{"large data", func(q *domain.WidgetQuery) { q.Data = strings.Repeat("x", 65) }},
		{"invalid JSON", func(q *domain.WidgetQuery) { q.Data = "bad" }},
		{"missing action fields", func(q *domain.WidgetQuery) { q.Data = `{"v":1}` }},
		{"null task index", func(q *domain.WidgetQuery) { q.Data = `{"k":null,"s":1,"v":1}` }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query(`{"k":0,"s":1,"v":1}`)
			tc.mutate(&q)
			before := len(api.calls)
			if err := c.Handle(context.Background(), q); err == nil {
				t.Fatal("forged query accepted")
			}
			if len(api.calls) <= before || api.calls[before].method != "answerWidgetQuery" {
				t.Fatal("error left spinner running")
			}
			task, _, _ := repo.Task(context.Background(), conv.ID, "nba")
			if task.Status != "waiting" || task.Version != 1 {
				t.Fatalf("unauthorized mutation: %+v", task)
			}
		})
	}
}

func TestNoFallbackAfterUnknownDeliveryAndNoAutomaticResend(t *testing.T) {
	c, _, api, conv := setup(t)
	api.reject = 500
	for i := 0; i < 2; i++ {
		if _, err := c.Send(context.Background(), conv, "panel", Request{Kind: "schedules", TaskIDs: []string{"nba"}}); err == nil {
			t.Fatal("unknown delivery reported success")
		}
	}
	if len(api.calls) != 1 || api.calls[0].method != "sendWidget" {
		t.Fatalf("unsafe retry: %+v", api.calls)
	}
}

func TestRegisteredHTMLStateAndValidation(t *testing.T) {
	c, _, api, conv := setup(t)
	for _, r := range []Request{
		{Widget: domain.Widget{HTML: "ok"}},
		{Widget: domain.Widget{HTML: "ok", Caption: "fallback", AllowQueries: true}},
		{Kind: "schedules", TaskIDs: []string{"foreign"}},
		{Kind: "schedules", TaskIDs: []string{"nba", "nba"}},
	} {
		if _, err := c.Send(context.Background(), conv, "invalid", r); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if len(api.calls) != 0 {
		t.Fatal("invalid request reached API")
	}
	out, err := c.Send(context.Background(), conv, "html", Request{Widget: domain.Widget{HTML: "<b>Read-only</b>", Caption: "Read-only fallback"}})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	if err := c.Edit(context.Background(), conv, out.MessageID, domain.WidgetData{ChatData: &empty}); err != nil {
		t.Fatal(err)
	}
	other := conv
	other.ID = "personal:8"
	other.OwnerID = 8
	other.ChatID = 8
	if err := c.Edit(context.Background(), other, out.MessageID, domain.WidgetData{ChatData: &empty}); err == nil {
		t.Fatal("cross-chat edit accepted")
	}
	if err := c.Handle(context.Background(), query(`{"k":0,"s":1,"v":1}`)); err == nil {
		t.Fatal("read-only HTML mutated schedule")
	}
}

type blockedTasks struct{ domain.Repository }

func (b blockedTasks) Task(ctx context.Context, scope, id string) (domain.Task, bool, error) {
	<-ctx.Done()
	return domain.Task{}, false, ctx.Err()
}

func TestSlowStorageStillAnswersWithinTelegramDeadline(t *testing.T) {
	c, _, api, conv := setup(t)
	sendPanel(t, c, conv)
	c.Repo = blockedTasks{c.Repo}
	// Prevent the separate best-effort repaint from extending this test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	api.mu.Lock()
	before := len(api.calls)
	api.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- c.Handle(ctx, query(`{"k":0,"s":1,"v":1}`)) }()
	// The answer is emitted after the 4s storage budget, independently of refresh.
	select {
	case <-time.After(8 * time.Second):
		t.Fatal("handler exceeded reply budget")
	case err := <-done:
		if err == nil {
			t.Fatal("slow operation succeeded")
		}
	}
	if time.Since(start) >= 10*time.Second {
		t.Fatal("deadline exceeded")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.calls) != before+1 || api.calls[before].method != "answerWidgetQuery" {
		t.Fatal("slow storage left spinner running")
	}
}

func TestTypedCommandsUseSameScheduleActionsAndKeepScopes(t *testing.T) {
	c, repo, api, conv := setup(t)
	for _, text := range []string{"/schedules", "/pause nba", "/resume nba"} {
		command := strings.TrimPrefix(strings.Fields(text)[0], "/")
		if err := c.Command(context.Background(), domain.Incoming{ID: text, Conversation: conv, Command: command, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	task, _, _ := repo.Task(context.Background(), conv.ID, "nba")
	if task.Status != "queued" || task.Version != 3 {
		t.Fatalf("keyboard flow: %+v", task)
	}
	if !strings.Contains(api.calls[0].payload["caption"].(string), "/pause ID") {
		t.Fatal("keyboard commands not discoverable")
	}
	group := conv
	group.Private = false
	group.ChatID = -20
	if err := c.Command(context.Background(), domain.Incoming{Conversation: group, Command: "pause", Text: "/pause nba"}); err == nil {
		t.Fatal("group command mutated private task")
	}
	if err := c.Command(context.Background(), domain.Incoming{Conversation: conv, Command: "schedules", Text: "/schedules 999999999999999999999"}); err == nil {
		t.Fatal("bad page accepted")
	}
}

func TestCommandAcknowledgementRetryDoesNotReplayTaskMutation(t *testing.T) {
	for _, first := range []string{"pause", "resume"} {
		t.Run(first, func(t *testing.T) {
			c, repo, api, conv := setup(t)
			second := "resume"
			if first == "resume" {
				second = "pause"
				task, _, _ := repo.Task(context.Background(), conv.ID, "nba")
				task.Status = "paused"
				if _, err := repo.UpdateTask(context.Background(), task); err != nil {
					t.Fatal(err)
				}
			}
			api.rateMethod = "sendMessage"
			old := domain.Incoming{ID: "first-command", Conversation: conv, Command: first, Text: "/" + first + " nba"}
			var limited *RateLimitError
			if err := c.Command(context.Background(), old); !errors.As(err, &limited) {
				t.Fatalf("acknowledgement rate limit lost: %v", err)
			}
			if err := c.Command(context.Background(), domain.Incoming{ID: "later-command", Conversation: conv, Command: second, Text: "/" + second + " nba"}); err != nil {
				t.Fatal(err)
			}
			latest, _, _ := repo.Task(context.Background(), conv.ID, "nba")
			if err := c.Command(context.Background(), old); !errors.As(err, &limited) {
				t.Fatalf("early acknowledgement retry: %v", err)
			}
			if len(api.calls) != 2 {
				t.Fatal("cooldown caused a second send")
			}
			key := "widget-command-send/" + conv.ID + "/" + old.ID
			raw, _, _ := repo.LoadCheckpoint(context.Background(), key)
			var state sendState
			_ = json.Unmarshal(raw, &state)
			state.RetryAt = time.Now().Add(-time.Second)
			if err := c.saveSend(context.Background(), key, state); err != nil {
				t.Fatal(err)
			}
			c = &Controller{Repo: repo, Transport: c.Transport}
			if err := c.Command(context.Background(), old); err != nil {
				t.Fatal(err)
			}
			after, _, _ := repo.Task(context.Background(), conv.ID, "nba")
			if after.Status != latest.Status || after.Version != latest.Version {
				t.Fatalf("acknowledgement retried mutation: before=%+v after=%+v", latest, after)
			}
			if len(api.calls) != 3 || api.calls[2].method != "sendMessage" {
				t.Fatal("acknowledgement was not retried exactly once")
			}
		})
	}
}

func TestWidgetUnavailableExplainsFallbackAndSkipsNextProbe(t *testing.T) {
	c, repo, api, conv := setup(t)
	api.reject = 400
	api.description = "Bad Request: INPUT_MEDIA_WIDGET_NOT_ALLOWED"
	req := Request{Widget: domain.Widget{HTML: "<b>probe</b>", Caption: "Useful text"}}
	for _, key := range []string{"first", "second", "first"} {
		// Recreate the controller too: capability state belongs to the bot transport.
		c = &Controller{Repo: repo, Transport: c.Transport}
		result, err := c.Send(context.Background(), conv, key, req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(result)
		var got struct {
			Fallback bool   `json:"fallback"`
			Reason   string `json:"fallback_reason"`
			APIError struct {
				Description string `json:"description"`
				Status      int    `json:"http_status"`
			} `json:"api_error"`
		}
		_ = json.Unmarshal(raw, &got)
		if !got.Fallback || got.Reason != "api_not_supported" || got.APIError.Status != 400 || got.APIError.Description != api.description {
			t.Fatalf("missing fallback diagnostics: %s", raw)
		}
	}
	probes, texts := 0, 0
	for _, call := range api.calls {
		if call.method == "sendWidget" {
			probes++
		}
		if call.method == "sendMessage" {
			texts++
		}
	}
	if probes != 1 || texts != 2 {
		t.Fatalf("calls: probes=%d texts=%d", probes, texts)
	}
}

func TestAmbiguousWidgetRejectionNeverFallsBackOrCachesCapability(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"proxy404", "Not found", 404},
		{"http503code400", `{"ok":false,"error_code":400,"description":"INPUT_MEDIA_WIDGET_NOT_ALLOWED"}`, 503},
		{"http503code429", `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes, texts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if path.Base(r.URL.Path) == "sendWidget" {
					probes++
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				texts++
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
			}))
			defer server.Close()
			tg, _ := integrations.NewTelegram(integrations.TelegramConfig{Token: "synthetic", BaseURL: server.URL, AllowedUserIDs: []int64{7}})
			c := &Controller{Repo: storage.NewMemory(), Transport: tg}
			conv := domain.Conversation{ID: "personal:7", OwnerID: 7, ChatID: 7, Private: true}
			for _, key := range []string{"first", "first", "second"} {
				_, err := c.Send(context.Background(), conv, key, Request{Widget: domain.Widget{HTML: "ok", Caption: "Useful text"}})
				var retry *RateLimitError
				if err == nil || errors.As(err, &retry) {
					t.Fatalf("ambiguous delivery authorized fallback/retry: %v", err)
				}
			}
			if probes != 2 || texts != 0 {
				t.Fatalf("probes=%d texts=%d", probes, texts)
			}
		})
	}
}
