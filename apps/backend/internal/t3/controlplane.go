// Package t3 implements the server-side T3 Code Control Plane: host registry,
// command broker with instant SSE pub/sub, thread tracking, event ingestion,
// and conversation/task waking.
package t3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/scheduling"
)

const (
	DefaultOnlineThreshold          = 3 * time.Minute
	DefaultDispatchTimeout          = 8 * time.Second
	DefaultKeepaliveInterval        = 15 * time.Second
	DefaultCommandRedeliveryTimeout = 30 * time.Second
)

var (
	ErrHostNotFound        = errors.New("T3 host not found")
	ErrCommandNotFound     = errors.New("T3 command not found")
	ErrThreadNotFound      = errors.New("T3 thread not found")
	ErrEnvironmentMismatch = domain.ErrT3EnvironmentMismatch
	ErrInvalidInput        = errors.New("invalid T3 request")
)

// EventObserver receives live updates from the T3 Control Plane (e.g. for
// Telegram workspace topic prefixes, pinned boards, and interactive buttons).
type EventObserver interface {
	OnThreadEvent(ctx context.Context, event domain.T3Event) error
	OnHostUpdated(ctx context.Context, host domain.T3Host) error
}

// HandledEventObserver may optionally be implemented by an EventObserver when
// it handles a thread_waiting_input event directly (for example, posting
// zero-LLM inline buttons) so ControlPlane skips queueing an LLM wake turn.
type HandledEventObserver interface {
	OnThreadEventHandled(ctx context.Context, event domain.T3Event) (handled bool, err error)
}

// Config configures a ControlPlane instance.
type Config struct {
	OwnerUserID              int64
	Logger                   *slog.Logger
	OnlineThreshold          time.Duration
	DispatchTimeout          time.Duration
	KeepaliveInterval        time.Duration
	CommandRedeliveryTimeout time.Duration
	TaskChanged              func(ctx context.Context, scope, id string) error
	Now                      func() time.Time
	Observers                []EventObserver
}

type commandOutcome struct {
	Command domain.T3Command
	Thread  *domain.T3Thread
}

// ControlPlane manages T3 hosts, command dispatch/streaming, thread state, and events.
type ControlPlane struct {
	store             domain.T3Store
	repo              domain.Repository
	ownerUserID       int64
	logger            *slog.Logger
	onlineThreshold   time.Duration
	dispatchTimeout   time.Duration
	keepaliveInterval time.Duration
	redeliveryTimeout time.Duration
	taskChanged       func(ctx context.Context, scope, id string) error
	now               func() time.Time

	mu            sync.RWMutex
	subscribers   map[string]map[chan struct{}]struct{}
	activeStreams map[string]int
	activeThreads map[string][]string
	waiters       map[string]map[chan commandOutcome]struct{}
	observers     []EventObserver
}

