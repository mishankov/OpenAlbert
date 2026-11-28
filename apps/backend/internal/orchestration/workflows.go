package orchestration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

const (
	sessionSeenLimit     = 4096
	taskSchedulerMaxPoll = 30 * time.Second
	analysisRetryInitial = time.Minute
	analysisRetryMax     = time.Hour
)

type SessionInput struct {
	ConversationID string
	IdleTimeout    time.Duration
	MaxSteps       int
}

type Event struct {
	ID         string
	Stop       bool
	NewSession bool
}

type TaskInput struct {
	ConversationID string
	ID             string
	MaxSteps       int
}

type InitiativeInput struct {
	ConversationID string
	Interval       time.Duration
	MaxSteps       int
}

func TaskWorkflowID(id string) string { return "task/" + id }

var errUserCancel = errors.New("user cancelled")

func isUserCanceled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errUserCancel)
}

func mergeUniqueIDs(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	seen := make(map[string]bool, len(base)+len(extra))
	for _, id := range base {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range extra {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ---- Per-conversation SessionActor ----

type sessionRuntimeState struct {
	SessionID        string          `json:"session_id"`
	NextTurn         int             `json:"next_turn"`
	ActiveTurnKey    string          `json:"active_turn_key,omitempty"`
	ActiveIncoming   []string        `json:"active_incoming,omitempty"`
	ActiveStep       int             `json:"active_step,omitempty"`
	ActiveAfterModel bool            `json:"active_after_model,omitempty"`
	ActiveToolIndex  int             `json:"active_tool_index,omitempty"`
	ActiveSent       bool            `json:"active_sent,omitempty"`
	ActiveStats      agent.TurnStats `json:"active_stats,omitempty"`
	IdleDeadlineAt   time.Time       `json:"idle_deadline_at,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at,omitempty"`
}

func (r *Runtime) loadCachedModelOutput(ctx context.Context, turnKey string, step int, finalize bool) (agent.ModelOutput, bool, error) {
	if r.Repo == nil || turnKey == "" {
		return agent.ModelOutput{}, false, nil
	}
	modelKey := fmt.Sprintf("model/%s/%d", turnKey, step)
	if finalize {
		modelKey += "/finalize"
	}
	raw, ok, err := r.Repo.LoadCheckpoint(ctx, modelKey)
	if err != nil || !ok {
		return agent.ModelOutput{}, false, err
	}
	var out agent.ModelOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return agent.ModelOutput{}, true, nil
	}
	return out, true, nil
}

func cloneTurnStats(s agent.TurnStats) agent.TurnStats {
	if len(s.Tools) > 0 {
		s.Tools = append([]string(nil), s.Tools...)
	}
	return s
}

func sessionRuntimeKey(convID string) string {
	return "session-runtime/" + agent.ID(convID)
}

func newSessionID(convID string) string {
	return agent.ID("session", convID, rand.Text())
}

type sessionActor struct {
	mu           sync.Mutex
	queue        []Event
	seen         map[string]bool
	seenOrder    []string
	notify       chan struct{}
	activeCancel context.CancelCauseFunc
	closing      bool
	done         chan struct{}
}

func newSessionActor() *sessionActor {
	return &sessionActor{
		seen:   map[string]bool{},
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

func (a *sessionActor) enqueue(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ev.ID != "" {
		if a.seen[ev.ID] {
			return
		}
		a.seen[ev.ID] = true
		a.seenOrder = append(a.seenOrder, ev.ID)
		if len(a.seenOrder) > sessionSeenLimit {
			oldest := a.seenOrder[0]
			a.seenOrder = a.seenOrder[1:]
			delete(a.seen, oldest)
		}
	}
	a.queue = append(a.queue, ev)
	if (ev.Stop || ev.NewSession) && a.activeCancel != nil {
		a.activeCancel(errUserCancel)
	}
	select {
	case a.notify <- struct{}{}:
	default:
	}
}

func (a *sessionActor) hasEvents() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queue) > 0
}

func (a *sessionActor) drainBatch() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) == 0 {
		return nil
	}
	batch := a.queue
	a.queue = nil
	return batch
}

func (a *sessionActor) prepend(events []Event) {
	if len(events) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = append(append([]Event(nil), events...), a.queue...)
	select {
	case a.notify <- struct{}{}:
	default:
	}
}

// startTurnCancel atomically checks for already-queued /stop or /new commands
// before installing the active turn's cancellation function so control commands
// arriving between drainBatch and turn start never wait for prepareTurn.
func (a *sessionActor) startTurnCancel(cancel context.CancelCauseFunc) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ev := range a.queue {
		if ev.Stop || ev.NewSession {
			cancel(errUserCancel)
			return false
		}
	}
	a.activeCancel = cancel
	return true
}

func (a *sessionActor) clearActiveCancel() {
	a.mu.Lock()
	a.activeCancel = nil
	a.mu.Unlock()
}

func (a *sessionActor) peekSteering() (count int, canceled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ev := range a.queue {
		if ev.Stop || ev.NewSession {
			return 0, true
		}
	}
	return len(a.queue), false
}

func (a *sessionActor) takeSteering(count int) (ids []string, canceled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ev := range a.queue {
		if ev.Stop || ev.NewSession {
			return nil, true
		}
	}
	if count > len(a.queue) {
		count = len(a.queue)
	}
	if count == 0 {
		return nil, false
	}
	ids = make([]string, 0, count)
	for _, ev := range a.queue[:count] {
		ids = append(ids, ev.ID)
	}
	a.queue = a.queue[count:]
	return ids, false
}

// ensureSessionActor starts a session actor for an already-active conversation
// without enqueuing a synthetic event, used to restore idle timers after restart.
func (r *Runtime) ensureSessionActor(ctx context.Context, convID string) {
	r.ensureInit()
	r.mu.Lock()
	defer r.mu.Unlock()
	if actor := r.sessions[convID]; actor != nil && !actor.closing {
		return
	}
	actor := newSessionActor()
	r.sessions[convID] = actor
	runCtx := r.bgCtxLocked(ctx)
	in := SessionInput{
		ConversationID: convID,
		IdleTimeout:    r.IdleTimeout,
		MaxSteps:       r.maxSteps(),
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(actor.done)
		if err := r.runSessionActor(runCtx, actor, in); err != nil && runCtx.Err() == nil {
			r.record(runCtx, "session.run", err)
		}
	}()
}

// SignalSession delivers an incoming event to the per-conversation actor,
// starting the actor goroutine if none is currently running.
func (r *Runtime) SignalSession(ctx context.Context, convID string, ev Event) error {
	r.ensureInit()
	for {
		r.mu.Lock()
		actor := r.sessions[convID]
		if actor != nil && actor.closing {
			done := actor.done
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if actor == nil {
			actor = newSessionActor()
			r.sessions[convID] = actor
			runCtx := r.bgCtxLocked(ctx)
			in := SessionInput{
				ConversationID: convID,
				IdleTimeout:    r.IdleTimeout,
				MaxSteps:       r.maxSteps(),
			}
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				defer close(actor.done)
				if err := r.runSessionActor(runCtx, actor, in); err != nil && runCtx.Err() == nil {
					r.record(runCtx, "session.run", err)
				}
			}()
		}
		actor.enqueue(ev)
		r.mu.Unlock()
		return nil
	}
}

