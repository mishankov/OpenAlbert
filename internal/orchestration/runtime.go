package orchestration

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"jarvis/internal/agent"
	"jarvis/internal/domain"
	"jarvis/internal/integrations"
	"jarvis/internal/messagefmt"
	"jarvis/internal/telemetry"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

type Runtime struct {
	Repo               domain.Repository
	Activities         *agent.Activities
	Telegram           *integrations.Telegram
	Client             client.Client
	TaskQueue          string
	IdleTimeout        time.Duration
	MaxSteps           int
	InitiativeInterval time.Duration
	Logger             *slog.Logger
	wg                 sync.WaitGroup
}

func (r *Runtime) Worker() worker.Worker {
	w := worker.New(r.Client, r.TaskQueue, worker.Options{MaxConcurrentActivityExecutionSize: 4, MaxConcurrentWorkflowTaskExecutionSize: 4, WorkerStopTimeout: 15 * time.Second})
	w.RegisterWorkflow(SessionWorkflow)
	w.RegisterWorkflow(TaskWorkflow)
	w.RegisterWorkflow(AnalysisWorkflow)
	w.RegisterWorkflow(InitiativeWorkflow)
	w.RegisterActivity(r.Activities)
	return w
}

// Long audio never occupies chat/tool activity slots. The HTTP adapter also
// enforces one native call, including across disconnects and worker retries.
func (r *Runtime) STTWorker() worker.Worker {
	w := worker.New(r.Client, r.TaskQueue+".stt", worker.Options{MaxConcurrentActivityExecutionSize: 1, WorkerStopTimeout: 15 * time.Second})
	w.RegisterActivity(r.Activities.TranscribeAudio)
	return w
}
func (r *Runtime) Start(ctx context.Context) {
	for _, loop := range []func(context.Context){r.poll, r.dispatch, r.deliver, r.typing, r.reapBrowsers} {
		r.wg.Add(1)
		go func(fn func(context.Context)) { defer r.wg.Done(); fn(ctx) }(loop)
	}
}
func (r *Runtime) Wait() { r.wg.Wait() }

func (r *Runtime) typing(ctx context.Context) {
	next := map[string]time.Time{}
	for ctx.Err() == nil {
		if err := r.refreshTyping(ctx, time.Now(), next); err != nil && ctx.Err() == nil {
			r.record(ctx, "telegram.typing", err)
		}
		if !pause(ctx, time.Second) {
			return
		}
	}
}

