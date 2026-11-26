package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
)

var _ domain.T3Store = (*Memory)(nil)

type memoryT3Event struct {
	event      domain.T3Event
	raw        json.RawMessage
	receivedAt time.Time
	wokenAt    *time.Time
}

type memoryT3 struct {
	hosts           map[string]domain.T3Host // keyed by environment_id
	hostOrder       []string
	commands        map[string]domain.T3Command
	commandOrder    []string
	commandSeq      int64
	threads         map[string]domain.T3Thread
	threadLastEvent map[string]time.Time
	events          map[string]memoryT3Event
	eventOrder      []string
}

func (m *Memory) ensureT3() *memoryT3 {
	if m.t3 == nil {
		m.t3 = &memoryT3{
			hosts:           map[string]domain.T3Host{},
			commands:        map[string]domain.T3Command{},
			threads:         map[string]domain.T3Thread{},
			threadLastEvent: map[string]time.Time{},
			events:          map[string]memoryT3Event{},
		}
	}
	return m.t3
}

func (m *Memory) UpsertT3Host(ctx context.Context, host domain.T3Host) (domain.T3Host, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Host{}, err
	}
	if err := validID(host.EnvironmentID); err != nil {
		return domain.T3Host{}, fmt.Errorf("environment_id: %w", err)
	}
	if err := validID(host.HostID); err != nil {
		return domain.T3Host{}, fmt.Errorf("host_id: %w", err)
	}
	if strings.TrimSpace(host.Label) == "" {
		return domain.T3Host{}, errors.New("label is required")
	}
	now := nowIfZero(host.UpdatedAt).UTC()
	lastSeen := nowIfZero(host.LastSeenAt).UTC()
	created := nowIfZero(host.CreatedAt).UTC()

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	saved := clone(host)
	if saved.Projects == nil {
		saved.Projects = []domain.T3Project{}
	}
	if saved.Providers == nil {
		saved.Providers = []domain.T3Provider{}
	}
	if prev, ok := st.hosts[host.EnvironmentID]; ok {
		saved.HostID = prev.HostID
		saved.CreatedAt = prev.CreatedAt
		if len(saved.Projects) == 0 && len(prev.Projects) > 0 {
			saved.Projects = clone(prev.Projects)
		}
		if len(saved.Providers) == 0 && len(prev.Providers) > 0 {
			saved.Providers = clone(prev.Providers)
		}
	} else {
		saved.CreatedAt = created
		st.hostOrder = append(st.hostOrder, host.EnvironmentID)
	}
	saved.LastSeenAt = lastSeen
	saved.UpdatedAt = now
	st.hosts[host.EnvironmentID] = saved
	return clone(saved), nil
}

func (m *Memory) TouchT3Host(ctx context.Context, environmentID string, at time.Time) (domain.T3Host, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Host{}, false, err
	}
	if err := validID(environmentID); err != nil {
		return domain.T3Host{}, false, err
	}
	now := nowIfZero(at).UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	h, ok := st.hosts[environmentID]
	if !ok {
		return domain.T3Host{}, false, nil
	}
	if now.After(h.LastSeenAt) {
		h.LastSeenAt = now
	}
	if now.After(h.UpdatedAt) {
		h.UpdatedAt = now
	}
	st.hosts[environmentID] = h
	return clone(h), true, nil
}

func (m *Memory) T3Host(ctx context.Context, hostOrEnvID string) (domain.T3Host, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Host{}, false, err
	}
	if strings.TrimSpace(hostOrEnvID) == "" {
		return domain.T3Host{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	if h, ok := st.hosts[hostOrEnvID]; ok {
		return clone(h), true, nil
	}
	for _, envID := range st.hostOrder {
		if h := st.hosts[envID]; h.HostID == hostOrEnvID {
			return clone(h), true, nil
		}
	}
	return domain.T3Host{}, false, nil
}

func (m *Memory) ListT3Hosts(ctx context.Context) ([]domain.T3Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	out := make([]domain.T3Host, 0, len(st.hostOrder))
	for _, envID := range st.hostOrder {
		out = append(out, clone(st.hosts[envID]))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].EnvironmentID < out[j].EnvironmentID
	})
	return out, nil
}

