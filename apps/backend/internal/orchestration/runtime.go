package orchestration

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/messagefmt"
	"openalbert/apps/backend/internal/telemetry"
	"openalbert/apps/backend/internal/widgets"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

type Runtime struct {
	Repo                        domain.Repository
	Activities                  *agent.Activities
	Telegram                    *integrations.Telegram
	TTS                         domain.SpeechSynthesizer
	Client                      client.Client
	TaskQueue                   string
	IdleTimeout                 time.Duration
	MaxSteps                    int
	InitiativeInterval          time.Duration
	MemoryConsolidationInterval time.Duration
	Logger                      *slog.Logger
	wg                          sync.WaitGroup
	voice                       *voiceSynthesis
	lastConsolidationCheck      time.Time
}

func (r *Runtime) Worker() worker.Worker {
	w := worker.New(r.Client, r.TaskQueue, worker.Options{MaxConcurrentActivityExecutionSize: 4, MaxConcurrentWorkflowTaskExecutionSize: 4, WorkerStopTimeout: 15 * time.Second})
	w.RegisterWorkflow(SessionWorkflow)
	w.RegisterWorkflow(TaskWorkflow)
	w.RegisterWorkflow(AnalysisWorkflow)
	w.RegisterWorkflow(InitiativeWorkflow)
	w.RegisterWorkflow(BackupEventWorkflow)
	w.RegisterWorkflow(MemoryConsolidationWorkflow)
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
		if in.Source == "desktop" {
			delete(active, in.Conversation.ID)
		} else {
			active[in.Conversation.ID] = in.Conversation
		}
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
		// Queries are independent of inbox delivery and model workflows. Start
		// the whole bounded API batch immediately so one slow request cannot
		// exhaust the 15-second deadline of the following queries.
		for _, update := range updates {
			if update.WidgetQuery == nil || r.Activities.Widgets == nil {
				continue
			}
			r.wg.Add(1)
			go func(q domain.WidgetQuery) {
				defer r.wg.Done()
				r.record(ctx, "telegram.widget_query", r.Activities.Widgets.Handle(ctx, q))
			}(*update.WidgetQuery)
		}
		for _, update := range updates {
			event := telemetry.Start(ctx, r.Logger, "telegram.receive")
			if update.Incoming == nil {
				err = r.Repo.SetPollOffset(event.Context(), update.Offset)
			} else if update.Incoming.Command == "chatgpt" {
				err = r.handleChatGPTUpdate(event.Context(), update)
			} else {
				event.Set("conversation_id", update.Incoming.Conversation.ID)
				event.Set("incoming_id", update.Incoming.ID)
				_, err = r.Repo.AcceptIncoming(event.Context(), *update.Incoming, update.Offset)
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
	if err := r.DispatchBackupEvents(ctx); err != nil {
		r.record(ctx, "backup.events", err)
	}
	incoming, err := r.Repo.PendingIncoming(ctx, 100)
	if err != nil {
		return err
	}
	for _, in := range incoming {
		if in.Command == "schedules" || in.Command == "pause" || in.Command == "resume" {
			if r.Activities.Widgets == nil {
				return errors.New("Telegram widgets are unavailable")
			}
			commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := r.Activities.Widgets.Command(commandCtx, in)
			var replyErr *widgets.CommandReplyError
			var limited *widgets.RateLimitError
			if err != nil && !errors.As(err, &replyErr) && !errors.As(err, &limited) && !errors.Is(err, widgets.ErrDeliveryPending) {
				err = r.Activities.Widgets.Reply(commandCtx, in, err.Error())
			}
			if errors.As(err, &limited) || errors.Is(err, widgets.ErrDeliveryPending) {
				cancel()
				continue
			} // Keep the inbox item for a later, cooldown-aware retry.
			if err != nil {
				event := telemetry.Start(ctx, r.Logger, "telegram.widget_command")
				event.Set("conversation_id", in.Conversation.ID)
				event.Set("incoming_id", in.ID)
				event.End(err)
			}
			cancel()
			if err := r.Repo.MarkIncomingHandled(ctx, in.ID); err != nil {
				return err
			}
			continue
		}
		text := strings.ToLower(strings.TrimSpace(in.Text))
		stop := text == "/stop" || text == "стоп" || text == "stop"
		newSession := len(in.Attachments) == 0 && (in.Command == "new" || text == "/new")
		_, err = r.Client.SignalWithStartWorkflow(ctx, SessionWorkflowID(in.Conversation.ID), MessageSignal, Event{ID: in.ID, Stop: stop || in.Command == "stop", NewSession: newSession}, workflowOptions(SessionWorkflowID(in.Conversation.ID), r.TaskQueue), SessionWorkflow, SessionInput{ConversationID: in.Conversation.ID, IdleTimeout: r.IdleTimeout, MaxSteps: r.MaxSteps})
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
	if r.MemoryConsolidationInterval > 0 {
		now := time.Now().UTC()
		if r.lastConsolidationCheck.IsZero() || now.Sub(r.lastConsolidationCheck) >= memoryConsolidationCheckInterval {
			r.lastConsolidationCheck = now
			if err := r.DispatchMemoryConsolidation(ctx, now); err != nil {
				r.record(ctx, "memory.consolidation.dispatch", err)
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
	readyVoiceID := ""
	if r.voice != nil {
		select {
		case <-r.voice.done:
			readyVoiceID = r.voice.id
		default:
		}
	}
	outgoing, err := r.Repo.PendingDeliverableOutgoing(ctx, 50, readyVoiceID, r.voice == nil)
	if err != nil {
		return err
	}
	for _, out := range outgoing {
		var voice []byte
		var fallback domain.VoiceFallback
		if out.VoiceReplyTo > 0 && out.VoiceFallback == "" {
			var ready bool
			voice, fallback, ready = r.voiceReady(ctx, out)
			if !ready {
				continue
			}
		}
		event := telemetry.Start(ctx, r.Logger, "telegram.deliver")
		ctx := event.Context()
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
		if fallback != "" {
			if err = r.Repo.FallbackVoice(ctx, out.ID, fallback); err != nil {
				event.End(err)
				return err
			}
			out.VoiceFallback = fallback
		}
		if out.VoiceFallback != "" {
			event.Set("voice_fallback", out.VoiceFallback)
		}
		// Claim the Telegram send durably before uploading. Synthesis can safely
		// run again if interrupted before this point.
		if err = r.Repo.SetOutgoingState(ctx, out.ID, "sending"); err != nil {
			event.End(err)
			return err
		}
		if out.Conversation.DeliveryChannel == "desktop" {
			// Durable queuing is delivery for the polling client; no Telegram call.
			err = nil
		} else if len(voice) > 0 {
			_, err = r.Telegram.SendVoice(ctx, out.Conversation, out.VoiceReplyTo, voice)
		} else if out.VoiceFallback != "" {
			_, err = r.Telegram.SendFormatted(ctx, out.Conversation, voiceFallbackText(out.VoiceFallback)+"\n\n"+out.Text, nil)
		} else if out.ArtifactID != "" {
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
		var apiErr *integrations.TelegramError
		rateLimited := false
		if err != nil {
			state = "unknown"
			var deliveryErr *integrations.TelegramDeliveryError
			if errors.As(err, &deliveryErr) && deliveryErr.Unknown {
				state = "unknown"
			} else if errors.As(err, &apiErr) {
				if apiErr.Code == 429 {
					state = "pending"
					rateLimited = true
				} else {
					state = "failed"
				}
			} else if len(voice) > 0 {
				state = "failed"
			}
		}
		var persistErr error
		if len(voice) > 0 && err != nil && (!rateLimited || out.Attempts >= 2) {
			fallback := domain.VoiceRejected
			if state == "unknown" {
				fallback = domain.VoiceUnconfirmed
			}
			persistErr = r.Repo.FallbackVoice(ctx, out.ID, fallback)
			event.Set("voice_fallback", fallback)
			state = "pending"
		} else {
			persistErr = r.Repo.SetOutgoingState(ctx, out.ID, state)
		}
		event.Set("delivery_state", state)
		if persistErr != nil {
			event.Set("delivery_state", "state_persist_failed")
			event.End(persistErr)
			return persistErr
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
			if state == "pending" && rateLimited {
				delay := 5 * time.Second
				if apiErr.RetryAfter > delay {
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

// Settings side effects run once before delivery. A rejected notification must
// neither re-exchange an OAuth code nor stop ingestion for every conversation.
func (r *Runtime) handleChatGPTUpdate(ctx context.Context, update integrations.Update) error {
	reply := r.Telegram.PrepareChatGPTReply(ctx, *update.Incoming)
	if err := r.Repo.SetPollOffset(ctx, update.Offset); err != nil {
		return err
	}
	if err := r.Telegram.SendChatGPTReply(ctx, update.Incoming.Conversation, reply); err != nil {
		r.record(ctx, "telegram.chatgpt.delivery", err)
	}
	return nil
}
