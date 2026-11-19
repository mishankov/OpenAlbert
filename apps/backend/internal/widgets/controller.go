// Package widgets handles Telegram controls without entering a model turn.
package widgets

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
)

//go:embed schedules.html
var ScheduleHTML string

type Controller struct {
	Repo             domain.Repository
	Transport        domain.WidgetTransport
	TaskChanged      func(context.Context, string, string) error
	PrepareCloseTask func(string) func()
	refreshOnce      sync.Once
	refreshLocks     [64]chan struct{} // Bounded stripes preserve same-message ordering.
}

type Request struct {
	domain.Widget
	Kind    string   `json:"kind"`
	TaskIDs []string `json:"task_ids"`
}

type Result struct {
	MessageID      int64                               `json:"message_id"`
	Fallback       bool                                `json:"fallback"`
	FallbackReason string                              `json:"fallback_reason,omitempty"`
	APIError       *integrations.TelegramAPIDiagnostic `json:"api_error,omitempty"`
}

type sendState struct {
	Result
	RetryAt time.Time `json:"retry_at,omitempty"`
}

// RateLimitError preserves an explicitly rejected send for an activity retry.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return "Telegram rate limit; widget delivery will be retried"
}

// An observer of another attempt must not cache a terminal result: that sender
// may still receive an explicit rejection with a safe retry time.
var ErrDeliveryPending = errors.New("widget delivery in progress or outcome unknown; do not resend automatically")

type registration struct {
	Conversation domain.Conversation `json:"conversation"`
	Kind         string              `json:"kind"`
	Tasks        []string            `json:"tasks,omitempty"`
	Widget       bool                `json:"widget"`
}

func registryKey(chat, message int64) string { return fmt.Sprintf("widget/%d/%d", chat, message) }

func private(c domain.Conversation) error {
	if !c.Private || c.DeliveryChannel == "desktop" || c.ChatID <= 0 || c.ChatID != c.OwnerID || c.ID != fmt.Sprintf("personal:%d", c.OwnerID) {
		return errors.New("в проде виджеты работают только в личке")
	}
	return nil
}

func (c *Controller) load(ctx context.Context, chat, message int64) (registration, error) {
	raw, ok, err := c.Repo.LoadCheckpoint(ctx, registryKey(chat, message))
	if err != nil {
		return registration{}, err
	}
	var r registration
	if !ok || json.Unmarshal(raw, &r) != nil {
		return r, errors.New("Пульт устарел. Откройте /schedules заново.")
	}
	return r, nil
}

func scheduled(t domain.Task) bool { return t.ScheduleSeconds > 0 || t.ScheduleCron != "" }
func controllable(t domain.Task) bool {
	return scheduled(t) && (t.Status == "paused" || t.Status == "queued" || t.Status == "waiting" || t.Status == "running")
}