func (r *Runtime) loadSessionRuntime(ctx context.Context, convID string) (sessionRuntimeState, bool, error) {
	if r.Repo == nil {
		return sessionRuntimeState{}, false, nil
	}
	raw, ok, err := r.Repo.LoadCheckpoint(ctx, sessionRuntimeKey(convID))
	if err != nil || !ok {
		return sessionRuntimeState{}, false, err
	}
	var state sessionRuntimeState
	if err := json.Unmarshal(raw, &state); err != nil || state.SessionID == "" {
		return sessionRuntimeState{}, false, nil
	}
	s, found, err := r.Repo.Session(ctx, state.SessionID)
	if err != nil {
		return sessionRuntimeState{}, false, err
	}
	if !found || s.Status != "active" || s.ConversationID != convID {
		return sessionRuntimeState{}, false, nil
	}
	return state, true, nil
}

func (r *Runtime) saveSessionRuntime(ctx context.Context, convID string, state sessionRuntimeState) error {
	if r.Repo == nil {
		return nil
	}
	state.UpdatedAt = r.now().UTC()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return r.Repo.SaveCheckpoint(ctx, sessionRuntimeKey(convID), raw)
}

func (r *Runtime) clearSessionRuntime(ctx context.Context, convID, expectedSessionID string) {
	if r.Repo == nil {
		return
	}
	if expectedSessionID != "" {
		raw, ok, err := r.Repo.LoadCheckpoint(ctx, sessionRuntimeKey(convID))
		if err != nil || !ok {
			return
		}
		var state sessionRuntimeState
		if json.Unmarshal(raw, &state) != nil || state.SessionID != expectedSessionID {
			return
		}
	}
	_ = r.Repo.DeleteCheckpointsByPrefix(ctx, sessionRuntimeKey(convID))
}