func (m *Memory) CreateT3Command(ctx context.Context, cmd domain.T3Command) (domain.T3Command, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Command{}, false, err
	}
	if err := validID(cmd.ID); err != nil {
		return domain.T3Command{}, false, err
	}
	if err := validID(cmd.EnvironmentID); err != nil {
		return domain.T3Command{}, false, err
	}
	if !domain.ValidT3CommandAction(cmd.Action) {
		return domain.T3Command{}, false, errors.New("invalid command action")
	}
	now := nowIfZero(cmd.CreatedAt).UTC()
	updated := nowIfZero(cmd.UpdatedAt).UTC()

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	if existing, ok := st.commands[cmd.ID]; ok {
		return clone(existing), false, nil
	}
	st.commandSeq++
	saved := clone(cmd)
	saved.Seq = st.commandSeq
	if saved.Status == "" {
		saved.Status = domain.T3CommandStatusPending
	}
	saved.Payload = json.RawMessage(normalizeJSONObject(saved.Payload))
	saved.CreatedAt = now
	saved.UpdatedAt = updated
	st.commands[cmd.ID] = saved
	st.commandOrder = append(st.commandOrder, cmd.ID)
	return clone(saved), true, nil
}

func (m *Memory) T3Command(ctx context.Context, id string) (domain.T3Command, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Command{}, false, err
	}
	if strings.TrimSpace(id) == "" {
		return domain.T3Command{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	c, ok := st.commands[id]
	if !ok {
		return domain.T3Command{}, false, nil
	}
	return clone(c), true, nil
}

func (m *Memory) ClaimPendingT3Commands(ctx context.Context, environmentID string, limit int, at, redeliveryBefore time.Time) ([]domain.T3Command, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validID(environmentID); err != nil {
		return nil, err
	}
	now := nowIfZero(at).UTC()
	capLimit := limitValue(limit)

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	out := []domain.T3Command{}
	for _, id := range st.commandOrder {
		if len(out) >= capLimit {
			break
		}
		c := st.commands[id]
		if c.EnvironmentID != environmentID {
			continue
		}
		eligible := c.Status == domain.T3CommandStatusPending ||
			(!redeliveryBefore.IsZero() && c.Status == domain.T3CommandStatusDispatched && !c.UpdatedAt.After(redeliveryBefore.UTC()))
		if !eligible {
			continue
		}
		c.Status = domain.T3CommandStatusDispatched
		c.UpdatedAt = now
		st.commands[id] = c
		out = append(out, clone(c))
	}
	return out, nil
}

func (m *Memory) RequeueT3Command(ctx context.Context, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(id); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	c, ok := st.commands[id]
	if !ok || c.Status != domain.T3CommandStatusDispatched {
		return nil
	}
	c.Status = domain.T3CommandStatusPending
	c.UpdatedAt = now
	st.commands[id] = c
	return nil
}

func (m *Memory) RequeueDispatchedT3Commands(ctx context.Context, environmentID string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(environmentID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	for _, id := range st.commandOrder {
		c := st.commands[id]
		if c.EnvironmentID == environmentID && c.Status == domain.T3CommandStatusDispatched {
			c.Status = domain.T3CommandStatusPending
			c.UpdatedAt = now
			st.commands[id] = c
		}
	}
	return nil
}

func (m *Memory) CompleteT3Command(
	ctx context.Context,
	id, environmentID string,
	ok bool,
	result json.RawMessage,
	errText string,
	at time.Time,
	buildEffect func(cmd domain.T3Command) *domain.T3CommandThreadEffect,
) (domain.T3Command, *domain.T3Thread, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	if err := validID(id); err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	if err := validID(environmentID); err != nil {
		return domain.T3Command{}, nil, false, false, err
	}
	now := nowIfZero(at).UTC()

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	c, found := st.commands[id]
	if !found {
		return domain.T3Command{}, nil, false, false, nil
	}
	if c.EnvironmentID != environmentID {
		return domain.T3Command{}, nil, false, false, domain.ErrT3EnvironmentMismatch
	}
	if c.Status == domain.T3CommandStatusSucceeded || c.Status == domain.T3CommandStatusFailed {
		return clone(c), nil, true, false, nil
	}
	next := c
	if ok {
		next.Status = domain.T3CommandStatusSucceeded
	} else {
		next.Status = domain.T3CommandStatusFailed
	}
	if res := nullableJSON(result); res != nil {
		next.Result = json.RawMessage(append([]byte(nil), res...))
	} else {
		next.Result = nil
	}
	next.ErrorText = errText
	next.UpdatedAt = now

	// Validate and apply thread effect before committing command update so any
	// thread update error leaves both command and thread unchanged.
	var updatedThread *domain.T3Thread
	if ok && buildEffect != nil {
		if eff := buildEffect(clone(next)); eff != nil {
			if eff.ClearThreadID != "" {
				if err := validID(eff.ClearThreadID); err != nil {
					return domain.T3Command{}, nil, false, false, err
				}
			}
			if eff.Patch != nil {
				if err := validID(eff.Patch.ThreadID); err != nil {
					return domain.T3Command{}, nil, false, false, fmt.Errorf("thread_id: %w", err)
				}
				if err := validID(eff.Patch.EnvironmentID); err != nil {
					return domain.T3Command{}, nil, false, false, fmt.Errorf("environment_id: %w", err)
				}
			}
			if eff.ClearThreadID != "" {
				st.clearPendingLocked(eff.ClearThreadID, eff.ClearRequestID, now)
				if th, exists := st.threads[eff.ClearThreadID]; exists {
					cpTh := clone(th)
					if h, ok := st.hosts[cpTh.EnvironmentID]; ok {
						cpTh.HostID = h.HostID
					}
					updatedThread = &cpTh
				}
			}
			if eff.Patch != nil {
				th, err := st.upsertThreadLocked(*eff.Patch)
				if err != nil {
					return domain.T3Command{}, nil, false, false, err
				}
				updatedThread = &th
			}
		}
	}

	st.commands[id] = next
	return clone(next), updatedThread, true, true, nil
}

func (st *memoryT3) clearPendingLocked(threadID, requestID string, at time.Time) {
	th, ok := st.threads[threadID]
	if !ok {
		return
	}
	if requestID == "" || (th.PendingRequest != nil && th.PendingRequest.RequestID == requestID) {
		th.PendingRequest = nil
		if at.After(th.UpdatedAt) {
			th.UpdatedAt = at
		}
		st.threads[threadID] = th
	}
}

func (st *memoryT3) upsertThreadLocked(patch domain.T3ThreadPatch) (domain.T3Thread, error) {
	if err := validID(patch.ThreadID); err != nil {
		return domain.T3Thread{}, fmt.Errorf("thread_id: %w", err)
	}
	if err := validID(patch.EnvironmentID); err != nil {
		return domain.T3Thread{}, fmt.Errorf("environment_id: %w", err)
	}
	now := nowIfZero(patch.At).UTC()
	th, exists := st.threads[patch.ThreadID]
	if !exists {
		th = domain.T3Thread{
			ThreadID:      patch.ThreadID,
			EnvironmentID: patch.EnvironmentID,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}
	lastEventAt, hasLastEvent := st.threadLastEvent[patch.ThreadID]
	stale := exists && patch.RequireCurrent && hasLastEvent && now.Before(lastEventAt)

	if patch.EnvironmentID != "" {
		th.EnvironmentID = patch.EnvironmentID
	}
	if patch.ConversationID != "" {
		th.ConversationID = patch.ConversationID
	}
	if patch.TaskID != "" {
		th.TaskID = patch.TaskID
	}
	if patch.ProjectID != "" && (!stale || th.ProjectID == "") {
		th.ProjectID = patch.ProjectID
	}
	if patch.Title != "" && (!stale || th.Title == "") {
		th.Title = patch.Title
	}
	if patch.Link != "" && (!stale || th.Link == "") {
		th.Link = patch.Link
	}
	if patch.ProviderInstanceID != "" && (!stale || th.ProviderInstanceID == "") {
		th.ProviderInstanceID = patch.ProviderInstanceID
	}
	if patch.Model != "" && (!stale || th.Model == "") {
		th.Model = patch.Model
	}
	if patch.Branch != "" && (!stale || th.Branch == "") {
		th.Branch = patch.Branch
	}
	if patch.PRURL != "" && (!stale || th.PRURL == "") {
		th.PRURL = patch.PRURL
	}
	if patch.PreserveExistingStatus {
		if th.Status == "" && patch.Status != "" {
			th.Status = patch.Status
		}
	} else if !stale && patch.Status != "" {
		th.Status = patch.Status
	}
	if !stale {
		if patch.SetPendingRequest {
			if patch.PendingRequest != nil {
				pr := clone(*patch.PendingRequest)
				if pr.Questions == nil {
					pr.Questions = []json.RawMessage{}
				}
				th.PendingRequest = &pr
			} else {
				th.PendingRequest = nil
			}
		}
		if patch.SetLastOutput {
			if out := nullableJSON(patch.LastOutput); out != nil {
				th.LastOutput = json.RawMessage(append([]byte(nil), out...))
			} else {
				th.LastOutput = nil
			}
		}
		if patch.LastSummary != "" {
			th.LastSummary = patch.LastSummary
		}
		if patch.SetLastError {
			th.LastError = patch.LastError
		} else if patch.LastError != "" {
			th.LastError = patch.LastError
		}
	}
	if patch.RequireCurrent && (!hasLastEvent || now.After(lastEventAt)) {
		st.threadLastEvent[patch.ThreadID] = now
	}
	if now.After(th.UpdatedAt) {
		th.UpdatedAt = now
	}
	st.threads[patch.ThreadID] = th
	out := clone(th)
	if h, ok := st.hosts[out.EnvironmentID]; ok {
		out.HostID = h.HostID
	}
	return out, nil
}

func (m *Memory) UpsertT3Thread(ctx context.Context, patch domain.T3ThreadPatch) (domain.T3Thread, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Thread{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureT3().upsertThreadLocked(patch)
}

func (m *Memory) ClearT3ThreadPendingRequest(ctx context.Context, threadID, requestID string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(threadID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureT3().clearPendingLocked(threadID, requestID, now)
	return nil
}

func (m *Memory) T3Thread(ctx context.Context, threadID string) (domain.T3Thread, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Thread{}, false, err
	}
	if strings.TrimSpace(threadID) == "" {
		return domain.T3Thread{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	th, ok := st.threads[threadID]
	if !ok {
		return domain.T3Thread{}, false, nil
	}
	out := clone(th)
	if h, ok := st.hosts[out.EnvironmentID]; ok {
		out.HostID = h.HostID
	}
	return out, true, nil
}

func (m *Memory) ListT3Threads(ctx context.Context, filter domain.T3ThreadFilter) ([]domain.T3Thread, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	out := []domain.T3Thread{}
	for _, th := range st.threads {
		item := clone(th)
		if h, ok := st.hosts[item.EnvironmentID]; ok {
			item.HostID = h.HostID
		}
		if filter.EnvironmentID != "" && item.EnvironmentID != filter.EnvironmentID {
			continue
		}
		if filter.HostID != "" && item.HostID != filter.HostID && item.EnvironmentID != filter.HostID {
			continue
		}
		if filter.ConversationID != "" && item.ConversationID != filter.ConversationID {
			continue
		}
		if filter.ProjectID != "" && item.ProjectID != filter.ProjectID {
			continue
		}
		if filter.Status != "" && item.Status != filter.Status {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ThreadID < out[j].ThreadID
	})
	limit := limitValue(filter.Limit)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) RecordT3EventAndUpsertThread(ctx context.Context, event domain.T3Event, rawPayload json.RawMessage, patch domain.T3ThreadPatch, receivedAt time.Time) (domain.T3Thread, bool, domain.T3StoredEvent, error) {
	if err := ctx.Err(); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
	}
	if err := validID(event.EventID); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, fmt.Errorf("event_id: %w", err)
	}
	if err := validID(event.EnvironmentID); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, fmt.Errorf("environment_id: %w", err)
	}
	if err := validID(event.ThreadID); err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, fmt.Errorf("thread_id: %w", err)
	}
	if !domain.ValidT3EventKind(event.Kind) {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, errors.New("invalid event kind")
	}
	now := nowIfZero(receivedAt).UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	if existing, exists := st.events[event.EventID]; exists {
		var th domain.T3Thread
		if cur, ok := st.threads[existing.event.ThreadID]; ok {
			th = clone(cur)
			if h, ok := st.hosts[th.EnvironmentID]; ok {
				th.HostID = h.HostID
			}
		}
		var woken *time.Time
		if existing.wokenAt != nil {
			t := *existing.wokenAt
			woken = &t
		}
		return th, true, domain.T3StoredEvent{Event: clone(existing.event), WokenAt: woken}, nil
	}
	th, err := st.upsertThreadLocked(patch)
	if err != nil {
		return domain.T3Thread{}, false, domain.T3StoredEvent{}, err
	}
	storedEv := clone(event)
	storedEv.ConversationID = ""
	storedEv.TaskID = ""
	st.events[event.EventID] = memoryT3Event{
		event:      storedEv,
		raw:        json.RawMessage(normalizeJSONObject(rawPayload)),
		receivedAt: now,
	}
	st.eventOrder = append(st.eventOrder, event.EventID)
	return th, false, domain.T3StoredEvent{Event: clone(storedEv)}, nil
}

func (m *Memory) PendingWakeT3Events(ctx context.Context, threadID string) ([]domain.T3Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validID(threadID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	out := []domain.T3Event{}
	for _, id := range st.eventOrder {
		rec := st.events[id]
		if rec.event.ThreadID != threadID || rec.wokenAt != nil {
			continue
		}
		switch rec.event.Kind {
		case domain.T3EventThreadWaitingInput, domain.T3EventThreadCompleted, domain.T3EventThreadFailed:
			out = append(out, clone(rec.event))
		}
	}
	return out, nil
}

func (m *Memory) MarkT3EventWoken(ctx context.Context, eventID string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(eventID); err != nil {
		return err
	}
	now := nowIfZero(at).UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.ensureT3()
	rec, ok := st.events[eventID]
	if !ok {
		return nil
	}
	if rec.wokenAt == nil {
		rec.wokenAt = &now
		st.events[eventID] = rec
	}
	return nil
}