// Send records a durable allowlist for the exact message. The sending marker
// prevents a workflow retry from duplicating a message after a lost response.
func (c *Controller) Send(ctx context.Context, conv domain.Conversation, key string, req Request) (Result, error) {
	if err := private(conv); err != nil {
		return Result{}, err
	}
	sendKey := "widget-send/" + conv.ID + "/" + key
	state, claimKey, err := c.deliveryState(ctx, sendKey)
	if err != nil {
		return Result{}, err
	}
	if state.MessageID > 0 {
		return state.Result, nil
	}
	result := state.Result
	fallback := result.Fallback
	r := registration{Conversation: conv, Kind: req.Kind, Widget: true}
	var buttons [][]domain.WidgetButton
	if r.Kind == "schedules" {
		if len(req.TaskIDs) == 0 || len(req.TaskIDs) > 4 {
			return Result{}, errors.New("provide 1–4 scheduled task_ids from task_find")
		}
		seen := map[string]bool{}
		for _, id := range req.TaskIDs {
			t, found, err := c.Repo.Task(ctx, conv.ID, id)
			if err != nil {
				return Result{}, err
			}
			if !found || !controllable(t) || seen[id] {
				return Result{}, errors.New("task must be a distinct active or paused schedule in this conversation")
			}
			seen[id] = true
		}
		r.Tasks = req.TaskIDs
		state, caption, b, err := c.panel(ctx, r)
		if err != nil {
			return Result{}, err
		}
		req.Widget = domain.Widget{HTML: ScheduleHTML, ChatData: state, Caption: caption, AllowQueries: true}
		buttons = b
	} else {
		if r.Kind != "" && r.Kind != "html" {
			return Result{}, errors.New("widget kind must be html or schedules")
		}
		r.Kind = "html"
		if req.AllowQueries {
			return Result{}, errors.New("allow_queries is supported only for the registered schedules template")
		}
		if strings.TrimSpace(req.Caption) == "" {
			return Result{}, errors.New("provide a text caption as the accessible fallback")
		}
	}
	if err := integrations.ValidateWidget(req.Widget); err != nil {
		return Result{}, err
	}
	claimed, err := c.Repo.ClaimCheckpoint(ctx, claimKey, json.RawMessage(`true`))
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{}, ErrDeliveryPending
	}
	if err := c.saveSend(ctx, sendKey, sendState{Result: result}); err != nil {
		return Result{}, err
	}
	var id int64
	if fallback {
		id, err = c.Transport.SendWidgetFallback(ctx, conv, req.Caption, buttons)
	} else {
		id, err = c.Transport.SendWidget(ctx, conv, req.Widget, buttons)
	}
	if err != nil {
		// A timeout or 5xx may have delivered the widget. Only explicit API
		// rejection is safe to replace with a text message.
		var rejected *integrations.WidgetFallbackError
		if fallback || !errors.As(err, &rejected) {
			return Result{}, c.sendResultError(ctx, sendKey, result, err)
		}
		result.Fallback = true
		result.FallbackReason, result.APIError = rejected.Reason, rejected.APIError
		if err := c.saveSend(ctx, sendKey, sendState{Result: result}); err != nil {
			return Result{}, err
		}
		id, err = c.Transport.SendWidgetFallback(ctx, conv, req.Caption, buttons)
		fallback = true
	}
	if err != nil {
		return Result{}, c.sendResultError(ctx, sendKey, result, err)
	}
	r.Widget = !fallback
	raw, _ := json.Marshal(r)
	if err = c.Repo.SaveCheckpoint(ctx, registryKey(conv.ChatID, id), raw); err != nil {
		return Result{}, err
	}
	result.MessageID, result.Fallback = id, fallback
	raw, _ = json.Marshal(result)
	return result, c.Repo.SaveCheckpoint(ctx, sendKey, raw)
}

// Read delivery state before consulting mutable task data. An overlapping
// observer must neither resend nor finalize another attempt's pending result.
func (c *Controller) deliveryState(ctx context.Context, key string) (sendState, string, error) {
	state := sendState{}
	claim := key + "/claim"
	if raw, ok, err := c.Repo.LoadCheckpoint(ctx, key); err != nil {
		return state, "", err
	} else if ok {
		if json.Unmarshal(raw, &state) != nil {
			return state, "", errors.New("invalid widget delivery state")
		}
		if state.MessageID > 0 {
			return state, "", nil
		}
		if state.RetryAt.IsZero() {
			return state, "", ErrDeliveryPending
		}
		if delay := time.Until(state.RetryAt); delay > 0 {
			return state, "", &RateLimitError{RetryAfter: delay}
		}
		claim += "/" + strconv.FormatInt(state.RetryAt.UnixNano(), 10)
	}
	return state, claim, nil
}

type CommandReplyError struct{ Cause error }

func (e *CommandReplyError) Error() string { return e.Cause.Error() }
func (e *CommandReplyError) Unwrap() error { return e.Cause }

var errCommandReplyUnknown = &integrations.TelegramDeliveryError{
	Cause:   errors.New("widget command acknowledgement expired before its result was recorded"),
	Unknown: true,
}

func commandReplyKey(in domain.Incoming) string {
	return "widget-command-reply/" + in.Conversation.ID + "/" + in.ID
}