func (r *Runtime) runSessionActor(ctx context.Context, actor *sessionActor, in SessionInput) error {
	ctx = telemetry.WithFields(ctx, map[string]any{"workflow_id": SessionWorkflowID(in.ConversationID)})
	defer func() {
		r.mu.Lock()
		if r.sessions[in.ConversationID] == actor {
			delete(r.sessions, in.ConversationID)
		}
		r.mu.Unlock()
	}()

	idleTimeout := in.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Minute
	}
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = r.maxSteps()
	}

	sessionID := newSessionID(in.ConversationID)
	nextTurn := 0
	activeTurnKey := ""
	var activeIncoming []string
	activeStep := 0
	activeAfterModel := false
	activeToolIndex := 0
	activeSent := false
	var activeStats agent.TurnStats
	var idleDeadlineAt time.Time
	sessionStarted := false
	newCommandSeen := false

	if saved, ok, err := r.loadSessionRuntime(ctx, in.ConversationID); err != nil {
		return err
	} else if ok {
		sessionID = saved.SessionID
		nextTurn = saved.NextTurn
		activeTurnKey = saved.ActiveTurnKey
		activeIncoming = append([]string(nil), saved.ActiveIncoming...)
		activeStep = saved.ActiveStep
		activeAfterModel = saved.ActiveAfterModel
		activeToolIndex = saved.ActiveToolIndex
		activeSent = saved.ActiveSent
		activeStats = cloneTurnStats(saved.ActiveStats)
		idleDeadlineAt = saved.IdleDeadlineAt
		sessionStarted = true
	}

	resetActiveTurn := func() {
		activeTurnKey = ""
		activeIncoming = nil
		activeStep = 0
		activeAfterModel = false
		activeToolIndex = 0
		activeSent = false
		activeStats = agent.TurnStats{}
	}

	reconcileCompletedTurn := func(reconcileCtx context.Context) error {
		if activeTurnKey == "" || r.Repo == nil {
			return nil
		}
		raw, done, err := r.Repo.LoadCheckpoint(reconcileCtx, "turn-done/"+activeTurnKey)
		if err != nil || !done {
			return err
		}
		var doneTurn agent.Turn
		if json.Unmarshal(raw, &doneTurn) == nil && doneTurn.Stats.FinishReason == "failed" && !doneTurn.Sent {
			if err := r.actNotifyFailure(reconcileCtx, in.ConversationID, activeTurnKey); err != nil {
				return err
			}
		}
		resetActiveTurn()
		nextTurn++
		idleDeadlineAt = r.now().UTC().Add(idleTimeout)
		return r.saveSessionRuntime(reconcileCtx, in.ConversationID, sessionRuntimeState{
			SessionID:      sessionID,
			NextTurn:       nextTurn,
			IdleDeadlineAt: idleDeadlineAt,
		})
	}
	if err := reconcileCompletedTurn(ctx); err != nil {
		return err
	}

	for {
		batch := actor.drainBatch()
		if len(batch) == 0 {
			wait := idleTimeout
			if !idleDeadlineAt.IsZero() {
				wait = idleDeadlineAt.Sub(r.now().UTC())
				if wait < 0 {
					wait = 0
				}
			}
			timer := r.clock().NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-actor.notify:
				timer.Stop()
				continue
			case <-timer.C():
				timer.Stop()
				r.mu.Lock()
				if actor.hasEvents() {
					r.mu.Unlock()
					continue
				}
				actor.closing = true
				r.mu.Unlock()
				if newCommandSeen && !sessionStarted {
					return nil
				}
				cleanup, cancel := disconnectedCtx(ctx)
				defer cancel()
				r.clearSessionRuntime(cleanup, in.ConversationID, sessionID)
				return r.actCloseSession(cleanup, sessionID)
			}
		}

		// Keep the /new boundary inside this actor so signals arriving during the
		// transition cannot be lost or sent back into the previous session.
		newIndex := -1
		for i, ev := range batch {
			if ev.NewSession {
				newIndex = i
				break
			}
		}
		if newIndex >= 0 {
			newCommandSeen = true
			ids := make([]string, 0, newIndex+1)
			for _, ev := range batch[:newIndex+1] {
				ids = append(ids, ev.ID)
			}
			nextSession := agent.ID(sessionID, "new", batch[newIndex].ID)
			started, err := r.actStartNewSession(ctx, in.ConversationID, sessionID, nextSession, ids)
			if err != nil {
				return err
			}
			if started {
				sessionID = nextSession
				nextTurn = 0
				resetActiveTurn()
				idleDeadlineAt = r.now().UTC().Add(idleTimeout)
				sessionStarted = true
				if err := r.saveSessionRuntime(ctx, in.ConversationID, sessionRuntimeState{
					SessionID:      sessionID,
					NextTurn:       nextTurn,
					IdleDeadlineAt: idleDeadlineAt,
				}); err != nil {
					return err
				}
				actor.prepend(batch[newIndex+1:])
			} else {
				// Ignore the stale command without discarding adjacent messages.
				remaining := append([]Event{}, batch[:newIndex]...)
				remaining = append(remaining, batch[newIndex+1:]...)
				actor.prepend(remaining)
			}
			continue
		}

		ids := make([]string, 0, len(batch))
		stop := false
		lastStop := -1
		for i, ev := range batch {
			ids = append(ids, ev.ID)
			if ev.Stop {
				stop = true
				lastStop = i
			}
		}
		if stop {
			actor.prepend(batch[lastStop+1:])
			ids = ids[:lastStop+1]
			// Durably retire any interrupted turn BEFORE consuming /stop so a
			// crash after AcknowledgeStop cannot leave the old turn key active.
			if activeTurnKey != "" {
				nextTurn++
				resetActiveTurn()
				idleDeadlineAt = r.now().UTC().Add(idleTimeout)
				sessionStarted = true
				if err := r.saveSessionRuntime(ctx, in.ConversationID, sessionRuntimeState{
					SessionID:      sessionID,
					NextTurn:       nextTurn,
					IdleDeadlineAt: idleDeadlineAt,
				}); err != nil {
					return err
				}
			}
			if err := r.actAcknowledgeStop(ctx, in.ConversationID, sessionID, ids); err != nil {
				return err
			}
			idleDeadlineAt = r.now().UTC().Add(idleTimeout)
			sessionStarted = true
			if err := r.saveSessionRuntime(ctx, in.ConversationID, sessionRuntimeState{
				SessionID:      sessionID,
				NextTurn:       nextTurn,
				IdleDeadlineAt: idleDeadlineAt,
			}); err != nil {
				return err
			}
			continue
		}

		runctx, cancel := context.WithCancelCause(ctx)
		if !actor.startTurnCancel(cancel) {
			cancel(nil)
			actor.prepend(batch)
			continue
		}
		if err := reconcileCompletedTurn(runctx); err != nil {
			actor.clearActiveCancel()
			cancel(nil)
			return err
		}
		key := activeTurnKey
		turnIDs := ids
		if key != "" {
			merged := mergeUniqueIDs(activeIncoming, ids)
			// If new user messages arrived while the service was down, treat them
			// as steering and supersede any already-cached model output for the
			// interrupted step before dispatching cached tool calls.
			if len(merged) > len(activeIncoming) {
				if activeAfterModel {
					activeStep++
					activeAfterModel = false
					activeToolIndex = 0
				} else {
					cachedOut, ok, err := r.loadCachedModelOutput(runctx, key, activeStep, activeStep == maxSteps)
					if err != nil {
						actor.clearActiveCancel()
						cancel(nil)
						return err
					}
					if ok {
						activeStats.ModelCalls++
						activeStats.InputTokens += cachedOut.InputTokens
						activeStats.OutputTokens += cachedOut.OutputTokens
						activeStep++
						activeAfterModel = false
						activeToolIndex = 0
					}
				}
			}
			turnIDs = merged
		} else {
			key = agent.ID(sessionID, fmt.Sprint(nextTurn))
			activeStep = 0
			activeAfterModel = false
			activeToolIndex = 0
			activeSent = false
			activeStats = agent.TurnStats{}
		}
		activeTurnKey = key
		activeIncoming = append([]string(nil), turnIDs...)
		idleDeadlineAt = time.Time{}
		sessionStarted = true
		saveActiveProgress := func(saveCtx context.Context) error {
			return r.saveSessionRuntime(saveCtx, in.ConversationID, sessionRuntimeState{
				SessionID:        sessionID,
				NextTurn:         nextTurn,
				ActiveTurnKey:    activeTurnKey,
				ActiveIncoming:   activeIncoming,
				ActiveStep:       activeStep,
				ActiveAfterModel: activeAfterModel,
				ActiveToolIndex:  activeToolIndex,
				ActiveSent:       activeSent,
				ActiveStats:      cloneTurnStats(activeStats),
			})
		}
		if err := saveActiveProgress(ctx); err != nil {
			actor.clearActiveCancel()
			cancel(nil)
			return err
		}

		progress := &turnProgress{
			StartStep:  activeStep,
			AfterModel: activeAfterModel,
			StartTool:  activeToolIndex,
			Sent:       activeSent,
			Stats:      cloneTurnStats(activeStats),
			OnUpdate: func(uCtx context.Context, step int, afterModel bool, toolIdx int, sent bool, stats agent.TurnStats) error {
				activeStep = step
				activeAfterModel = afterModel
				activeToolIndex = toolIdx
				activeSent = activeSent || sent
				activeStats = cloneTurnStats(stats)
				return saveActiveProgress(uCtx)
			},
		}

		turn, runErr := r.runTurnWithProgress(runctx, agent.RunInput{
			Key:            key,
			SessionID:      sessionID,
			ConversationID: in.ConversationID,
			IncomingIDs:    turnIDs,
		}, maxSteps, progress, func(turn *agent.Turn, resumeStep int, beforeSteer func() error) error {
			count, canceled := actor.peekSteering()
			if canceled {
				return context.Canceled
			}
			if count == 0 {
				return nil
			}
			if beforeSteer != nil {
				if err := beforeSteer(); err != nil {
					return err
				}
			}
			newIDs, canceled := actor.takeSteering(count)
			if canceled {
				return context.Canceled
			}
			if len(newIDs) == 0 {
				return nil
			}
			steerInput := turn.Input
			// Retain consumed inputs, cached-step usage, and the superseded
			// resume step in one durable update BEFORE prepareTurn so a crash
			// during preparation cannot replay obsolete cached actions.
			allIDs := mergeUniqueIDs(turn.Input.IncomingIDs, newIDs)
			turn.Input.IncomingIDs = allIDs
			steerInput.IncomingIDs = allIDs
			activeIncoming = append([]string(nil), allIDs...)
			if resumeStep == activeStep && !activeAfterModel {
				cachedOut, ok, err := r.loadCachedModelOutput(runctx, key, activeStep, activeStep == maxSteps)
				if err != nil {
					return err
				}
				if ok {
					turn.Stats.ModelCalls++
					turn.Stats.InputTokens += cachedOut.InputTokens
					turn.Stats.OutputTokens += cachedOut.OutputTokens
					resumeStep = activeStep + 1
				}
			}
			if resumeStep > activeStep || (resumeStep == activeStep && activeAfterModel) {
				activeStep = resumeStep
				activeAfterModel = false
				activeToolIndex = 0
			}
			activeSent = activeSent || turn.Sent
			activeStats = cloneTurnStats(turn.Stats)
			if err := saveActiveProgress(runctx); err != nil {
				return err
			}
			if steerInput.OriginalIncomingID == "" && len(allIDs) > 0 {
				steerInput.OriginalIncomingID = allIDs[0]
			}
			updated, err := r.prepareTurn(runctx, steerInput)
			if err != nil {
				return err
			}
			updated.Input.IncomingIDs = allIDs
			updated.Stats = turn.Stats
			if !turn.ReceivedAt.IsZero() && (updated.ReceivedAt.IsZero() || turn.ReceivedAt.Before(updated.ReceivedAt)) {
				updated.ReceivedAt = turn.ReceivedAt
			}
			*turn = updated
			return nil
		})
		userStopped := isUserCanceled(runctx)
		actor.clearActiveCancel()
		cancel(nil)

		// Distinguish graceful runtime shutdown from user /stop or /new: leave
		// the active turn key and pending inbox items intact so restart resumes
		// the exact same turn and replays its receipts without dropping work.
		if ctx.Err() != nil && !userStopped {
			return ctx.Err()
		}

		// Persist final state outside the cancelled turn context so /stop or /new
		// cannot resurrect inbox items.
		cleanup, cancelCleanup := disconnectedCtx(ctx)
		finishErr := r.actFinish(cleanup, finishInput(turn, runErr))
		if finishErr != nil {
			cancelCleanup()
			return finishErr
		}
		if runErr != nil && !isCanceled(runErr) && !turn.Sent {
			if err := r.actNotifyFailure(cleanup, in.ConversationID, key); err != nil {
				cancelCleanup()
				return err
			}
		}
		resetActiveTurn()
		nextTurn++
		idleDeadlineAt = r.now().UTC().Add(idleTimeout)
		if err := r.saveSessionRuntime(cleanup, in.ConversationID, sessionRuntimeState{
			SessionID:      sessionID,
			NextTurn:       nextTurn,
			IdleDeadlineAt: idleDeadlineAt,
		}); err != nil {
			cancelCleanup()
			return err
		}
		cancelCleanup()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// reapOrphanedSessions runs once at startup to close active non-coding message