func New(store domain.T3Store, repo domain.Repository, cfg Config) *ControlPlane {
	if cfg.OnlineThreshold <= 0 {
		cfg.OnlineThreshold = DefaultOnlineThreshold
	}
	if cfg.DispatchTimeout <= 0 {
		cfg.DispatchTimeout = DefaultDispatchTimeout
	}
	if cfg.KeepaliveInterval <= 0 {
		cfg.KeepaliveInterval = DefaultKeepaliveInterval
	}
	if cfg.CommandRedeliveryTimeout <= 0 {
		cfg.CommandRedeliveryTimeout = DefaultCommandRedeliveryTimeout
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cp := &ControlPlane{
		store:             store,
		repo:              repo,
		ownerUserID:       cfg.OwnerUserID,
		logger:            cfg.Logger,
		onlineThreshold:   cfg.OnlineThreshold,
		dispatchTimeout:   cfg.DispatchTimeout,
		keepaliveInterval: cfg.KeepaliveInterval,
		redeliveryTimeout: cfg.CommandRedeliveryTimeout,
		taskChanged:       cfg.TaskChanged,
		now:               cfg.Now,
		subscribers:       map[string]map[chan struct{}]struct{}{},
		activeStreams:     map[string]int{},
		activeThreads:     map[string][]string{},
		waiters:           map[string]map[chan commandOutcome]struct{}{},
	}
	if len(cfg.Observers) > 0 {
		cp.observers = append([]EventObserver(nil), cfg.Observers...)
	}
	return cp
}

// AddObserver subscribes an EventObserver to host and thread updates.
func (cp *ControlPlane) AddObserver(obs EventObserver) {
	if obs == nil {
		return
	}
	cp.mu.Lock()
	cp.observers = append(cp.observers, obs)
	cp.mu.Unlock()
}

func (cp *ControlPlane) KeepaliveInterval() time.Duration {
	return cp.keepaliveInterval
}

func (cp *ControlPlane) DispatchTimeout() time.Duration {
	return cp.dispatchTimeout
}

func stableHostID(environmentID string) string {
	sum := sha256.Sum256([]byte("t3-host:" + environmentID))
	return "host-" + hex.EncodeToString(sum[:8])
}

func (cp *ControlPlane) isHostOnlineLocked(envID string, lastSeen, now time.Time) bool {
	if cp.activeStreams[envID] > 0 {
		return true
	}
	if lastSeen.IsZero() {
		return false
	}
	return now.Sub(lastSeen) <= cp.onlineThreshold
}

func (cp *ControlPlane) enrichHost(h domain.T3Host, now time.Time) domain.T3Host {
	cp.mu.RLock()
	h.Online = cp.isHostOnlineLocked(h.EnvironmentID, h.LastSeenAt, now)
	if threads := cp.activeThreads[h.EnvironmentID]; len(threads) > 0 {
		h.ActiveThreadIDs = append([]string(nil), threads...)
	}
	cp.mu.RUnlock()
	if h.Projects == nil {
		h.Projects = []domain.T3Project{}
	}
	if h.Providers == nil {
		h.Providers = []domain.T3Provider{}
	}
	return h
}

func (cp *ControlPlane) snapshotObservers() []EventObserver {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	if len(cp.observers) == 0 {
		return nil
	}
	return append([]EventObserver(nil), cp.observers...)
}

func (cp *ControlPlane) notifyHostUpdated(ctx context.Context, host domain.T3Host) {
	for _, obs := range cp.snapshotObservers() {
		if err := obs.OnHostUpdated(ctx, host); err != nil && cp.logger != nil {
			cp.logger.Warn("T3 observer OnHostUpdated failed", "environment_id", host.EnvironmentID, "error", err.Error())
		}
	}
}

func (cp *ControlPlane) notifyThreadEvent(ctx context.Context, event domain.T3Event) bool {
	suppressWake := false
	for _, obs := range cp.snapshotObservers() {
		if h, ok := obs.(HandledEventObserver); ok {
			handled, err := h.OnThreadEventHandled(ctx, event)
			if err != nil && cp.logger != nil {
				cp.logger.Warn("T3 observer OnThreadEvent failed", "event_id", event.EventID, "thread_id", event.ThreadID, "error", err.Error())
			}
			if handled && event.Kind == domain.T3EventThreadWaitingInput {
				suppressWake = true
			}
			continue
		}
		if err := obs.OnThreadEvent(ctx, event); err != nil && cp.logger != nil {
			cp.logger.Warn("T3 observer OnThreadEvent failed", "event_id", event.EventID, "thread_id", event.ThreadID, "error", err.Error())
		}
	}
	return suppressWake
}

func (cp *ControlPlane) notifyHandledObservers(ctx context.Context, event domain.T3Event) bool {
	if event.Kind != domain.T3EventThreadWaitingInput {
		return false
	}
	suppressWake := false
	for _, obs := range cp.snapshotObservers() {
		h, ok := obs.(HandledEventObserver)
		if !ok {
			continue
		}
		handled, err := h.OnThreadEventHandled(ctx, event)
		if err != nil && cp.logger != nil {
			cp.logger.Warn("T3 observer OnThreadEvent replay failed", "event_id", event.EventID, "thread_id", event.ThreadID, "error", err.Error())
		}
		if handled {
			suppressWake = true
		}
	}
	return suppressWake
}

// RegisterHostRequest matches POST /api/v1/t3/hosts/register.
type RegisterHostRequest struct {
	EnvironmentID string              `json:"environment_id"`
	Label         string              `json:"label"`
	Projects      []domain.T3Project  `json:"projects"`
	Providers     []domain.T3Provider `json:"providers"`
}

// RegisterHost registers or updates a T3 host's projects and model catalog.
func (cp *ControlPlane) RegisterHost(ctx context.Context, req RegisterHostRequest) (domain.T3Host, error) {
	envID := strings.TrimSpace(req.EnvironmentID)
	label := strings.TrimSpace(req.Label)
	if envID == "" || label == "" {
		return domain.T3Host{}, fmt.Errorf("%w: environment_id and label are required", ErrInvalidInput)
	}
	now := cp.now().UTC()
	host, err := cp.store.UpsertT3Host(ctx, domain.T3Host{
		HostID:        stableHostID(envID),
		EnvironmentID: envID,
		Label:         label,
		Projects:      req.Projects,
		Providers:     req.Providers,
		LastSeenAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		return domain.T3Host{}, err
	}
	host = cp.enrichHost(host, now)
	cp.notifyHostUpdated(ctx, host)
	return host, nil
}

// Heartbeat updates host liveness and active thread metadata.
func (cp *ControlPlane) Heartbeat(ctx context.Context, environmentID string, activeThreadIDs []string) (time.Time, error) {
	envID := strings.TrimSpace(environmentID)
	if envID == "" {
		return time.Time{}, fmt.Errorf("%w: environment_id is required", ErrInvalidInput)
	}
	now := cp.now().UTC()
	host, found, err := cp.store.TouchT3Host(ctx, envID, now)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, ErrHostNotFound
	}
	cp.mu.Lock()
	if len(activeThreadIDs) > 0 {
		cp.activeThreads[envID] = append([]string(nil), activeThreadIDs...)
	} else {
		delete(cp.activeThreads, envID)
	}
	cp.mu.Unlock()
	host = cp.enrichHost(host, now)
	cp.notifyHostUpdated(ctx, host)
	return now, nil
}

// ListHosts returns all registered hosts with dynamic online status.
func (cp *ControlPlane) ListHosts(ctx context.Context) ([]domain.T3Host, error) {
	hosts, err := cp.store.ListT3Hosts(ctx)
	if err != nil {
		return nil, err
	}
	now := cp.now().UTC()
	out := make([]domain.T3Host, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, cp.enrichHost(h, now))
	}
	return out, nil
}

// ResolveHost finds a host by host_id or environment_id with dynamic online status.
func (cp *ControlPlane) ResolveHost(ctx context.Context, hostOrEnvID string) (domain.T3Host, error) {
	id := strings.TrimSpace(hostOrEnvID)
	if id == "" {
		return domain.T3Host{}, fmt.Errorf("%w: host_id is required", ErrInvalidInput)
	}
	h, found, err := cp.store.T3Host(ctx, id)
	if err != nil {
		return domain.T3Host{}, err
	}
	if !found {
		return domain.T3Host{}, ErrHostNotFound
	}
	return cp.enrichHost(h, cp.now().UTC()), nil
}

// ListThreads returns tracked threads matching the filter.
func (cp *ControlPlane) ListThreads(ctx context.Context, filter domain.T3ThreadFilter) ([]domain.T3Thread, error) {
	return cp.store.ListT3Threads(ctx, filter)
}

// Thread returns a single tracked thread.
func (cp *ControlPlane) Thread(ctx context.Context, threadID string) (domain.T3Thread, error) {
	id := strings.TrimSpace(threadID)
	if id == "" {
		return domain.T3Thread{}, fmt.Errorf("%w: thread_id is required", ErrInvalidInput)
	}
	th, found, err := cp.store.T3Thread(ctx, id)
	if err != nil {
		return domain.T3Thread{}, err
	}
	if !found {
		return domain.T3Thread{}, ErrThreadNotFound
	}
	return th, nil
}