// Reply freezes a command's acknowledgement before contacting Telegram. A 429
// retries only this text, even if a later command has changed the same task.
// The transport checks the recipient for both private and group error replies.
func (c *Controller) Reply(ctx context.Context, in domain.Incoming, text string) error {
	if err := c.reply(ctx, in, text); err != nil {
		return &CommandReplyError{Cause: err}
	}
	return nil
}

func (c *Controller) reply(ctx context.Context, in domain.Incoming, text string) error {
	raw, _ := json.Marshal(text)
	claimed, err := c.Repo.ClaimCheckpoint(ctx, commandReplyKey(in), raw)
	if err != nil {
		return err
	}
	if !claimed {
		raw, _, err = c.Repo.LoadCheckpoint(ctx, commandReplyKey(in))
		if err != nil {
			return err
		}
		if json.Unmarshal(raw, &text) != nil {
			return errors.New("invalid command reply")
		}
	}
	key := "widget-command-send/" + in.Conversation.ID + "/" + in.ID
	state, claim, err := c.deliveryState(ctx, key)
	if errors.Is(err, ErrDeliveryPending) {
		// A sending marker from before claims had deadlines cannot be retried.
		return errCommandReplyUnknown
	}
	if err != nil {
		return err
	}
	if state.MessageID > 0 {
		return nil
	}
	// Bound the send and result write well within the lifetime of the claim.
	// Storing the deadline in the claim also covers interruption before HTTP.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, _ = json.Marshal(time.Now().Add(time.Minute))
	claimed, err = c.Repo.ClaimCheckpoint(ctx, claim, raw)
	if err != nil {
		return err
	}
	if !claimed {
		return c.observeCommandReply(ctx, key, claim)
	}
	id, err := c.Transport.SendWidgetFallback(ctx, in.Conversation, text, nil)
	if err != nil {
		return c.sendResultError(ctx, key, Result{Fallback: true}, err)
	}
	return c.saveSend(ctx, key, sendState{Result: Result{MessageID: id, Fallback: true}})
}

func (c *Controller) observeCommandReply(ctx context.Context, key, claim string) error {
	raw, _, err := c.Repo.LoadCheckpoint(ctx, claim)
	if err != nil {
		return err
	}
	var deadline time.Time
	if string(raw) != "true" && json.Unmarshal(raw, &deadline) != nil {
		return errors.New("invalid widget command acknowledgement claim")
	}
	if time.Now().Before(deadline) {
		return ErrDeliveryPending
	}
	// A result may have landed between the original state read and the claim
	// check. Honor it before deciding that delivery is unknown.
	state, currentClaim, err := c.deliveryState(ctx, key)
	if err != nil || state.MessageID > 0 {
		return err
	}
	if currentClaim != claim {
		return ErrDeliveryPending
	}
	return errCommandReplyUnknown
}

func (c *Controller) saveSend(ctx context.Context, key string, state sendState) error {
	raw, _ := json.Marshal(state)
	return c.Repo.SaveCheckpoint(ctx, key, raw)
}

func (c *Controller) sendResultError(ctx context.Context, key string, result Result, err error) error {
	var rejected *integrations.TelegramError
	if !errors.As(err, &rejected) || !rejected.Rejected || rejected.Status >= 500 || rejected.Code != 429 {
		return err
	}
	delay := max(time.Second, rejected.RetryAfter)
	if saveErr := c.saveSend(ctx, key, sendState{Result: result, RetryAt: time.Now().Add(delay)}); saveErr != nil {
		return saveErr
	}
	return &RateLimitError{RetryAfter: delay}
}

func (c *Controller) Edit(ctx context.Context, conv domain.Conversation, id int64, data domain.WidgetData) error {
	if err := private(conv); err != nil {
		return err
	}
	r, err := c.load(ctx, conv.ChatID, id)
	if err != nil {
		return err
	}
	if r.Conversation.ID != conv.ID || r.Kind != "html" || !r.Widget {
		return errors.New("only this conversation's registered HTML widgets can be edited; schedule state is server-managed")
	}
	return c.Transport.EditWidgetData(ctx, conv, id, data)
}

type item struct {
	Title   string `json:"t"`
	Paused  int    `json:"s"`
	Version int64  `json:"v"`
}