// sessions already past their idle deadline and restore remaining idle timers
// for active message sessions that have not yet expired.
func (r *Runtime) reapOrphanedSessions(ctx context.Context) error {
	if r.Repo == nil || r.IdleTimeout <= 0 || r.Activities == nil {
		return nil
	}
	sessions, err := r.Repo.ActiveSessions(ctx)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	pending, err := r.Repo.PendingIncoming(ctx, 100)
	if err != nil {
		return err
	}
	hasPending := map[string]bool{}
	for _, in := range pending {
		hasPending[in.Conversation.ID] = true
	}
	now := r.now().UTC()
	for _, s := range sessions {
		if strings.HasPrefix(s.ConversationID, "coding:") || hasPending[s.ConversationID] {
			continue
		}
		// Never adopt background task, initiative, backup, or coding sessions
		// as the interactive chat session for the shared conversation ID.
		if s.Trigger != "" && s.Trigger != "message" {
			continue
		}
		var state sessionRuntimeState
		hasState := false
		if raw, ok, e := r.Repo.LoadCheckpoint(ctx, sessionRuntimeKey(s.ConversationID)); e == nil && ok {
			if json.Unmarshal(raw, &state) == nil && state.SessionID == s.ID {
				hasState = true
			}
		}
		// Reconcile any completed failed turn before expiring the session so
		// its pending failure notice is delivered even after extended downtime.
		reconciled := false
		if hasState && state.ActiveTurnKey != "" {
			raw, done, e := r.Repo.LoadCheckpoint(ctx, "turn-done/"+state.ActiveTurnKey)
			if e != nil {
				r.record(ctx, "sessions.recover", e)
				continue
			}
			if done {
				var doneTurn agent.Turn
				if json.Unmarshal(raw, &doneTurn) == nil && doneTurn.Stats.FinishReason == "failed" && !doneTurn.Sent {
					if err := r.actNotifyFailure(ctx, s.ConversationID, state.ActiveTurnKey); err != nil {
						r.record(ctx, "sessions.recover", err)
						continue
					}
				}
				state.ActiveTurnKey = ""
				state.ActiveIncoming = nil
				state.ActiveStep = 0
				state.ActiveAfterModel = false
				state.ActiveToolIndex = 0
				state.ActiveSent = false
				state.ActiveStats = agent.TurnStats{}
				state.NextTurn++
				reconciled = true
			}
		}
		var deadline time.Time
		if hasState && !state.IdleDeadlineAt.IsZero() {
			deadline = state.IdleDeadlineAt.UTC()
		} else {
			lastActive := s.CreatedAt.UTC()
			if hasState && state.UpdatedAt.After(lastActive) {
				lastActive = state.UpdatedAt.UTC()
			}
			if !lastActive.IsZero() {
				deadline = lastActive.Add(r.IdleTimeout)
			}
		}
		if !deadline.IsZero() && !deadline.After(now) {
			if err := r.actCloseSession(ctx, s.ID); err != nil {
				r.record(ctx, "sessions.recover", err)
				continue
			}
			if hasState {
				r.clearSessionRuntime(ctx, s.ConversationID, s.ID)
			}
			continue
		}
		if !deadline.IsZero() {
			if reconciled || !hasState || state.IdleDeadlineAt.IsZero() {
				state.SessionID = s.ID
				state.IdleDeadlineAt = deadline
				if err := r.saveSessionRuntime(ctx, s.ConversationID, state); err != nil {
					r.record(ctx, "sessions.recover", err)
					continue
				}
			}
			r.ensureSessionActor(ctx, s.ConversationID)
		}
	}
	return nil
}

// ---- Turn execution ----

type turnSteerer func(*agent.Turn, int, func() error) error

type turnProgress struct {
	StartStep  int
	AfterModel bool
	StartTool  int
	Sent       bool
	Stats      agent.TurnStats
	OnUpdate   func(ctx context.Context, step int, afterModel bool, toolIdx int, sent bool, stats agent.TurnStats) error
}

func findAssistantToolCallsIndex(messages []domain.Message, target domain.Message) int {
	if len(target.ToolCalls) == 0 {
		return -1
	}
	firstID := target.ToolCalls[0].ID
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == firstID {
			return i
		}
	}
	return -1
}

func hasMessageContent(messages []domain.Message, target domain.Message) bool {
	for _, m := range messages {
		if m.Role == target.Role && m.Content == target.Content && len(m.Images) == len(target.Images) {
			return true
		}
	}
	return false
}

func (r *Runtime) prepareTurn(ctx context.Context, in agent.RunInput) (turn agent.Turn, err error) {
	turn.Input = in
	if len(in.IncomingIDs) > 0 {
		var jobs []agent.AudioInput
		jobs, err = r.actPrepareAudio(ctx, in)
		if err != nil {
			return turn, err
		}
		for i, job := range jobs {
			_, e := r.actTranscribeAudio(ctx, job)
			if e == nil {
				continue
			}
			code := "stt_failed"
			if c := domain.ErrorCode(e); c != "" {
				code = c
			}
			cleanup := ctx
			var cancelCleanup context.CancelFunc
			remaining := jobs[i : i+1]
			if isCanceled(e) {
				// Only mark audio as permanently cancelled on explicit user /stop or /new;
				// runtime shutdown leaves transcription resumable on restart.
				if !isUserCanceled(ctx) {
					return turn, e
				}
				code = "cancelled"
				cleanup, cancelCleanup = disconnectedCtx(ctx)
				remaining = jobs[i:]
			}
			for _, pending := range remaining {
				if saveErr := r.actRecordAudioFailure(cleanup, agent.AudioFailure{Input: pending, Code: code}); saveErr != nil {
					if cancelCleanup != nil {
						cancelCleanup()
					}
					return turn, saveErr
				}
			}
			if cancelCleanup != nil {
				cancelCleanup()
			}
			if isCanceled(e) {
				return turn, e
			}
		}
	}
	return r.actPrepare(ctx, in)
}

func (r *Runtime) runTurn(ctx context.Context, in agent.RunInput, maxSteps int, steer ...turnSteerer) (out agent.Turn, err error) {
	return r.runTurnWithProgress(ctx, in, maxSteps, nil, steer...)
}