// RebindThreadConversation updates a tracked thread's bound conversation_id
// (e.g. after recreating a deleted Telegram forum topic) without altering status.
func (cp *ControlPlane) RebindThreadConversation(ctx context.Context, threadID, conversationID string) (domain.T3Thread, error) {
	id := strings.TrimSpace(threadID)
	convID := strings.TrimSpace(conversationID)
	if id == "" || convID == "" {
		return domain.T3Thread{}, fmt.Errorf("%w: thread_id and conversation_id are required", ErrInvalidInput)
	}
	existing, found, err := cp.store.T3Thread(ctx, id)
	if err != nil {
		return domain.T3Thread{}, err
	}
	if !found {
		return domain.T3Thread{}, ErrThreadNotFound
	}
	return cp.store.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:               existing.ThreadID,
		EnvironmentID:          existing.EnvironmentID,
		ConversationID:         convID,
		PreserveExistingStatus: true,
		At:                     cp.now().UTC(),
	})
}

// Command returns a single persisted T3 command by ID.
func (cp *ControlPlane) Command(ctx context.Context, commandID string) (domain.T3Command, bool, error) {
	id := strings.TrimSpace(commandID)
	if id == "" {
		return domain.T3Command{}, false, nil
	}
	return cp.store.T3Command(ctx, id)
}

// SubscribeCommands registers an SSE listener for environmentID. When a host
// reconnects with no other active stream, any unacknowledged dispatched
// commands are requeued under the same command IDs so partial-batch disconnects
// are recovered automatically.
func (cp *ControlPlane) SubscribeCommands(ctx context.Context, environmentID string) (<-chan struct{}, func(), error) {
	envID := strings.TrimSpace(environmentID)
	if envID == "" {
		return nil, nil, fmt.Errorf("%w: environment_id is required", ErrInvalidInput)
	}
	ch := make(chan struct{}, 1)
	cp.mu.Lock()
	if cp.subscribers[envID] == nil {
		cp.subscribers[envID] = map[chan struct{}]struct{}{}
	}
	cp.subscribers[envID][ch] = struct{}{}
	wasIdle := cp.activeStreams[envID] == 0
	cp.activeStreams[envID]++
	cp.mu.Unlock()

	now := cp.now().UTC()
	if wasIdle {
		if err := cp.store.RequeueDispatchedT3Commands(ctx, envID, now); err != nil {
			cp.mu.Lock()
			delete(cp.subscribers[envID], ch)
			if len(cp.subscribers[envID]) == 0 {
				delete(cp.subscribers, envID)
			}
			if cp.activeStreams[envID] > 1 {
				cp.activeStreams[envID]--
			} else {
				delete(cp.activeStreams, envID)
			}
			cp.mu.Unlock()
			return nil, nil, err
		}
	}
	if host, found, err := cp.store.TouchT3Host(ctx, envID, now); err == nil && found {
		cp.notifyHostUpdated(ctx, cp.enrichHost(host, now))
	}

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			cp.mu.Lock()
			if set := cp.subscribers[envID]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(cp.subscribers, envID)
				}
			}
			if cp.activeStreams[envID] > 1 {
				cp.activeStreams[envID]--
			} else {
				delete(cp.activeStreams, envID)
			}
			cp.mu.Unlock()
		})
	}
	return ch, unsubscribe, nil
}

func (cp *ControlPlane) notifySubscribers(environmentID string) {
	cp.mu.RLock()
	set := cp.subscribers[environmentID]
	for ch := range set {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	cp.mu.RUnlock()
}

// ClaimPendingCommands atomically transitions pending commands (and stale
// unacknowledged dispatched commands past CommandRedeliveryTimeout) to
// dispatched in FIFO order.
func (cp *ControlPlane) ClaimPendingCommands(ctx context.Context, environmentID string, limit int) ([]domain.T3Command, error) {
	envID := strings.TrimSpace(environmentID)
	if envID == "" {
		return nil, fmt.Errorf("%w: environment_id is required", ErrInvalidInput)
	}
	now := cp.now().UTC()
	var redeliveryBefore time.Time
	if cp.redeliveryTimeout > 0 {
		redeliveryBefore = now.Add(-cp.redeliveryTimeout)
	}
	return cp.store.ClaimPendingT3Commands(ctx, envID, limit, now, redeliveryBefore)
}

// RequeueCommand returns a dispatched command back to pending if delivery
// failed before the host could receive it.
func (cp *ControlPlane) RequeueCommand(ctx context.Context, commandID string) error {
	if strings.TrimSpace(commandID) == "" {
		return nil
	}
	return cp.store.RequeueT3Command(ctx, commandID, cp.now().UTC())
}

func (cp *ControlPlane) addWaiter(commandID string) (chan commandOutcome, func()) {
	ch := make(chan commandOutcome, 1)
	cp.mu.Lock()
	if cp.waiters[commandID] == nil {
		cp.waiters[commandID] = map[chan commandOutcome]struct{}{}
	}
	cp.waiters[commandID][ch] = struct{}{}
	cp.mu.Unlock()
	return ch, func() {
		cp.mu.Lock()
		if set := cp.waiters[commandID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(cp.waiters, commandID)
			}
		}
		cp.mu.Unlock()
	}
}

func (cp *ControlPlane) resolveWaiters(commandID string, outcome commandOutcome) {
	cp.mu.Lock()
	set := cp.waiters[commandID]
	delete(cp.waiters, commandID)
	cp.mu.Unlock()
	for ch := range set {
		select {
		case ch <- outcome:
		default:
		}
	}
}