func short(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

func (c *Controller) panel(ctx context.Context, r registration) (string, string, [][]domain.WidgetButton, error) {
	items := []item{}
	caption := "Расписания\n"
	buttons := [][]domain.WidgetButton{}
	for k, id := range r.Tasks {
		t, found, err := c.Repo.Task(ctx, r.Conversation.ID, id)
		if err != nil {
			return "", "", nil, err
		}
		entry := item{Title: short(t.Title, 24)}
		status := "недоступно"
		if found && controllable(t) {
			entry.Version = t.Version
			status = "включено"
			verb := "Приостановить"
			if t.Status == "paused" {
				entry.Paused = 1
				status = "пауза"
				verb = "Включить"
			}
			data, _ := json.Marshal(action{Index: k, Paused: 1 - entry.Paused, Version: t.Version})
			buttons = append(buttons, []domain.WidgetButton{{Text: verb + ": " + entry.Title, Data: string(data)}})
		}
		items = append(items, entry)
		caption += fmt.Sprintf("%s: %s\nID: %s\n", entry.Title, status, id)
	}
	caption += "/schedules [страница] — обновить · /pause ID · /resume ID"
	raw, _ := json.Marshal(items)
	return string(raw), caption, buttons, nil
}

type action struct {
	Index   int   `json:"k"`
	Paused  int   `json:"s"`
	Version int64 `json:"v"`
}

// set uses the repository's version check. Replayed/stale taps never invert a
// task a second time, and a concurrent worker update cannot be overwritten.
func (c *Controller) set(ctx context.Context, conv domain.Conversation, id string, paused int, version int64) (func(), error) {
	t, found, err := c.Repo.Task(ctx, conv.ID, id)
	if err != nil {
		return nil, err
	}
	if !found || !controllable(t) {
		return nil, errors.New("Расписание недоступно. Обновите /schedules.")
	}
	desired := "queued"
	if paused == 1 {
		desired = "paused"
	}
	if (t.Status == "paused") == (paused == 1) {
		return nil, nil
	}
	if t.Version != version {
		return nil, errors.New("Расписание изменилось. Обновите /schedules.")
	}
	var cleanup func()
	if paused == 1 && c.PrepareCloseTask != nil {
		cleanup = c.PrepareCloseTask(id)
	}
	t.Status = desired
	if _, err = c.Repo.UpdateTask(ctx, t); err != nil {
		return nil, errors.New("Расписание изменилось. Обновите /schedules.")
	}
	if c.TaskChanged != nil {
		_ = c.TaskChanged(ctx, conv.ID, id)
	}
	return cleanup, nil
}

// Handle answers before rendering updates. Slow Telegram edits cannot hold the
// request spinner open. No operation in this path calls the model.
func (c *Controller) Handle(ctx context.Context, q domain.WidgetQuery) error {
	operation, cancel := context.WithTimeout(ctx, 4*time.Second)
	r, err := c.load(operation, q.ChatID, q.MessageID)
	var cleanup func()
	if err == nil && (!q.Private || q.UserID <= 0 || q.ChatID != q.UserID || r.Conversation.OwnerID != q.UserID || r.Conversation.ChatID != q.ChatID || r.Kind != "schedules" || !q.DataType) {
		err = errors.New("Действие недоступно.")
	}
	if err == nil {
		var a struct {
			Index   *int   `json:"k"`
			Paused  *int   `json:"s"`
			Version *int64 `json:"v"`
		}
		if !utf8.ValidString(q.Data) || len(q.Data) == 0 || len(q.Data) > 64 || json.Unmarshal([]byte(q.Data), &a) != nil || a.Index == nil || a.Paused == nil || a.Version == nil || *a.Index < 0 || *a.Index >= len(r.Tasks) || *a.Paused < 0 || *a.Paused > 1 || *a.Version <= 0 {
			err = errors.New("Неизвестное действие. Обновите /schedules.")
		} else {
			cleanup, err = c.set(operation, r.Conversation, r.Tasks[*a.Index], *a.Paused, *a.Version)
		}
	}
	cancel()
	text := "Обновлено"
	if err != nil {
		text = err.Error()
	}
	answer, stop := context.WithTimeout(ctx, 2*time.Second)
	answerErr := c.Transport.AnswerWidgetQuery(answer, q, text)
	stop()
	// Driver shutdown has no context and may wait for startup or teardown.
	// Keep it after both the answer and the repaint, even when it blocks.
	if cleanup != nil {
		defer cleanup()
	}
	// Refresh also on a stale tap so that the next tap has a current version.
	if r.Kind == "schedules" && q.Private && r.Conversation.OwnerID == q.UserID && q.ChatID == q.UserID && q.DataType {
		refreshErr := c.refresh(ctx, q.MessageID, r)
		return errors.Join(err, answerErr, refreshErr)
	}
	return errors.Join(err, answerErr)
}

func (c *Controller) refresh(ctx context.Context, id int64, r registration) error {
	c.refreshOnce.Do(func() {
		for i := range c.refreshLocks {
			c.refreshLocks[i] = make(chan struct{}, 1)
		}
	})
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(registryKey(r.Conversation.ChatID, id)))
	lock := c.refreshLocks[hash.Sum64()%uint64(len(c.refreshLocks))]
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-ctx.Done():
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	state, caption, buttons, err := c.panel(ctx, r)
	if err != nil {
		return err
	}
	if r.Widget {
		err = c.Transport.EditWidgetData(ctx, r.Conversation, id, domain.WidgetData{ChatData: &state})
	}
	return errors.Join(err, c.Transport.EditWidgetControls(ctx, r.Conversation, id, r.Widget, caption, buttons))
}