func (r *Runtime) runTurnWithProgress(ctx context.Context, in agent.RunInput, maxSteps int, progress *turnProgress, steer ...turnSteerer) (out agent.Turn, err error) {
	if in.OriginalIncomingID == "" && len(in.IncomingIDs) > 0 {
		in.OriginalIncomingID = in.IncomingIDs[0]
	}
	in.StartedAt = r.now()
	defer func() {
		out.Messages = agent.CompactContext(out.Messages, out.Input.Key)
		if out.Stats.FinishReason == "" {
			out.Stats.FinishReason = "no_reply"
			if out.Sent {
				out.Stats.FinishReason = "reply_queued"
			}
			if err != nil {
				out.Stats.FinishReason = "failed"
			}
			if isCanceled(err) {
				out.Stats.FinishReason = "cancelled"
			}
		}
	}()
	turn, prepareErr := r.prepareTurn(ctx, in)
	if prepareErr != nil {
		return turn, prepareErr
	}
	if turn.Done {
		return turn, nil
	}
	startStep := 0
	startAfterModel := false
	startTool := 0
	if progress != nil {
		startStep = progress.StartStep
		startAfterModel = progress.AfterModel
		startTool = progress.StartTool
		turn.Sent = turn.Sent || progress.Sent
		turn.Stats = cloneTurnStats(progress.Stats)
	}
	saveProgress := func(step int, afterModel bool, toolIdx int, sent bool, stats agent.TurnStats) error {
		if progress == nil || progress.OnUpdate == nil {
			return nil
		}
		return progress.OnUpdate(ctx, step, afterModel, toolIdx, sent, stats)
	}
	codemodeTurn := turn.Codemode
	if len(steer) > 0 {
		steerTurn := steer[0]
		steer = []turnSteerer{func(turn *agent.Turn, resumeStep int, beforeSteer func() error) error {
			sent := turn.Sent
			err := steerTurn(turn, resumeStep, beforeSteer)
			turn.Sent = turn.Sent || sent
			return err
		}}
	}
steps:
	for step := startStep; step < maxSteps; step++ {
		resumingAfterModel := step == startStep && startAfterModel
		toolStart := 0
		if resumingAfterModel {
			toolStart = startTool
		}
		if !resumingAfterModel && len(steer) > 0 {
			before := len(turn.Input.IncomingIDs)
			beforeCalls := turn.Stats.ModelCalls
			if err := steer[0](&turn, step, nil); err != nil {
				return turn, err
			}
			if len(turn.Input.IncomingIDs) > before {
				// If a cached model output already exists at this step (e.g. a
				// follow-up arrived during restart preparation), advance past it
				// instead of replaying obsolete actions. Account for its usage
				// only if the steering callback did not already merge it.
				if cachedOut, ok, err := r.loadCachedModelOutput(ctx, in.Key, step, false); err != nil {
					return turn, err
				} else if ok {
					if turn.Stats.ModelCalls == beforeCalls {
						turn.Stats.ModelCalls++
						turn.Stats.InputTokens += cachedOut.InputTokens
						turn.Stats.OutputTokens += cachedOut.OutputTokens
						if err := saveProgress(step+1, false, 0, turn.Sent, turn.Stats); err != nil {
							return turn, err
						}
					}
					continue steps
				}
			}
		}
		if in.TaskID != "" {
			current, err := r.actGetTask(ctx, in.ConversationID, in.TaskID)
			if err != nil {
				return turn, err
			}
			if current.Status == "cancelled" || current.Status == "paused" {
				turn.Stats.FinishReason = current.Status
				return turn, nil
			}
		}
		turn.Messages = agent.CompactContext(turn.Messages, turn.Input.Key)
		if !resumingAfterModel {
			turn.Stats.ModelCalls++
		}
		result, err := r.actModelCall(ctx, agent.ModelInput{Turn: turn, Step: step, Codemode: codemodeTurn})
		if err != nil {
			return turn, err
		}
		if !resumingAfterModel {
			turn.Stats.InputTokens += result.InputTokens
			turn.Stats.OutputTokens += result.OutputTokens
		}
		if !resumingAfterModel && len(steer) > 0 {
			before := len(turn.Input.IncomingIDs)
			if err := steer[0](&turn, step+1, nil); err != nil {
				return turn, err
			}
			if len(turn.Input.IncomingIDs) > before {
				continue
			}
		}
		message := result.Message
		assistantIdx := -1
		if !resumingAfterModel {
			turn.Messages = append(turn.Messages, message)
			assistantIdx = len(turn.Messages) - 1
			if err := r.actPersistMessage(ctx, in.SessionID, fmt.Sprintf("%s/assistant/%d", in.Key, step), message); err != nil {
				return turn, err
			}
			if len(message.ToolCalls) == 0 {
				turn.LastText = message.Content
				return turn, nil
			}
			if err := saveProgress(step, true, 0, turn.Sent, turn.Stats); err != nil {
				return turn, err
			}
		} else {
			if len(message.ToolCalls) == 0 {
				turn.LastText = message.Content
				return turn, nil
			}
			// Prepare filters incomplete assistant+tool exchanges from history.
			// When resuming mid-batch, locate or re-attach the assistant message
			// (and any already-persisted tool results below) so the next model
			// call sees one complete exchange without duplicate tool responses.
			assistantIdx = findAssistantToolCallsIndex(turn.Messages, message)
			if assistantIdx < 0 {
				turn.Messages = append(turn.Messages, message)
				assistantIdx = len(turn.Messages) - 1
			}
		}
		if codemodeTurn {
			err := r.runCodemodeCalls(ctx, &turn, step, assistantIdx, message.ToolCalls, false, steer...)
			switch {
			case errors.Is(err, errCodemodeSteered):
				continue steps
			case errors.Is(err, errCodemodeTaskStopped):
				return turn, nil
			case err != nil:
				return turn, err
			}
			if err := saveProgress(step+1, false, 0, turn.Sent, turn.Stats); err != nil {
				return turn, err
			}
			continue steps
		}
		presentInExchange := map[string]bool{}
		for _, m := range turn.Messages[assistantIdx+1:] {
			if m.Role == "tool" && m.ToolCallID != "" {
				presentInExchange[m.ToolCallID] = true
			}
		}
		type pendingView struct {
			key string
			msg domain.Message
		}
		views := []pendingView{}
		if resumingAfterModel && toolStart > 0 && r.Repo != nil {
			persisted, err := r.Repo.Messages(ctx, in.SessionID)
			if err != nil {
				return turn, err
			}
			completedTools := map[string]domain.Message{}
			for _, m := range persisted {
				if m.Role == "tool" && m.ToolCallID != "" {
					completedTools[m.ToolCallID] = m
				}
			}
			for i := 0; i < toolStart && i < len(message.ToolCalls); i++ {
				call := message.ToolCalls[i]
				toolMsg, ok := completedTools[call.ID]
				if !ok {
					// Fall back to cached tool receipt if message was not persisted.
					outStr, err := r.executeTool(ctx, toolInput(turn, step, call))
					if err != nil {
						return turn, err
					}
					toolMsg = domain.Message{Role: "tool", Content: outStr, ToolCallID: call.ID}
					if err := r.actPersistMessage(ctx, in.SessionID, fmt.Sprintf("%s/tool/%d/%s", in.Key, step, call.ID), toolMsg); err != nil {
						return turn, err
					}
				}
				if !presentInExchange[call.ID] {
					turn.Messages = append(turn.Messages, toolMsg)
					presentInExchange[call.ID] = true
				}
				if call.Name == "artifact_read" && !strings.Contains(toolMsg.Content, `"error"`) {
					var args struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(call.Arguments, &args)
					view, err := r.actArtifactView(ctx, turn.Conversation.ID, args.ID)
					if err != nil {
						return turn, err
					}
					if len(view.Images) > 0 {
						views = append(views, pendingView{
							key: fmt.Sprintf("%s/image/%d/%s", in.Key, step, call.ID),
							msg: view,
						})
					}
				}
			}
		}
		for i, call := range message.ToolCalls {
			if i < toolStart {
				continue
			}
			if len(steer) > 0 {
				before := len(turn.Input.IncomingIDs)
				if err := steer[0](&turn, step+1, nil); err != nil {
					return turn, err
				}
				if len(turn.Input.IncomingIDs) > before {
					continue steps
				}
			}
			if in.TaskID != "" {
				current, err := r.actGetTask(ctx, in.ConversationID, in.TaskID)
				if err != nil {
					return turn, err
				}
				if current.Status == "cancelled" || current.Status == "paused" {
					turn.Stats.FinishReason = current.Status
					return turn, nil
				}
			}
			recordTool(&turn, call.Name)
			output, err := r.executeTool(ctx, toolInput(turn, step, call))
			if err != nil {
				turn.Stats.ToolErrors++
				return turn, err
			}
			if agent.ToolFailed(output) {
				turn.Stats.ToolErrors++
			}
			toolMessage := domain.Message{Role: "tool", Content: output, ToolCallID: call.ID}
			if !presentInExchange[call.ID] {
				turn.Messages = append(turn.Messages, toolMessage)
				presentInExchange[call.ID] = true
			}
			if err := r.actPersistMessage(ctx, in.SessionID, fmt.Sprintf("%s/tool/%d/%s", in.Key, step, call.ID), toolMessage); err != nil {
				return turn, err
			}
			if call.Name == "artifact_read" && !strings.Contains(output, `"error"`) {
				var args struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(call.Arguments, &args)
				view, err := r.actArtifactView(ctx, turn.Conversation.ID, args.ID)
				if err != nil {
					return turn, err
				}
				if len(view.Images) > 0 {
					views = append(views, pendingView{
						key: fmt.Sprintf("%s/image/%d/%s", in.Key, step, call.ID),
						msg: view,
					})
				}
			}
			if (domain.IsReplyTool(call.Name) || call.Name == "send_widget") && !agent.ToolFailed(output) {
				turn.Sent = true
			}
			if err := saveProgress(step, true, i+1, turn.Sent, turn.Stats); err != nil {
				return turn, err
			}
		}
		for _, pv := range views {
			if err := r.actPersistMessage(ctx, in.SessionID, pv.key, pv.msg); err != nil {
				return turn, err
			}
			// Deduplicate only within the current step's exchange (after assistantIdx)
			// so re-reading the same artifact in a later turn always appends a fresh
			// image view to that turn's protected context.
			if !hasMessageContent(turn.Messages[assistantIdx+1:], pv.msg) {
				turn.Messages = append(turn.Messages, pv.msg)
			}
		}
		if err := saveProgress(step+1, false, 0, turn.Sent, turn.Stats); err != nil {
			return turn, err
		}
	}
	if startStep > maxSteps {
		return turn, domain.NewNonRetryableError("agent step limit reached after new input", "step_limit", nil)
	}
	if in.TaskID == "" && !in.Initiative && !turn.Sent {
		return r.finalizeBudget(ctx, turn, maxSteps, codemodeTurn, saveProgress, steer...)
	}
	return turn, domain.NewNonRetryableError("agent step limit reached", "step_limit", nil)
}