// DispatchResult is returned by DispatchAndWait.
type DispatchResult struct {
	Status        string           `json:"status"`
	CommandID     string           `json:"command_id"`
	EnvironmentID string           `json:"environment_id"`
	HostID        string           `json:"host_id,omitempty"`
	HostOnline    bool             `json:"host_online"`
	Result        json.RawMessage  `json:"result,omitempty"`
	Error         string           `json:"error,omitempty"`
	Thread        *domain.T3Thread `json:"thread,omitempty"`
	Note          string           `json:"note,omitempty"`
}

// EnqueueCommand persists a command and wakes any active SSE stream for environmentID.
func (cp *ControlPlane) EnqueueCommand(ctx context.Context, commandID, environmentID, action string, payload any) (domain.T3Command, error) {
	envID := strings.TrimSpace(environmentID)
	if envID == "" {
		return domain.T3Command{}, fmt.Errorf("%w: environment_id is required", ErrInvalidInput)
	}
	if !domain.ValidT3CommandAction(action) {
		return domain.T3Command{}, fmt.Errorf("%w: unsupported command action %q", ErrInvalidInput, action)
	}
	if strings.TrimSpace(commandID) == "" {
		commandID = uuid.NewString()
	}
	var rawPayload json.RawMessage
	switch v := payload.(type) {
	case nil:
		rawPayload = json.RawMessage(`{}`)
	case json.RawMessage:
		rawPayload = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return domain.T3Command{}, err
		}
		rawPayload = b
	}
	now := cp.now().UTC()
	cmd, created, err := cp.store.CreateT3Command(ctx, domain.T3Command{
		ID:            commandID,
		EnvironmentID: envID,
		Action:        action,
		Payload:       rawPayload,
		Status:        domain.T3CommandStatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		return domain.T3Command{}, err
	}
	if created || cmd.Status == domain.T3CommandStatusPending {
		cp.notifySubscribers(envID)
	}
	return cmd, nil
}

// DispatchAndWait enqueues a command and waits up to timeout for a live host to
// post /commands/{id}/result. If the host is offline or does not reply before
// timeout, it returns a clean queued/dispatched status rather than an error.
func (cp *ControlPlane) DispatchAndWait(ctx context.Context, commandID, environmentID, action string, payload any, timeout time.Duration) (DispatchResult, error) {
	envID := strings.TrimSpace(environmentID)
	if envID == "" {
		return DispatchResult{}, fmt.Errorf("%w: environment_id is required", ErrInvalidInput)
	}
	if !domain.ValidT3CommandAction(action) {
		return DispatchResult{}, fmt.Errorf("%w: unsupported command action %q", ErrInvalidInput, action)
	}
	if strings.TrimSpace(commandID) == "" {
		commandID = uuid.NewString()
	}
	if timeout == 0 {
		timeout = cp.dispatchTimeout
	}

	now := cp.now().UTC()
	var hostID string
	var online bool
	if h, found, err := cp.store.T3Host(ctx, envID); err != nil {
		return DispatchResult{}, err
	} else if found {
		h = cp.enrichHost(h, now)
		hostID = h.HostID
		online = h.Online
	} else {
		cp.mu.RLock()
		online = cp.activeStreams[envID] > 0
		cp.mu.RUnlock()
	}

	// Register waiter before enqueueing so an immediate LAN reply can never race.
	waitCh, cleanup := cp.addWaiter(commandID)
	defer cleanup()

	cmd, err := cp.EnqueueCommand(ctx, commandID, envID, action, payload)
	if err != nil {
		return DispatchResult{}, err
	}
	if cmd.Status == domain.T3CommandStatusSucceeded || cmd.Status == domain.T3CommandStatusFailed {
		return cp.resultFromCommand(cmd, hostID, online, nil), nil
	}
	if !online || timeout < 0 {
		return DispatchResult{
			Status:        queuedOrDispatched(cmd.Status),
			CommandID:     cmd.ID,
			EnvironmentID: envID,
			HostID:        hostID,
			HostOnline:    online,
			Note:          "Host is currently offline; command is queued for delivery on reconnect.",
		}, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case outcome := <-waitCh:
		return cp.resultFromCommand(outcome.Command, hostID, true, outcome.Thread), nil
	case <-timer.C:
		latest, found, err := cp.store.T3Command(ctx, cmd.ID)
		if err == nil && found {
			if latest.Status == domain.T3CommandStatusSucceeded || latest.Status == domain.T3CommandStatusFailed {
				return cp.resultFromCommand(latest, hostID, online, nil), nil
			}
			cmd = latest
		}
		return DispatchResult{
			Status:        queuedOrDispatched(cmd.Status),
			CommandID:     cmd.ID,
			EnvironmentID: envID,
			HostID:        hostID,
			HostOnline:    online,
			Note:          "Timed out waiting for immediate host reply; command remains queued/dispatched.",
		}, nil
	case <-ctx.Done():
		return DispatchResult{}, ctx.Err()
	}
}

func queuedOrDispatched(status string) string {
	if status == domain.T3CommandStatusDispatched {
		return domain.T3CommandStatusDispatched
	}
	return "queued"
}

func (cp *ControlPlane) resultFromCommand(cmd domain.T3Command, hostID string, online bool, th *domain.T3Thread) DispatchResult {
	return DispatchResult{
		Status:        cmd.Status,
		CommandID:     cmd.ID,
		EnvironmentID: cmd.EnvironmentID,
		HostID:        hostID,
		HostOnline:    online,
		Result:        cmd.Result,
		Error:         cmd.ErrorText,
		Thread:        th,
	}
}

// CommandResultRequest matches POST /api/v1/t3/commands/{command_id}/result.
type CommandResultRequest struct {
	EnvironmentID string          `json:"environment_id"`
	OK            bool            `json:"ok"`
	Result        json.RawMessage `json:"result"`
	Error         string          `json:"error"`
}