// Command is the keyboard route. Menu commands and typed commands share this
// handler; /new and /stop continue to use the existing conversation workflow.
func (c *Controller) Command(ctx context.Context, in domain.Incoming) error {
	if err := private(in.Conversation); err != nil {
		return err
	}
	if raw, ok, err := c.Repo.LoadCheckpoint(ctx, commandReplyKey(in)); err != nil {
		return err
	} else if ok {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return errors.New("invalid command reply")
		}
		return c.Reply(ctx, in, text)
	}
	if in.Command == "schedules" {
		state, _, err := c.deliveryState(ctx, "widget-send/"+in.Conversation.ID+"/"+in.ID)
		if err != nil {
			return err
		}
		if state.MessageID > 0 {
			return nil
		}
	}
	fields := strings.Fields(in.Text)
	if len(fields) == 0 {
		return errors.New("command required")
	}
	switch in.Command {
	case "schedules":
		page := 1
		if len(fields) > 2 {
			return errors.New("Используйте /schedules [номер страницы]")
		}
		if len(fields) == 2 {
			var err error
			page, err = strconv.Atoi(fields[1])
			if err != nil || page < 1 {
				return errors.New("Используйте /schedules [номер страницы]")
			}
		}
		tasks, err := c.Repo.FindTasks(ctx, in.Conversation.ID, "")
		if err != nil {
			return err
		}
		ids := []string{}
		for _, t := range tasks {
			if controllable(t) {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) == 0 {
			return c.Reply(ctx, in, "Нет активных или приостановленных расписаний.")
		}
		if page > (len(ids)+3)/4 {
			return errors.New("Страница не найдена. Используйте /schedules.")
		}
		start := (page - 1) * 4
		end := min(start+4, len(ids))
		_, err = c.Send(ctx, in.Conversation, in.ID, Request{Kind: "schedules", TaskIDs: ids[start:end]})
		return err
	case "pause", "resume":
		if len(fields) != 2 {
			return errors.New("Используйте /pause ID или /resume ID; ID виден в /schedules.")
		}
		t, found, err := c.Repo.Task(ctx, in.Conversation.ID, fields[1])
		if err != nil {
			return err
		}
		if !found {
			return errors.New("Расписание не найдено.")
		}
		paused := 0
		if in.Command == "pause" {
			paused = 1
		}
		cleanup, err := c.set(ctx, in.Conversation, t.ID, paused, t.Version)
		if err != nil {
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		return c.Reply(ctx, in, "Обновлено. /schedules — показать расписания.")
	}
	return errors.New("unknown widget command")
}