// One reserved synthesis call can use existing evidence but cannot continue
// research or mutate task state. The caller retains NotifyFailure as a fallback.
func (r *Runtime) finalizeBudget(ctx context.Context, turn agent.Turn, step int, codemodeTurn bool, saveProgress func(int, bool, int, bool, agent.TurnStats) error, steer ...turnSteerer) (agent.Turn, error) {
	if len(steer) > 0 {
		before := len(turn.Input.IncomingIDs)
		beforeCalls := turn.Stats.ModelCalls
		if err := steer[0](&turn, step, nil); err != nil {
			return turn, err
		}
		if len(turn.Input.IncomingIDs) > before {
			if cachedOut, ok, err := r.loadCachedModelOutput(ctx, turn.Input.Key, step, true); err != nil {
				return turn, err
			} else if ok {
				if turn.Stats.ModelCalls == beforeCalls {
					turn.Stats.ModelCalls++
					turn.Stats.InputTokens += cachedOut.InputTokens
					turn.Stats.OutputTokens += cachedOut.OutputTokens
					if saveProgress != nil {
						if err := saveProgress(step+1, false, 0, turn.Sent, turn.Stats); err != nil {
							return turn, err
						}
					}
				}
				return turn, domain.NewNonRetryableError("agent step limit reached after new input", "step_limit", nil)
			}
		}
	}
	instruction := "The research step budget is exhausted. This is the single reserved finalization step. Stop researching and queue the remaining answer now from the evidence already collected. You may send multiple requested messages or artifacts in this final tool batch; do not resend parts already queued. Give a useful partial result when possible, clearly mark gaps and uncertainty, and never invent missing facts or claim unfinished work succeeded. If the evidence is insufficient, say what prevented completion. Only reply tools are available. Prefer send_message; choose send_rich_message only if rich formatting is needed, or send_voice when available and explicitly requested. Do not request other tools."
	if codemodeTurn {
		instruction += " Send the remaining parts in one codemode script using the appropriate reply tools."
	}
	turn.Messages = append(turn.Messages, domain.Message{Role: "user", Content: instruction})
	turn.Messages = agent.CompactContext(turn.Messages, turn.Input.Key)
	turn.Stats.ModelCalls++
	result, err := r.actModelCall(ctx, agent.ModelInput{Turn: turn, Step: step, Finalize: true, Codemode: codemodeTurn})
	if err != nil {
		return turn, err
	}
	turn.Stats.InputTokens += result.InputTokens
	turn.Stats.OutputTokens += result.OutputTokens
	if len(steer) > 0 {
		before := len(turn.Input.IncomingIDs)
		if err := steer[0](&turn, step+1, nil); err != nil {
			return turn, err
		}
		if len(turn.Input.IncomingIDs) > before {
			return turn, domain.NewNonRetryableError("agent step limit reached after new input", "step_limit", nil)
		}
	}
	message := result.Message
	assistantIdx := len(turn.Messages)
	turn.Messages = append(turn.Messages, message)
	if err := r.actPersistMessage(ctx, turn.Input.SessionID, fmt.Sprintf("%s/assistant/%d", turn.Input.Key, step), message); err != nil {
		return turn, err
	}
	if codemodeTurn {
		if err := r.runCodemodeCalls(ctx, &turn, step, assistantIdx, message.ToolCalls, true, steer...); err != nil {
			if errors.Is(err, errCodemodeSteered) {
				return turn, domain.NewNonRetryableError("agent step limit reached after new input", "step_limit", nil)
			}
			return turn, err
		}
		if turn.Sent {
			return turn, nil
		}
		return turn, domain.NewNonRetryableError("agent finalization did not queue a reply", "step_limit", nil)
	}
	for _, call := range message.ToolCalls {
		// Validate even a forged model response; advertised tools are not a guard.
		var output string
		if !domain.IsReplyTool(call.Name) {
			output = `{"error":"Finalization only permits reply tools; no further actions are executed."}`
		} else {
			recordTool(&turn, call.Name)
			var err error
			output, err = r.actToolCall(ctx, toolInput(turn, step, call))
			if err != nil {
				turn.Stats.ToolErrors++
				return turn, err
			}
			if agent.ToolFailed(output) {
				turn.Stats.ToolErrors++
			} else {
				turn.Sent = true
			}
		}
		toolMessage := domain.Message{Role: "tool", Content: output, ToolCallID: call.ID}
		turn.Messages = append(turn.Messages, toolMessage)
		if err := r.actPersistMessage(ctx, turn.Input.SessionID, fmt.Sprintf("%s/tool/%d/%s", turn.Input.Key, step, call.ID), toolMessage); err != nil {
			return turn, err
		}
	}
	if turn.Sent {
		return turn, nil
	}
	return turn, domain.NewNonRetryableError("agent finalization did not queue a reply", "step_limit", nil)
}