// Incoming messages stay pending until Finish. Only these conversations get a
// typing indicator; idle sessions, scheduled tasks and initiative checks do not.
func (r *Runtime) refreshTyping(ctx context.Context, now time.Time, next map[string]time.Time) error {
	incoming, err := r.Repo.PendingIncoming(ctx, 100)
	if err != nil {
		return err
	}
	active := map[string]domain.Conversation{}
	for _, in := range incoming {
		active[in.Conversation.ID] = in.Conversation
	}
	for id := range next {
		if _, ok := active[id]; !ok {
			delete(next, id)
		}
	}
	for id, conv := range active {
		if now.Before(next[id]) {
			continue
		}
		timeout, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := r.Telegram.Typing(timeout, conv)
		cancel()
		delay := 4 * time.Second // Telegram expires typing after at most five seconds.
		if err != nil {
			var apiErr *integrations.TelegramError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
				delay = apiErr.RetryAfter
			}
			if ctx.Err() == nil {
				r.record(ctx, "telegram.typing", err)
			}
		}
		next[id] = now.Add(delay)
	}
	return nil
}
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (r *Runtime) record(ctx context.Context, operation string, err error) {
	event := telemetry.Start(ctx, r.Logger, operation)
	event.End(err)
}
func (r *Runtime) poll(ctx context.Context) {
	for ctx.Err() == nil {
		offset, err := r.Repo.PollOffset(ctx)
		if err != nil {
			r.record(ctx, "telegram.offset", err)
			if !pause(ctx, 3*time.Second) {
				return
			}
			continue
		}
		updates, err := r.Telegram.Poll(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.record(ctx, "telegram.poll", err)
			if !pause(ctx, 5*time.Second) {
				return
			}
			continue
		}
		for _, update := range updates {
			event := telemetry.Start(ctx, r.Logger, "telegram.receive")
			if update.Incoming == nil {
				err = r.Repo.SetPollOffset(ctx, update.Offset)
			} else {
				event.Set("conversation_id", update.Incoming.Conversation.ID)
				event.Set("incoming_id", update.Incoming.ID)
				_, err = r.Repo.AcceptIncoming(ctx, *update.Incoming, update.Offset)
			}
			event.End(err)
			if err != nil {
				break
			}
		}
		if err != nil {
			if !pause(ctx, 3*time.Second) {
				return
			}
		} else if len(updates) == 0 {
			if !pause(ctx, 250*time.Millisecond) {
				return
			}
		}
	}
}
func workflowOptions(id, queue string) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{ID: id, TaskQueue: queue, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}
}
func already(err error) bool {
	var target *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &target)
}
func SessionWorkflowID(scope string) string { return "session/" + agent.ID(scope) }
func TaskWorkflowID(id string) string       { return "task/" + id }
func (r *Runtime) dispatch(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.DispatchOnce(ctx); err != nil {
			r.record(ctx, "dispatch", err)
		}
		if !pause(ctx, 2*time.Second) {
			return
		}
	}
}
func (r *Runtime) DispatchOnce(ctx context.Context) error {
	incoming, err := r.Repo.PendingIncoming(ctx, 100)
	if err != nil {
		return err
	}
	for _, in := range incoming {
		text := strings.ToLower(strings.TrimSpace(in.Text))
		stop := text == "/stop" || text == "стоп" || text == "stop"
		_, err = r.Client.SignalWithStartWorkflow(ctx, SessionWorkflowID(in.Conversation.ID), MessageSignal, Event{ID: in.ID, Stop: stop}, workflowOptions(SessionWorkflowID(in.Conversation.ID), r.TaskQueue), SessionWorkflow, SessionInput{ConversationID: in.Conversation.ID, IdleTimeout: r.IdleTimeout, MaxSteps: r.MaxSteps})
		if err != nil {
			return err
		}
	}
	tasks, err := r.Repo.RunnableTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		_, err = r.Client.ExecuteWorkflow(ctx, workflowOptions(TaskWorkflowID(t.ID), r.TaskQueue), TaskWorkflow, TaskInput{ConversationID: t.ConversationID, ID: t.ID, MaxSteps: r.MaxSteps})
		if err != nil && !already(err) {
			return err
		}
		if already(err) { // Changes wake timers; unchanged timestamps do not need signals.
			checkpoint := "task-dispatch/" + t.ID
			version := []byte(`"` + t.UpdatedAt.UTC().Format(time.RFC3339Nano) + `"`)
			old, ok, e := r.Repo.LoadCheckpoint(ctx, checkpoint)
			if e != nil {
				return e
			}
			if !ok || string(old) != string(version) {
				if e = r.Client.SignalWorkflow(ctx, TaskWorkflowID(t.ID), "", TaskSignal, "changed"); e != nil {
					var notFound *serviceerror.NotFound
					if !errors.As(e, &notFound) {
						return e
					}
				}
				if e = r.Repo.SaveCheckpoint(ctx, checkpoint, version); e != nil {
					return e
				}
			}
		}
	}
	sessions, err := r.Repo.PendingAnalysis(ctx, 50)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		id := "analysis/" + s.ID
		options := workflowOptions(id, r.TaskQueue)
		options.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
		options.RetryPolicy = &temporal.RetryPolicy{InitialInterval: time.Minute, BackoffCoefficient: 2, MaximumInterval: time.Hour}
		_, err = r.Client.ExecuteWorkflow(ctx, options, AnalysisWorkflow, s)
		if err != nil && !already(err) {
			return err
		}
	}
	if r.InitiativeInterval > 0 {
		conversations, e := r.Repo.ListPrivateConversations(ctx)
		if e != nil {
			return e
		}
		for _, c := range conversations {
			id := "initiative/" + agent.ID(c.ID)
			_, err = r.Client.ExecuteWorkflow(ctx, workflowOptions(id, r.TaskQueue), InitiativeWorkflow, InitiativeInput{ConversationID: c.ID, Interval: r.InitiativeInterval, MaxSteps: r.MaxSteps})
			if err != nil && !already(err) {
				return err
			}
		}
	}
	return nil
}
func (r *Runtime) deliver(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.DeliverOnce(ctx); err != nil {
			r.record(ctx, "outbox", err)
		}
		if ctx.Err() == nil {
			if err := r.ProgressOnce(ctx, time.Now().UTC()); err != nil {
				r.record(ctx, "progress", err)
			}
		}
		if !pause(ctx, time.Second) {
			return
		}
	}
}
func (r *Runtime) DeliverOnce(ctx context.Context) error {
	outgoing, err := r.Repo.PendingOutgoing(ctx, 50)
	if err != nil {
		return err
	}
	for _, out := range outgoing {
		event := telemetry.Start(ctx, r.Logger, "telegram.deliver")
		event.Set("delivery_id", out.ID)
		event.Set("conversation_id", out.Conversation.ID)
		for k, v := range map[string]string{"turn_id": out.TurnID, "session_id": out.SessionID, "task_id": out.TaskID, "trigger": out.Trigger} {
			if v != "" {
				event.Set(k, v)
			}
		}
		event.Set("attempt", out.Attempts+1)
		event.Set("part_index", out.PartIndex)
		event.Set("part_count", out.PartCount)
		if !out.EnqueuedAt.IsZero() {
			event.Set("queue_wait_ms", float64(time.Since(out.EnqueuedAt).Microseconds())/1000)
		}
		// Sending is claimed durably before network access. Startup turns abandoned sends into unknown.
		if err = r.Repo.SetOutgoingState(ctx, out.ID, "sending"); err != nil {
			event.End(err)
			return err
		}
		if out.ArtifactID != "" {
			artifact, ok, e := r.Repo.Artifact(ctx, out.Conversation.ID, out.ArtifactID)
			if e != nil {
				err = e
			} else if !ok {
				err = errors.New("outgoing artifact missing")
			} else {
				caption := messagefmt.Message{Text: out.Text, Entities: out.Entities}
				if !out.Formatted {
					caption = messagefmt.Markdown(out.Text)
				}
				_, err = r.Telegram.SendArtifact(ctx, out.Conversation, artifact, caption)
			}
			// Media and caption share one delivery. Legacy and long-text split
			// deliveries keep their independent states and are never recombined.
		} else {
			if out.Formatted {
				_, err = r.Telegram.SendFormatted(ctx, out.Conversation, out.Text, out.Entities)
			} else {
				_, err = r.Telegram.Send(ctx, out.Conversation, out.Text)
			}
		}
		state := "sent"
		if err != nil {
			state = "unknown"
			var apiErr *integrations.TelegramError
			var deliveryErr *integrations.TelegramDeliveryError
			if errors.As(err, &deliveryErr) && deliveryErr.Unknown {
				state = "unknown"
			} else if errors.As(err, &apiErr) {
				if apiErr.Code == 429 {
					state = "pending"
				} else {
					state = "failed"
				}
			}
		}
		event.Set("delivery_state", state)
		if e := r.Repo.SetOutgoingState(ctx, out.ID, state); e != nil {
			event.Set("delivery_state", "state_persist_failed")
			event.End(e)
			return e
		}
		if !out.StartedAt.IsZero() {
			event.Set("since_turn_started_ms", float64(time.Since(out.StartedAt).Microseconds())/1000)
		}
		if !out.ReceivedAt.IsZero() {
			event.Set("since_received_ms", float64(time.Since(out.ReceivedAt).Microseconds())/1000)
		}
		if state == "sent" && out.PartIndex == 0 {
			event.Set("first_part", true)
		}
		event.End(err)
		if err != nil {
			if state == "pending" {
				var apiErr *integrations.TelegramError
				delay := 5 * time.Second
				if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
					delay = apiErr.RetryAfter
				}
				if !pause(ctx, delay) {
					return ctx.Err()
				}
			}
			break
		}
	}
	return nil
}

// Browser Activities have separate workers so capacity waits cannot occupy LLM,
// inbox, delivery, or session-closing Activity execution slots.
func (r *Runtime) BrowserWorkers() []worker.Worker {
	var result []worker.Worker
	for _, lane := range []string{".browser-ui", ".browser-bg"} {
		w := worker.New(r.Client, r.TaskQueue+lane, worker.Options{MaxConcurrentActivityExecutionSize: 5, WorkerStopTimeout: 10 * time.Second})
		w.RegisterActivity(r.Activities.BrowserToolCall)
		result = append(result, w)
	}
	return result
}
func (r *Runtime) reapBrowsers(ctx context.Context) {
	if r.Activities == nil || r.Activities.Browser == nil {
		return
	}
	for pause(ctx, 15*time.Second) {
		observedAt := time.Now()
		sessions, err := r.Repo.ActiveSessions(ctx)
		if err != nil {
			r.record(ctx, "browser.reconcile", err)
			continue
		}
		active := map[string]bool{}
		for _, session := range sessions {
			active[session.ID] = true
		}
		r.Activities.Browser.Reconcile(active, observedAt)
	}
}