// RecordCommandResult atomically completes a command and applies its thread
// update on the first terminal transition, flushes any deferred actionable
// event wakes once ownership is established, and unblocks waiting callers.
func (cp *ControlPlane) RecordCommandResult(ctx context.Context, commandID string, req CommandResultRequest) error {
	cmdID := strings.TrimSpace(commandID)
	envID := strings.TrimSpace(req.EnvironmentID)
	if cmdID == "" || envID == "" {
		return fmt.Errorf("%w: command_id and environment_id are required", ErrInvalidInput)
	}
	now := cp.now().UTC()
	cmd, updatedThread, found, transitioned, err := cp.store.CompleteT3Command(
		ctx, cmdID, envID, req.OK, req.Result, req.Error, now,
		func(c domain.T3Command) *domain.T3CommandThreadEffect {
			return buildCommandThreadEffect(c, now)
		},
	)
	if err != nil {
		return err
	}
	if !found {
		return ErrCommandNotFound
	}
	_, _, _ = cp.store.TouchT3Host(ctx, envID, now)

	if updatedThread == nil && cmd.Action == domain.T3ActionThreadLaunch && cmd.Status == domain.T3CommandStatusSucceeded {
		var res struct {
			ThreadID string `json:"thread_id"`
		}
		if len(cmd.Result) > 0 && json.Unmarshal(cmd.Result, &res) == nil && strings.TrimSpace(res.ThreadID) != "" {
			th, ok, err := cp.store.T3Thread(ctx, strings.TrimSpace(res.ThreadID))
			if err != nil {
				return err
			}
			if ok {
				updatedThread = &th
			}
		}
	}
	if transitioned && updatedThread != nil {
		cp.notifyCommandThreadUpdate(ctx, cmd, *updatedThread, now)
	} else if !transitioned && cmd.Action == domain.T3ActionThreadRespond && cmd.Status == domain.T3CommandStatusSucceeded {
		// Replaying a succeeded thread_respond result (e.g. after a crash between
		// DB commit and observer delivery) retries idempotent observer finalization.
		var payload struct {
			ThreadID string `json:"thread_id"`
		}
		if len(cmd.Payload) > 0 && json.Unmarshal(cmd.Payload, &payload) == nil && strings.TrimSpace(payload.ThreadID) != "" {
			if th, ok, err := cp.store.T3Thread(ctx, strings.TrimSpace(payload.ThreadID)); err == nil && ok {
				cp.notifyCommandThreadUpdate(ctx, cmd, th, now)
			}
		}
	}
	if updatedThread != nil && updatedThread.ConversationID != "" {
		if err := cp.flushPendingWakes(ctx, *updatedThread, now, transitioned); err != nil {
			if transitioned {
				cp.resolveWaiters(cmd.ID, commandOutcome{Command: cmd, Thread: updatedThread})
			}
			return err
		}
	}
	if transitioned {
		cp.resolveWaiters(cmd.ID, commandOutcome{Command: cmd, Thread: updatedThread})
	}
	return nil
}

func (cp *ControlPlane) notifyCommandThreadUpdate(ctx context.Context, cmd domain.T3Command, thread domain.T3Thread, now time.Time) {
	if cmd.Status != domain.T3CommandStatusSucceeded {
		return
	}
	switch cmd.Action {
	case domain.T3ActionThreadLaunch:
		switch strings.ToLower(strings.TrimSpace(thread.Status)) {
		case "", "running", "starting", "queued":
			cp.notifyThreadEvent(ctx, domain.T3Event{
				EventID:            "cmd:" + cmd.ID,
				EnvironmentID:      thread.EnvironmentID,
				ThreadID:           thread.ThreadID,
				Kind:               domain.T3EventThreadLaunched,
				Status:             firstNonEmpty(thread.Status, "running"),
				Title:              thread.Title,
				Link:               thread.Link,
				ProjectID:          thread.ProjectID,
				ProviderInstanceID: thread.ProviderInstanceID,
				Model:              thread.Model,
				Branch:             thread.Branch,
				PRURL:              thread.PRURL,
				Summary:            thread.LastSummary,
				OccurredAt:         now,
				ConversationID:     thread.ConversationID,
				TaskID:             thread.TaskID,
			})
		}
	case domain.T3ActionThreadControl:
		cp.notifyThreadEvent(ctx, domain.T3Event{
			EventID:            "cmd:" + cmd.ID,
			EnvironmentID:      thread.EnvironmentID,
			ThreadID:           thread.ThreadID,
			Kind:               domain.T3EventThreadProgress,
			Status:             thread.Status,
			Title:              thread.Title,
			Link:               thread.Link,
			ProjectID:          thread.ProjectID,
			ProviderInstanceID: thread.ProviderInstanceID,
			Model:              thread.Model,
			Branch:             thread.Branch,
			PRURL:              thread.PRURL,
			Summary:            thread.LastSummary,
			OccurredAt:         now,
			ConversationID:     thread.ConversationID,
			TaskID:             thread.TaskID,
		})
	case domain.T3ActionThreadRespond:
		cp.notifyThreadEvent(ctx, domain.T3Event{
			EventID:            "cmd:" + cmd.ID,
			EnvironmentID:      thread.EnvironmentID,
			ThreadID:           thread.ThreadID,
			Kind:               domain.T3EventThreadProgress,
			Status:             thread.Status,
			Title:              thread.Title,
			Link:               thread.Link,
			ProjectID:          thread.ProjectID,
			ProviderInstanceID: thread.ProviderInstanceID,
			Model:              thread.Model,
			Branch:             thread.Branch,
			PRURL:              thread.PRURL,
			Summary:            thread.LastSummary,
			OccurredAt:         now,
			ConversationID:     thread.ConversationID,
			TaskID:             thread.TaskID,
		})
	}
}