func toolInput(t agent.Turn, step int, call domain.ToolCall) agent.ToolInput {
	return agent.ToolInput{
		RunKey:        t.Input.Key,
		Step:          step,
		Call:          call,
		Conversation:  t.Conversation,
		SessionID:     t.Input.SessionID,
		TaskID:        t.Input.TaskID,
		Background:    t.Input.Initiative,
		StartedAt:     t.Input.StartedAt,
		ReceivedAt:    t.ReceivedAt,
		TelegramReply: t.TelegramReply,
	}
}

func (r *Runtime) executeTool(ctx context.Context, in agent.ToolInput) (string, error) {
	if strings.HasPrefix(in.Call.Name, "browser_") {
		return r.actBrowserToolCall(ctx, in)
	}
	return r.actToolCall(ctx, in)
}

func recordTool(t *agent.Turn, name string) {
	t.Stats.ToolCalls++
	for _, old := range t.Stats.Tools {
		if old == name {
			return
		}
	}
	t.Stats.Tools = append(t.Stats.Tools, name)
}

func finishInput(t agent.Turn, err error) agent.FinishInput {
	code := ""
	if err != nil {
		code = telemetry.Classify(err).Code
	}
	if isCanceled(err) {
		code = "cancelled"
	}
	return agent.FinishInput{Turn: t, Failed: err != nil, FailureCode: code}
}

// ---- DB-Driven TaskScheduler ----

// NotifyTaskChanged wakes the DB-driven task scheduler immediately after any
// task is created, modified, paused, resumed, or cancelled.
func (r *Runtime) NotifyTaskChanged(_ context.Context, _, _ string) error {
	r.ensureInit()
	select {
	case r.taskWake <- struct{}{}:
	default:
	}
	return nil
}

// recoverRunningTasks resets any tasks left in status "running" after an
// unclean process exit back to "waiting" while preserving NextRunAt and Result.
func (r *Runtime) recoverRunningTasks(ctx context.Context) error {
	if r.Repo == nil {
		return nil
	}
	tasks, err := r.Repo.RunnableTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Status != "running" {
			continue
		}
		t.Status = "waiting"
		if _, err := r.Repo.UpdateTask(ctx, t); err != nil {
			r.record(ctx, "tasks.recover", err)
		}
	}
	return nil
}

func (r *Runtime) runTaskScheduler(ctx context.Context) {
	r.ensureInit()
	for ctx.Err() == nil {
		wait, err := r.StepTasks(ctx)
		if err != nil && ctx.Err() == nil {
			r.record(ctx, "tasks.schedule", err)
			wait = 5 * time.Second
		}
		if wait <= 0 || wait > taskSchedulerMaxPoll {
			wait = taskSchedulerMaxPoll
		}
		timer := r.clock().NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.taskWake:
			timer.Stop()
		case <-timer.C():
			timer.Stop()
		}
	}
}

// StepTasks scans runnable tasks once, launches any due tasks that are not
// already active, and returns the duration until the earliest future task.
func (r *Runtime) StepTasks(ctx context.Context) (time.Duration, error) {
	r.ensureInit()
	if r.Repo == nil || r.Activities == nil {
		return taskSchedulerMaxPoll, nil
	}
	tasks, err := r.Repo.RunnableTasks(ctx)
	if err != nil {
		return 0, err
	}
	now := r.now().UTC()
	var earliest time.Duration
	hasFuture := false

	for _, t := range tasks {
		if t.Status == "completed" || t.Status == "cancelled" || t.Status == "failed" || t.Status == "paused" {
			continue
		}
		r.mu.Lock()
		running := r.activeTasks[t.ID]
		r.mu.Unlock()
		if running {
			continue
		}
		if !t.NextRunAt.IsZero() {
			wait := t.NextRunAt.Sub(now)
			if wait > 0 {
				if !hasFuture || wait < earliest {
					earliest = wait
					hasFuture = true
				}
				continue
			}
		}
		r.mu.Lock()
		if r.activeTasks[t.ID] {
			r.mu.Unlock()
			continue
		}
		r.activeTasks[t.ID] = true
		taskCtx := r.bgCtxLocked(ctx)
		r.mu.Unlock()

		in := TaskInput{
			ConversationID: t.ConversationID,
			ID:             t.ID,
			MaxSteps:       r.maxSteps(),
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() {
				r.mu.Lock()
				delete(r.activeTasks, in.ID)
				r.mu.Unlock()
				_ = r.NotifyTaskChanged(context.Background(), in.ConversationID, in.ID)
			}()
			if err := r.RunTaskOnce(taskCtx, in); err != nil && taskCtx.Err() == nil {
				r.record(taskCtx, "task.run", err)
			}
		}()
	}

	if hasFuture {
		return earliest, nil
	}
	return taskSchedulerMaxPoll, nil
}

// RunTaskOnce executes one due occurrence of a scheduled task:
// BeginTask -> runTurn -> Finish -> CloseSession -> AdvanceTask.
func (r *Runtime) RunTaskOnce(ctx context.Context, in TaskInput) error {
	r.ensureInit()
	ctx = telemetry.WithFields(ctx, map[string]any{"workflow_id": TaskWorkflowID(in.ID)})
	t, err := r.actBeginTask(ctx, in.ConversationID, in.ID, r.now().UTC())
	if err != nil {
		return err
	}
	if t.Status != "running" {
		return nil
	}
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = r.maxSteps()
	}
	slot := t.NextRunAt.UTC().Format(time.RFC3339Nano)
	session := agent.ID("task-run", in.ID, slot)
	run := agent.RunInput{
		Key:            session,
		SessionID:      session,
		ConversationID: in.ConversationID,
		TaskID:         in.ID,
	}
	turn, runErr := r.runTurn(ctx, run, maxSteps)
	// On graceful runtime shutdown, leave the task in "running" state without
	// advancing or failing it so startup recovery resumes this exact occurrence
	// with the same stable session/run key and receipts.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	cleanup, cancel := disconnectedCtx(ctx)
	defer cancel()
	if err := r.actFinish(cleanup, finishInput(turn, runErr)); err != nil {
		return err
	}
	if err := r.actCloseSession(cleanup, session); err != nil {
		return err
	}
	// Preserve a persisted failure outcome if a crash occurred between Finish
	// (which wrote turn-done with FinishReason="failed") and AdvanceTask.
	failed := runErr != nil || turn.Stats.FinishReason == "failed"
	_, err = r.actAdvanceTask(cleanup, in.ConversationID, in.ID, failed, r.now().UTC())
	return err
}

