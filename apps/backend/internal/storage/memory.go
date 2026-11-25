package storage

import (
	"context"
	"encoding/json"
	"errors"
	"openalbert/apps/backend/internal/domain"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory implements the same repository contract for deterministic workflow tests.
// It owns all values; callers cannot mutate stored slices after a method returns.
type Memory struct {
	transcriptions     map[string]transcriptionLease
	mu                 sync.Mutex
	offset             int64
	conversations      map[string]domain.Conversation
	incoming           map[string]domain.Incoming
	incomingOrder      []string
	publicOrder        []publicKey
	handled            map[string]bool
	sessions           map[string]domain.Session
	sessionOrder       []string
	analyzed           map[string]bool
	messages           map[string][]domain.Message
	messageKeys        map[string]bool
	history            []domain.HistoryMessage
	tasks              map[string]domain.Task
	taskOrder          []string
	outgoing           map[string]domain.Outgoing
	outgoingOrder      []string
	artifacts          map[string]domain.Artifact
	checkpoints        map[string]json.RawMessage
	traces             map[string]domain.Trace
	progress           map[string]domain.Progress
	consolidations     map[string]domain.MemoryConsolidationReport
	consolidationOrder []string
	t3                 *memoryT3
}

var _ domain.Repository = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{
		transcriptions: map[string]transcriptionLease{},
		conversations:  map[string]domain.Conversation{}, incoming: map[string]domain.Incoming{}, handled: map[string]bool{}, sessions: map[string]domain.Session{}, analyzed: map[string]bool{}, messages: map[string][]domain.Message{}, messageKeys: map[string]bool{}, tasks: map[string]domain.Task{}, outgoing: map[string]domain.Outgoing{}, artifacts: map[string]domain.Artifact{}, checkpoints: map[string]json.RawMessage{}, traces: map[string]domain.Trace{}, progress: map[string]domain.Progress{}, consolidations: map[string]domain.MemoryConsolidationReport{},
	}
}
func (m *Memory) Close()                         {}
func (m *Memory) Ping(ctx context.Context) error { return ctx.Err() }
func clone[T any](v T) T                         { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }
func cloneIncoming(v domain.Incoming) domain.Incoming {
	o := clone(v)
	for i := range o.Attachments {
		o.Attachments[i].Data = append([]byte(nil), v.Attachments[i].Data...)
	}
	return o
}
func (m *Memory) AcceptIncoming(ctx context.Context, in domain.Incoming, offset int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validID(in.ID); err != nil {
		return false, err
	}
	if err := validID(in.Conversation.ID); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if offset > m.offset {
		m.offset = offset
	}
	canonical := in.Conversation
	canonical.DeliveryChannel = ""
	if existing, exists := m.conversations[in.Conversation.ID]; exists && existing.ThreadID > 0 && existing.Title != "" {
		canonical.Title = existing.Title
	}
	m.conversations[in.Conversation.ID] = canonical
	if _, ok := m.incoming[in.ID]; ok {
		return false, nil
	}
	in.CreatedAt = nowIfZero(in.CreatedAt)
	m.incoming[in.ID] = cloneIncoming(in)
	m.incomingOrder = append(m.incomingOrder, in.ID)
	m.publicOrder = append(m.publicOrder, publicKey{ID: in.ID, Incoming: true})
	return true, nil
}
func (m *Memory) PollOffset(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.offset, ctx.Err()
}
func (m *Memory) SetPollOffset(ctx context.Context, offset int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if offset > m.offset {
		m.offset = offset
	}
	return nil
}
func (m *Memory) PendingIncoming(ctx context.Context, n int) ([]domain.Incoming, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Incoming{}
	for _, id := range m.incomingOrder {
		if !m.handled[id] {
			out = append(out, cloneIncoming(m.incoming[id]))
			if len(out) >= limitValue(n) {
				break
			}
		}
	}
	return out, ctx.Err()
}
func (m *Memory) Incoming(ctx context.Context, id string) (domain.Incoming, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.incoming[id]
	return cloneIncoming(v), ok, ctx.Err()
}
func (m *Memory) MarkIncomingHandled(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handled[id] = true
	return ctx.Err()
}
func (m *Memory) Conversation(ctx context.Context, id string) (domain.Conversation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.conversations[id]
	return v, ok, ctx.Err()
}
func (m *Memory) SaveConversation(ctx context.Context, conv domain.Conversation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(conv.ID); err != nil {
		return err
	}
	conv.DeliveryChannel = ""
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conversations[conv.ID] = conv
	return nil
}
func (m *Memory) ListPrivateConversations(ctx context.Context) ([]domain.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Conversation{}
	for _, v := range m.conversations {
		if v.Private && !strings.HasPrefix(v.ID, "coding:") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, ctx.Err()
}
func (m *Memory) ListConversations(ctx context.Context) ([]domain.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Conversation{}
	for _, v := range m.conversations {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, ctx.Err()
}
func (m *Memory) ConversationHistory(ctx context.Context, scope, query string, before int64, n int) ([]domain.HistoryMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.HistoryMessage{}
	for i := len(m.history) - 1; i >= 0 && len(out) < historyLimit(n); i-- {
		v := m.history[i]
		if v.ConversationID == scope && (before == 0 || v.Seq < before) &&
			(v.Role == "user" || v.Role == "assistant") && (v.Content != "" || len(v.ToolCalls) > 0) && strings.Contains(strings.ToLower(v.Content), strings.ToLower(query)) {
			out = append(out, clone(v))
		}
	}
	return out, ctx.Err()
}
func (m *Memory) Session(ctx context.Context, id string) (domain.Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok, ctx.Err()
}
func (m *Memory) StartSession(ctx context.Context, s domain.Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(s.ID); err != nil {
		return err
	}
	if err := validID(s.ConversationID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; !ok {
		s.Status = "active"
		s.CreatedAt = nowIfZero(s.CreatedAt)
		m.sessions[s.ID] = s
		m.sessionOrder = append(m.sessionOrder, s.ID)
	}
	return nil
}
func (m *Memory) SyncCodingSession(ctx context.Context, batch domain.CodingSyncBatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(batch.SessionID); err != nil {
		return err
	}
	if err := validID(batch.ConversationID); err != nil {
		return err
	}
	if !strings.HasPrefix(batch.ConversationID, "coding:") {
		return domain.ErrSessionForbidden
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Pre-validate session state, message keys, trace ownership, and checkpoint key
	// BEFORE publishing any session/conversation/title mutations so failed batches are atomic.
	existingSess, hasSess := m.sessions[batch.SessionID]
	if hasSess {
		if !strings.HasPrefix(existingSess.ConversationID, "coding:") {
			return domain.ErrSessionForbidden
		}
		if existingSess.ConversationID != batch.ConversationID {
			return domain.ErrSessionConflict
		}
		if existingSess.Status != "active" {
			return domain.ErrSessionClosed
		}
	}
	for _, item := range batch.Messages {
		if err := validID(item.Key); err != nil {
			return err
		}
	}
	for _, tr := range batch.Traces {
		if existing, ok := m.traces[tr.ID]; ok && existing.SessionID != tr.SessionID {
			return domain.ErrSessionConflict
		}
	}
	if batch.CheckpointKey != "" && len(batch.CheckpointPayload) > 0 {
		if err := validID(batch.CheckpointKey); err != nil {
			return err
		}
	}

	if !hasSess {
		m.sessions[batch.SessionID] = domain.Session{
			ID: batch.SessionID, ConversationID: batch.ConversationID, Status: "active", Trigger: "coding", CreatedAt: time.Now().UTC(),
		}
		m.sessionOrder = append(m.sessionOrder, batch.SessionID)
	}
	if conv, ok := m.conversations[batch.ConversationID]; !ok {
		title := strings.TrimSpace(batch.Title)
		if title == "" {
			title = "Coding · " + batch.SessionID
		}
		m.conversations[batch.ConversationID] = domain.Conversation{
			ID: batch.ConversationID, Title: title, OwnerID: batch.OwnerID, Private: true,
		}
	} else if title := strings.TrimSpace(batch.Title); title != "" && conv.Title != title {
		conv.Title = title
		m.conversations[batch.ConversationID] = conv
	}
	se := m.sessions[batch.SessionID]
	for _, item := range batch.Messages {
		k := taskKey(batch.SessionID, item.Key)
		if !m.messageKeys[k] {
			m.messageKeys[k] = true
			m.messages[batch.SessionID] = append(m.messages[batch.SessionID], clone(item.Message))
			m.history = append(m.history, domain.HistoryMessage{
				Seq:              int64(len(m.history) + 1),
				ConversationID:   se.ConversationID,
				SessionID:        batch.SessionID,
				SessionStartedAt: se.CreatedAt,
				Role:             item.Message.Role,
				Content:          historyContent(item.Message),
				AuthorID:         item.Message.AuthorID,
				ToolCalls:        historyToolCalls(item.Message),
			})
		}
	}
	for _, tr := range batch.Traces {
		m.traces[tr.ID] = clone(tr)
	}
	if batch.CheckpointKey != "" && len(batch.CheckpointPayload) > 0 {
		m.checkpoints[batch.CheckpointKey] = append(json.RawMessage(nil), batch.CheckpointPayload...)
	}
	return nil
}
func (m *Memory) EndSession(ctx context.Context, id, summary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if s.Status == "active" || (s.Summary == "" && summary != "") {
		s.Status = "closed"
		s.Summary = summary
		m.sessions[id] = s
	}
	return nil
}
func (m *Memory) PendingAnalysis(ctx context.Context, n int) ([]domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Session{}
	for _, id := range m.sessionOrder {
		v := m.sessions[id]
		if v.Status == "closed" && !m.analyzed[id] && v.AnalysisError == "" {
			out = append(out, v)
			if len(out) >= limitValue(n) {
				break
			}
		}
	}
	return out, ctx.Err()
}
func (m *Memory) MarkAnalyzed(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok && s.Status == "closed" && s.AnalysisError == "" {
		m.analyzed[id] = true
	}
	return nil
}
func (m *Memory) MarkAnalysisFailed(ctx context.Context, id, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok && s.Status == "closed" && !m.analyzed[id] && s.AnalysisError == "" {
		s.AnalysisError = code
		m.sessions[id] = s
	}
	return nil
}
func (m *Memory) AppendMessage(ctx context.Context, session, key string, msg domain.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[session]; !ok {
		return ErrNotFound
	}
	k := taskKey(session, key)
	if !m.messageKeys[k] {
		m.messageKeys[k] = true
		m.messages[session] = append(m.messages[session], clone(msg))
		se := m.sessions[session]
		m.history = append(m.history, domain.HistoryMessage{Seq: int64(len(m.history) + 1), ConversationID: se.ConversationID, SessionID: session, SessionStartedAt: se.CreatedAt, Role: msg.Role, Content: historyContent(msg), AuthorID: msg.AuthorID, ToolCalls: historyToolCalls(msg)})
	}
	return nil
}
func (m *Memory) Messages(ctx context.Context, session string) ([]domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := clone(m.messages[session])
	if out == nil {
		out = []domain.Message{}
	}
	return out, ctx.Err()
}
func (m *Memory) CreateTask(ctx context.Context, t domain.Task) (domain.Task, error) {
	if err := ctx.Err(); err != nil {
		return t, err
	}
	if err := validateTask(t); err != nil {
		return t, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey(t.ConversationID, t.ID)
	if old, ok := m.tasks[k]; ok {
		return old, nil
	}
	if t.Status == "" {
		t.Status = "queued"
	}
	t.CreatedAt = nowIfZero(t.CreatedAt)
	t.UpdatedAt = t.CreatedAt
	t.Version = 1
	m.tasks[k] = t
	m.taskOrder = append(m.taskOrder, k)
	return t, nil
}
func (m *Memory) UpdateTask(ctx context.Context, t domain.Task) (domain.Task, error) {
	if err := ctx.Err(); err != nil {
		return t, err
	}
	if err := validateTask(t); err != nil {
		return t, err
	}
	if t.Status == "" {
		return t, errors.New("task status is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey(t.ConversationID, t.ID)
	old, ok := m.tasks[k]
	if !ok {
		return t, ErrNotFound
	}
	if t.Version != old.Version {
		return t, ErrConflict
	}
	t.Version++
	t.CreatedAt = old.CreatedAt
	t.UpdatedAt = time.Now().UTC()
	m.tasks[k] = t
	return t, nil
}
func (m *Memory) FindTasks(ctx context.Context, scope, q string) ([]domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Task{}
	query := strings.ToLower(q)
	for _, k := range m.taskOrder {
		t := m.tasks[k]
		if t.ConversationID == scope && (strings.Contains(strings.ToLower(t.Title), query) || strings.Contains(strings.ToLower(t.Goal), query) || t.ID == q) {
			out = append(out, t)
		}
	}
	return out, ctx.Err()
}
func (m *Memory) Task(ctx context.Context, scope, id string) (domain.Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.tasks[taskKey(scope, id)]
	return v, ok, ctx.Err()
}
func (m *Memory) RunnableTasks(ctx context.Context) ([]domain.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Task{}
	for _, k := range m.taskOrder {
		if t := m.tasks[k]; runnable(t.Status) {
			out = append(out, t)
		}
	}
	return out, ctx.Err()
}
func (m *Memory) EnqueueOutgoing(ctx context.Context, o domain.Outgoing) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(o.ID); err != nil {
		return err
	}
	if err := validID(o.Conversation.ID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.outgoing[o.ID]; !ok {
		o.EnqueuedAt = nowIfZero(o.EnqueuedAt)
		o.State = "pending"
		o.Attempts = 0
		m.outgoing[o.ID] = clone(o)
		m.outgoingOrder = append(m.outgoingOrder, o.ID)
		m.publicOrder = append(m.publicOrder, publicKey{ID: o.ID})
		if o.RecordInHistory {
			session, message := outgoingHistory(o)
			m.sessions[session.ID] = session
			m.sessionOrder = append(m.sessionOrder, session.ID)
			m.analyzed[session.ID] = true
			m.messages[session.ID] = []domain.Message{message}
			m.messageKeys[taskKey(session.ID, o.ID)] = true
			m.history = append(m.history, domain.HistoryMessage{Seq: int64(len(m.history) + 1), ConversationID: session.ConversationID, SessionID: session.ID, SessionStartedAt: session.CreatedAt, Role: message.Role, Content: historyContent(message), ToolCalls: historyToolCalls(message)})
		}
	}
	return nil
}
func (m *Memory) PendingOutgoing(ctx context.Context, n int) ([]domain.Outgoing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Outgoing{}
	for _, id := range m.outgoingOrder {
		if o := m.outgoing[id]; o.State == "pending" {
			out = append(out, clone(o))
			if len(out) >= limitValue(n) {
				break
			}
		}
	}
	return out, ctx.Err()
}

func (m *Memory) PendingDeliverableOutgoing(ctx context.Context, n int, readyVoiceID string, startVoice bool) ([]domain.Outgoing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Outgoing
	waitingForVoice := make(map[string]bool)
	for _, id := range m.outgoingOrder {
		o := m.outgoing[id]
		if o.State != "pending" || waitingForVoice[o.Conversation.ID] {
			continue
		}
		if o.VoiceReplyTo > 0 && o.VoiceFallback == "" {
			waitingForVoice[o.Conversation.ID] = true
			if id != readyVoiceID && !startVoice {
				continue
			}
			startVoice = false
		}
		out = append(out, clone(o))
		if len(out) >= limitValue(n) {
			break
		}
	}
	return out, ctx.Err()
}
func (m *Memory) SetOutgoingState(ctx context.Context, id, state string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state != "sending" && state != "sent" && state != "unknown" && state != "pending" && state != "failed" {
		return ErrInvalidTransition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outgoing[id]
	if !ok {
		return ErrNotFound
	}
	if o.State == state {
		return nil
	}
	if !outgoingTransition(o.State, state) {
		return ErrInvalidTransition
	}
	o.State = state
	if state == "sending" {
		o.Attempts++
	}
	m.outgoing[id] = o
	return nil
}

func (m *Memory) FallbackVoice(ctx context.Context, id string, reason domain.VoiceFallback) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.outgoing[id]
	if o.VoiceReplyTo == 0 || o.VoiceFallback != "" || (o.State != "pending" && o.State != "sending") {
		return ErrInvalidTransition
	}
	o.State, o.VoiceFallback = "pending", reason
	m.outgoing[id] = o
	return nil
}
func (m *Memory) SaveArtifact(ctx context.Context, a domain.Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(a.ID); err != nil {
		return err
	}
	if err := validID(a.ConversationID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := taskKey(a.ConversationID, a.ID)
	if _, ok := m.artifacts[k]; !ok {
		a.CreatedAt = nowIfZero(a.CreatedAt)
		a.Data = append([]byte(nil), a.Data...)
		m.artifacts[k] = a
	}
	return nil
}
func (m *Memory) Artifact(ctx context.Context, scope, id string) (domain.Artifact, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.artifacts[taskKey(scope, id)]
	a.Data = append([]byte(nil), a.Data...)
	return a, ok, ctx.Err()
}
func (m *Memory) LoadCheckpoint(ctx context.Context, key string) (json.RawMessage, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.checkpoints[key]
	return append(json.RawMessage(nil), v...), ok, ctx.Err()
}
func (m *Memory) SaveCheckpoint(ctx context.Context, key string, value json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validID(key); err != nil {
		return err
	}
	if !json.Valid(value) {
		return errors.New("invalid checkpoint JSON")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkpoints[key] = append(json.RawMessage(nil), value...)
	return nil
}

func (m *Memory) ClaimCheckpoint(ctx context.Context, key string, value json.RawMessage) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validID(key); err != nil {
		return false, err
	}
	if !json.Valid(value) {
		return false, errors.New("invalid checkpoint JSON")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.checkpoints[key]; ok {
		return false, nil
	}
	m.checkpoints[key] = append(json.RawMessage(nil), value...)
	return true, nil
}

func (m *Memory) ActiveSessions(ctx context.Context) ([]domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Session{}
	for _, session := range m.sessions {
		if session.Status == "active" {
			out = append(out, session)
		}
	}
	return out, ctx.Err()
}

func (m *Memory) SaveTrace(ctx context.Context, tr domain.Trace) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.traces[tr.ID]; ok && existing.SessionID != tr.SessionID {
		return domain.ErrSessionConflict
	}
	m.traces[tr.ID] = clone(tr)
	return ctx.Err()
}

// MaintainTask commits the restricted update and its durable audit under one lock.
func (m *Memory) MaintainTask(ctx context.Context, actorID, runID, key string, t domain.Task) (domain.Task, error) {
	if err := ctx.Err(); err != nil {
		return t, err
	}
	if err := validID(key); err != nil {
		return t, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	actor, ok := m.tasks[taskKey(t.ConversationID, actorID)]
	if !ok {
		return t, ErrNotFound
	}
	before, ok := m.tasks[taskKey(t.ConversationID, t.ID)]
	if !ok {
		return t, ErrNotFound
	}
	if raw, ok := m.checkpoints["task-maintenance/"+key]; ok {
		var audit TaskMaintenanceAudit
		if err := json.Unmarshal(raw, &audit); err != nil {
			return t, err
		}
		if audit.ActorTaskID != actorID || audit.TargetTaskID != t.ID || audit.RunID != runID || audit.ConversationID != t.ConversationID {
			return t, ErrConflict
		}
		return before, nil
	}
	if err := validateMaintenance(actor, before, t, time.Now()); err != nil {
		return t, err
	}
	t.Version++
	t.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(maintenanceAudit(actorID, runID, key, before, t))
	if err != nil {
		return t, err
	}
	m.tasks[taskKey(t.ConversationID, t.ID)] = t
	m.checkpoints["task-maintenance/"+key] = raw
	return t, nil
}