func buildCommandThreadEffect(cmd domain.T3Command, now time.Time) *domain.T3CommandThreadEffect {
	switch cmd.Action {
	case domain.T3ActionThreadLaunch:
		var payload struct {
			ProjectID      string          `json:"project_id"`
			Title          string          `json:"title"`
			ConversationID string          `json:"conversation_id"`
			TaskID         string          `json:"task_id"`
			ModelSelection json.RawMessage `json:"model_selection"`
		}
		_ = json.Unmarshal(cmd.Payload, &payload)

		var res struct {
			ThreadID           string          `json:"thread_id"`
			Link               string          `json:"link"`
			Status             string          `json:"status"`
			Branch             string          `json:"branch"`
			Model              string          `json:"model"`
			ProviderInstanceID string          `json:"provider_instance_id"`
			ProjectID          string          `json:"project_id"`
			Title              string          `json:"title"`
			PRURL              string          `json:"pr_url"`
			Summary            string          `json:"summary"`
			Output             json.RawMessage `json:"output"`
		}
		if len(cmd.Result) > 0 {
			_ = json.Unmarshal(cmd.Result, &res)
		}
		if strings.TrimSpace(res.ThreadID) == "" {
			return nil
		}
		if res.ProjectID == "" {
			res.ProjectID = payload.ProjectID
		}
		if res.Title == "" {
			res.Title = payload.Title
		}
		if res.Model == "" || res.ProviderInstanceID == "" {
			var ms struct {
				Model              string `json:"model"`
				ProviderInstanceID string `json:"provider_instance_id"`
				Provider           string `json:"provider"`
				InstanceID         string `json:"instanceId"`
			}
			if json.Unmarshal(payload.ModelSelection, &ms) == nil {
				if res.Model == "" {
					res.Model = ms.Model
				}
				if res.ProviderInstanceID == "" {
					res.ProviderInstanceID = firstNonEmpty(ms.ProviderInstanceID, ms.InstanceID, ms.Provider)
				}
			}
		}
		if res.Status == "" {
			res.Status = "running"
		}
		patch := domain.T3ThreadPatch{
			ThreadID:               res.ThreadID,
			EnvironmentID:          cmd.EnvironmentID,
			ConversationID:         payload.ConversationID,
			TaskID:                 payload.TaskID,
			ProjectID:              res.ProjectID,
			Title:                  res.Title,
			Link:                   res.Link,
			ProviderInstanceID:     res.ProviderInstanceID,
			Model:                  res.Model,
			Branch:                 res.Branch,
			Status:                 res.Status,
			PRURL:                  res.PRURL,
			PreserveExistingStatus: true,
			LastSummary:            res.Summary,
			At:                     now,
		}
		if hasJSONValue(res.Output) {
			patch.SetLastOutput = true
			patch.LastOutput = res.Output
		}
		return &domain.T3CommandThreadEffect{Patch: &patch}

	case domain.T3ActionThreadRespond:
		var payload struct {
			ThreadID  string `json:"thread_id"`
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(cmd.Payload, &payload)
		if strings.TrimSpace(payload.ThreadID) == "" {
			return nil
		}
		return &domain.T3CommandThreadEffect{
			ClearThreadID:  strings.TrimSpace(payload.ThreadID),
			ClearRequestID: strings.TrimSpace(payload.RequestID),
		}

	case domain.T3ActionThreadSend, domain.T3ActionThreadControl:
		var payload struct {
			ThreadID string `json:"thread_id"`
			Action   string `json:"action"`
		}
		_ = json.Unmarshal(cmd.Payload, &payload)
		var res struct {
			ThreadID string `json:"thread_id"`
			Status   string `json:"status"`
			Title    string `json:"title"`
			Link     string `json:"link"`
			Branch   string `json:"branch"`
			PRURL    string `json:"pr_url"`
			Summary  string `json:"summary"`
		}
		if len(cmd.Result) > 0 {
			_ = json.Unmarshal(cmd.Result, &res)
		}
		threadID := firstNonEmpty(res.ThreadID, payload.ThreadID)
		if threadID == "" {
			return nil
		}
		if cmd.Action == domain.T3ActionThreadControl && res.Status == "" {
			switch payload.Action {
			case "archive":
				res.Status = "archived"
			case "settle":
				res.Status = "settled"
			case "interrupt":
				res.Status = "interrupted"
			}
		}
		if res.Status == "" && res.Title == "" && res.Link == "" && res.Branch == "" && res.PRURL == "" && res.Summary == "" {
			return nil
		}
		patch := domain.T3ThreadPatch{
			ThreadID:      threadID,
			EnvironmentID: cmd.EnvironmentID,
			Status:        res.Status,
			Title:         res.Title,
			Link:          res.Link,
			Branch:        res.Branch,
			PRURL:         res.PRURL,
			LastSummary:   res.Summary,
			At:            now,
		}
		return &domain.T3CommandThreadEffect{Patch: &patch}
	}
	return nil
}

// IngestEvent deduplicates an event by event_id, updates openalbert_t3_threads
// when the event is current, notifies EventObservers, and delivers owner wakes
// on actionable events (retrying failed wakes idempotently on duplicate retries
// and deferring wakes until launch ownership is established).
func (cp *ControlPlane) IngestEvent(ctx context.Context, event domain.T3Event, rawPayload json.RawMessage) (deduplicated bool, err error) {
	event.EventID = strings.TrimSpace(event.EventID)
	event.EnvironmentID = strings.TrimSpace(event.EnvironmentID)
	event.ThreadID = strings.TrimSpace(event.ThreadID)
	event.Kind = strings.TrimSpace(event.Kind)
	if event.EventID == "" || event.EnvironmentID == "" || event.ThreadID == "" || !domain.ValidT3EventKind(event.Kind) {
		return false, fmt.Errorf("%w: event_id, environment_id, thread_id, and valid kind are required", ErrInvalidInput)
	}
	now := cp.now().UTC()
	occurredAt := event.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = now
		event.OccurredAt = occurredAt
	}
	status := effectiveStatusForEvent(event.Kind, event.Status)
	event.Status = status
	// Ownership comes strictly from the persisted thread, never from event input.
	event.ConversationID = ""
	event.TaskID = ""
	patch := domain.T3ThreadPatch{
		ThreadID:           event.ThreadID,
		EnvironmentID:      event.EnvironmentID,
		ProjectID:          event.ProjectID,
		Title:              event.Title,
		Link:               event.Link,
		ProviderInstanceID: event.ProviderInstanceID,
		Model:              event.Model,
		Branch:             event.Branch,
		Status:             status,
		PRURL:              event.PRURL,
		RequireCurrent:     true,
		LastSummary:        event.Summary,
		At:                 occurredAt,
	}
	if hasJSONValue(event.Output) {
		patch.SetLastOutput = true
		patch.LastOutput = event.Output
	}
	switch event.Kind {
	case domain.T3EventThreadWaitingInput:
		patch.SetPendingRequest = true
		patch.PendingRequest = event.PendingRequest
	case domain.T3EventThreadCompleted:
		patch.SetPendingRequest = true
		patch.PendingRequest = nil
		patch.SetLastError = true
		patch.LastError = event.Error
	case domain.T3EventThreadFailed:
		patch.SetPendingRequest = true
		patch.PendingRequest = nil
		patch.SetLastError = true
		patch.LastError = event.Error
	case domain.T3EventThreadProgress:
		if event.PendingRequest != nil {
			patch.SetPendingRequest = true
			patch.PendingRequest = event.PendingRequest
		} else if status == "running" || status == "starting" || status == "queued" {
			patch.SetPendingRequest = true
			patch.PendingRequest = nil
		}
		if event.Error != "" {
			patch.SetLastError = true
			patch.LastError = event.Error
		}
	case domain.T3EventThreadLaunched:
		if event.PendingRequest != nil {
			patch.SetPendingRequest = true
			patch.PendingRequest = event.PendingRequest
		}
		if event.Error != "" {
			patch.SetLastError = true
			patch.LastError = event.Error
		}
	}

	// Persist the normalized event (resolved occurred_at, normalized pending_request,
	// stripped owner fields) so PostgreSQL replay never fails on empty timestamps or
	// untrusted owner attributes.
	normalizedPayload, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	thread, dup, stored, err := cp.store.RecordT3EventAndUpsertThread(ctx, event, normalizedPayload, patch, now)
	if err != nil {
		return false, err
	}
	if dup {
		// Retry any previously failed wake idempotently by event_id without
		// re-mutating the thread or re-invoking standard observers.
		if isActionableEvent(stored.Event.Kind) && stored.WokenAt == nil {
			enriched := enrichEventWithThread(stored.Event, thread)
			if cp.notifyHandledObservers(ctx, enriched) {
				if err := cp.store.MarkT3EventWoken(ctx, stored.Event.EventID, now); err != nil {
					return true, err
				}
				return true, nil
			}
			if err := cp.deliverEventWake(ctx, enriched, thread, now); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	_, _, _ = cp.store.TouchT3Host(ctx, event.EnvironmentID, now)

	enriched := enrichEventWithThread(event, thread)
	suppressWake := cp.notifyThreadEvent(ctx, enriched)

	// Observers (such as T3WorkspaceBridge) may recreate a deleted Telegram topic
	// and rebind thread.ConversationID before owner wakes are queued.
	if refreshed, found, refreshErr := cp.store.T3Thread(ctx, event.ThreadID); refreshErr == nil && found {
		thread = refreshed
		enriched = enrichEventWithThread(event, thread)
	}

	if isActionableEvent(enriched.Kind) {
		if suppressWake {
			if err := cp.store.MarkT3EventWoken(ctx, event.EventID, now); err != nil {
				return false, err
			}
			return false, nil
		}
		if err := cp.deliverEventWake(ctx, enriched, thread, now); err != nil {
			return false, err
		}
	}
	return false, nil
}

func enrichEventWithThread(event domain.T3Event, thread domain.T3Thread) domain.T3Event {
	enriched := event
	// Always derive owner routing exclusively from the persisted thread.
	enriched.ConversationID = strings.TrimSpace(thread.ConversationID)
	enriched.TaskID = strings.TrimSpace(thread.TaskID)
	enriched.ProjectID = firstNonEmpty(event.ProjectID, thread.ProjectID)
	enriched.Title = firstNonEmpty(event.Title, thread.Title)
	enriched.Link = firstNonEmpty(event.Link, thread.Link)
	enriched.ProviderInstanceID = firstNonEmpty(event.ProviderInstanceID, thread.ProviderInstanceID)
	enriched.Model = firstNonEmpty(event.Model, thread.Model)
	enriched.Branch = firstNonEmpty(event.Branch, thread.Branch)
	enriched.PRURL = firstNonEmpty(event.PRURL, thread.PRURL)
	enriched.Status = effectiveStatusForEvent(enriched.Kind, enriched.Status)
	return enriched
}

func (cp *ControlPlane) flushPendingWakes(ctx context.Context, thread domain.T3Thread, now time.Time, notifyObservers bool) error {
	pending, err := cp.store.PendingWakeT3Events(ctx, thread.ThreadID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, ev := range pending {
		enriched := enrichEventWithThread(ev, thread)
		suppressWake := false
		if notifyObservers {
			suppressWake = cp.notifyThreadEvent(ctx, enriched)
		} else {
			suppressWake = cp.notifyHandledObservers(ctx, enriched)
		}
		if suppressWake {
			if err := cp.store.MarkT3EventWoken(ctx, ev.EventID, now); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := cp.deliverEventWake(ctx, enriched, thread, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// deliverEventWake wakes the owning conversation/task if ownership is known
// and marks the event woken in storage. If no owner is known yet (event arrived
// before launch result), it leaves woken_at NULL so RecordCommandResult flushes
// it once ownership is established.
func (cp *ControlPlane) deliverEventWake(ctx context.Context, event domain.T3Event, thread domain.T3Thread, now time.Time) error {
	if cp.repo == nil {
		return cp.store.MarkT3EventWoken(ctx, event.EventID, now)
	}
	if strings.TrimSpace(thread.ConversationID) == "" {
		// Retain pending wake until launch result establishes ownership.
		return nil
	}
	woken, err := cp.wakeOwner(ctx, event, thread, now)
	if err != nil {
		return err
	}
	if woken {
		return cp.store.MarkT3EventWoken(ctx, event.EventID, now)
	}
	return nil
}

func effectiveStatusForEvent(kind, rawStatus string) string {
	st := strings.TrimSpace(rawStatus)
	lower := strings.ToLower(st)
	switch kind {
	case domain.T3EventThreadCompleted:
		if lower == "" || lower == "idle" {
			return "completed"
		}
	case domain.T3EventThreadFailed:
		if lower == "" || lower == "idle" {
			return "failed"
		}
	case domain.T3EventThreadWaitingInput:
		if lower == "" || lower == "idle" {
			return "waiting_input"
		}
	default:
		if st == "" {
			return defaultStatusForEvent(kind)
		}
	}
	return st
}

func defaultStatusForEvent(kind string) string {
	switch kind {
	case domain.T3EventThreadLaunched, domain.T3EventThreadProgress:
		return "running"
	case domain.T3EventThreadWaitingInput:
		return "waiting_input"
	case domain.T3EventThreadCompleted:
		return "completed"
	case domain.T3EventThreadFailed:
		return "failed"
	default:
		return ""
	}
}

func isActionableEvent(kind string) bool {
	switch kind {
	case domain.T3EventThreadWaitingInput, domain.T3EventThreadCompleted, domain.T3EventThreadFailed:
		return true
	default:
		return false
	}
}

func (cp *ControlPlane) wakeOwner(ctx context.Context, event domain.T3Event, thread domain.T3Thread, now time.Time) (bool, error) {
	if cp.repo == nil {
		return true, nil
	}
	convID := strings.TrimSpace(thread.ConversationID)
	taskID := strings.TrimSpace(thread.TaskID)
	if convID == "" {
		return false, nil
	}

	taskContextID := ""
	if taskID != "" {
		var lastErr error
		for attempt := range 3 {
			task, found, err := cp.repo.Task(ctx, convID, taskID)
			if err != nil {
				return false, err
			}
			if !found || (task.Status != "queued" && task.Status != "waiting" && task.Status != "running") {
				lastErr = nil
				break
			}
			// Repeating tasks (interval or cron) anchor their next slot on NextRunAt.
			// Already-running one-time tasks are not waiting on TaskWorkflow's timer
			// signal and could complete without seeing TaskChanged. Route both through
			// the durable conversation inbox with TaskContextID so the event is never
			// lost and scheduled slots never drift.
			if scheduling.Repeats(task) || task.Status == "running" {
				taskContextID = task.ID
				if scheduling.Repeats(task) && !task.NextRunAt.IsZero() && !task.NextRunAt.After(now) && cp.taskChanged != nil {
					if err := cp.taskChanged(ctx, convID, taskID); err != nil {
						return false, err
					}
				}
				lastErr = nil
				break
			}
			// Idle one-time task (queued/waiting): pull NextRunAt forward to run immediately.
			if task.NextRunAt.IsZero() || task.NextRunAt.After(now) {
				task.NextRunAt = now
				task.UpdatedAt = now
				if _, err := cp.repo.UpdateTask(ctx, task); err != nil {
					lastErr = err
					if attempt < 2 {
						continue
					}
					return false, err
				}
			}
			if cp.taskChanged != nil {
				if err := cp.taskChanged(ctx, convID, taskID); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		if lastErr != nil {
			return false, lastErr
		}
	}

	// Wake the owning conversation via the durable incoming inbox.
	conv, ok, err := cp.repo.Conversation(ctx, convID)
	if err != nil {
		return false, err
	}
	if !ok {
		if cp.ownerUserID > 0 && convID == fmt.Sprintf("personal:%d", cp.ownerUserID) {
			conv = domain.Conversation{
				ID:      convID,
				ChatID:  cp.ownerUserID,
				OwnerID: cp.ownerUserID,
				Private: true,
			}
		} else {
			return true, nil
		}
	}
	conv.DeliveryChannel = ""
	createdAt := event.OccurredAt
	if createdAt.IsZero() {
		createdAt = now
	}
	in := domain.Incoming{
		ID:            "t3-event:" + event.EventID,
		Source:        "t3",
		Conversation:  conv,
		TaskContextID: taskContextID,
		Text:          formatWakeMessage(event),
		CreatedAt:     createdAt,
	}
	if _, err := cp.repo.AcceptIncoming(ctx, in, 0); err != nil {
		return false, err
	}
	return true, nil
}

func formatWakeMessage(event domain.T3Event) string {
	var b strings.Builder
	b.WriteString("[T3 Code thread event (operational update, not a user command)]\n")
	fmt.Fprintf(&b, "event_id: %s\n", event.EventID)
	fmt.Fprintf(&b, "kind: %s\n", event.Kind)
	fmt.Fprintf(&b, "thread_id: %s\n", event.ThreadID)
	fmt.Fprintf(&b, "environment_id: %s\n", event.EnvironmentID)
	if event.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", event.Status)
	}
	if event.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", clipText(event.Title, 300))
	}
	if event.Link != "" {
		fmt.Fprintf(&b, "link: %s\n", event.Link)
	}
	if event.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", event.Branch)
	}
	if event.PRURL != "" {
		fmt.Fprintf(&b, "pr_url: %s\n", event.PRURL)
	}
	if event.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", clipText(event.Summary, 4000))
	}
	if event.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", clipText(event.Error, 2000))
	}
	if event.PendingRequest != nil {
		if raw, err := json.Marshal(event.PendingRequest); err == nil {
			fmt.Fprintf(&b, "pending_request: %s\n", clipText(string(raw), 4000))
		}
	}
	if hasJSONValue(event.Output) {
		fmt.Fprintf(&b, "output: %s\n", clipText(strings.TrimSpace(string(event.Output)), 4000))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func hasJSONValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func clipText(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "...[truncated]"
}