// ---- Post-session Analysis ----

type analysisRetryState struct {
	Attempts    int       `json:"attempts"`
	NextRetryAt time.Time `json:"next_retry_at,omitempty"`
	Terminal    bool      `json:"terminal,omitempty"`
}

func analysisRetryKey(sessionID string) string {
	return "analysis-retry/" + sessionID
}

func analysisBackoff(attempts int) time.Duration {
	delay := analysisRetryInitial
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= analysisRetryMax {
			return analysisRetryMax
		}
	}
	if delay > analysisRetryMax {
		return analysisRetryMax
	}
	return delay
}

// RunAnalysis runs post-session analysis for one closed session using the
// bounded analysis concurrency lane and activity retry policy.
func (r *Runtime) RunAnalysis(ctx context.Context, session domain.Session) error {
	r.ensureInit()
	ctx = telemetry.WithFields(ctx, map[string]any{"workflow_id": "analysis/" + session.ID})
	return r.actAnalyze(ctx, session)
}

func (r *Runtime) dispatchAnalysis(ctx context.Context) error {
	r.ensureInit()
	if r.Repo == nil || r.Activities == nil {
		return nil
	}
	sessions, err := r.Repo.PendingAnalysis(ctx, 50)
	if err != nil {
		return err
	}
	now := r.now().UTC()
	for _, s := range sessions {
		r.mu.Lock()
		active := r.activeAnalysis[s.ID]
		r.mu.Unlock()
		if active {
			continue
		}
		var state analysisRetryState
		if raw, ok, err := r.Repo.LoadCheckpoint(ctx, analysisRetryKey(s.ID)); err != nil {
			return err
		} else if ok {
			_ = json.Unmarshal(raw, &state)
			if state.Terminal {
				continue
			}
			if !state.NextRetryAt.IsZero() && now.Before(state.NextRetryAt) {
				continue
			}
		}
		r.mu.Lock()
		if r.activeAnalysis[s.ID] {
			r.mu.Unlock()
			continue
		}
		r.activeAnalysis[s.ID] = true
		runCtx := r.bgCtxLocked(ctx)
		r.mu.Unlock()

		sess := s
		prevState := state
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() {
				r.mu.Lock()
				delete(r.activeAnalysis, sess.ID)
				r.mu.Unlock()
			}()
			r.runAnalysisJob(runCtx, sess, prevState)
		}()
	}
	return nil
}

func (r *Runtime) runAnalysisJob(ctx context.Context, session domain.Session, state analysisRetryState) {
	err := r.RunAnalysis(ctx, session)
	cleanup, cancel := disconnectedCtx(ctx)
	defer cancel()
	if err == nil {
		if state.Attempts > 0 || state.Terminal {
			_ = r.Repo.DeleteCheckpointsByPrefix(cleanup, analysisRetryKey(session.ID))
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	r.record(ctx, "session.analyze", err)
	if domain.IsNonRetryable(err) || domain.ErrorCode(err) == agent.AnalysisContextLimitErrorType {
		code := domain.ErrorCode(err)
		if code == "" {
			code = "analysis_failed"
		}
		if markErr := r.Repo.MarkAnalysisFailed(cleanup, session.ID, code); markErr != nil {
			r.record(cleanup, "session.analyze.mark_failed", markErr)
			return
		}
		_ = r.Repo.DeleteCheckpointsByPrefix(cleanup, analysisRetryKey(session.ID))
		return
	}
	state.Attempts++
	state.NextRetryAt = r.now().UTC().Add(analysisBackoff(state.Attempts))
	if raw, marshalErr := json.Marshal(state); marshalErr == nil {
		_ = r.Repo.SaveCheckpoint(cleanup, analysisRetryKey(session.ID), raw)
	}
}

// ---- Periodic Initiative ----

func initiativeLastKey(convID string) string {
	return "initiative-last/" + agent.ID(convID)
}

func (r *Runtime) dispatchInitiative(ctx context.Context) error {
	r.ensureInit()
	if r.InitiativeInterval <= 0 || r.Repo == nil || r.Activities == nil {
		return nil
	}
	conversations, err := r.Repo.ListPrivateConversations(ctx)
	if err != nil {
		return err
	}
	now := r.now().UTC()
	for _, c := range conversations {
		r.mu.Lock()
		active := r.activeInitiative[c.ID]
		r.mu.Unlock()
		if active {
			continue
		}
		key := initiativeLastKey(c.ID)
		raw, ok, err := r.Repo.LoadCheckpoint(ctx, key)
		if err != nil {
			return err
		}
		if !ok {
			if stamped, mErr := json.Marshal(now); mErr == nil {
				if err := r.Repo.SaveCheckpoint(ctx, key, stamped); err != nil {
					return err
				}
			}
			continue
		}
		var lastRun time.Time
		if err := json.Unmarshal(raw, &lastRun); err != nil {
			lastRun = time.Time{}
		}
		if !lastRun.IsZero() && now.Sub(lastRun) < r.InitiativeInterval {
			continue
		}
		if stamped, mErr := json.Marshal(now); mErr == nil {
			if err := r.Repo.SaveCheckpoint(ctx, key, stamped); err != nil {
				return err
			}
		}

		r.mu.Lock()
		if r.activeInitiative[c.ID] {
			r.mu.Unlock()
			continue
		}
		r.activeInitiative[c.ID] = true
		runCtx := r.bgCtxLocked(ctx)
		r.mu.Unlock()

		convID := c.ID
		maxSteps := r.maxSteps()
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() {
				r.mu.Lock()
				delete(r.activeInitiative, convID)
				r.mu.Unlock()
			}()
			if err := r.RunInitiativeOnce(runCtx, convID, maxSteps); err != nil && runCtx.Err() == nil {
				r.record(runCtx, "initiative.run", err)
			}
		}()
	}
	return nil
}

// RunInitiativeOnce executes a single initiative check turn for a conversation
// and closes its background session afterward.
func (r *Runtime) RunInitiativeOnce(ctx context.Context, convID string, maxSteps int) error {
	r.ensureInit()
	if maxSteps <= 0 {
		maxSteps = r.maxSteps()
	}
	ctx = telemetry.WithFields(ctx, map[string]any{"workflow_id": "initiative/" + agent.ID(convID)})
	session := agent.ID("initiative", convID, rand.Text())
	turn, runErr := r.runTurn(ctx, agent.RunInput{
		Key:            session,
		SessionID:      session,
		ConversationID: convID,
		Initiative:     true,
	}, maxSteps)
	cleanup, cancel := disconnectedCtx(ctx)
	defer cancel()
	if err := r.actFinish(cleanup, finishInput(turn, runErr)); err != nil {
		return err
	}
	return r.actCloseSession(cleanup, session)
}
