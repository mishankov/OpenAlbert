package desktopapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

const (
	maxCodingCompleteBody = 8 << 20
	maxCodingSyncBody     = 8 << 20
	maxCodingToolBody     = 1 << 20
)

func decodeBodyLimit(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "too_large", "Request body is too large")
		} else {
			fail(w, 400, "invalid_request", "Invalid JSON request")
		}
		return false
	}
	return true
}

func (a *API) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

func (a *API) llmModels(w http.ResponseWriter, r *http.Request) {
	if a.Billing == nil {
		fail(w, 503, "unavailable", "Model gateway is unavailable")
		return
	}
	refresh := r.URL.Query().Get("refresh") == "true" || r.URL.Query().Get("refresh") == "1"
	defaultModel, models, err := a.Billing.Catalog(r.Context(), refresh)
	if err != nil {
		fail(w, 503, "unavailable", "Model catalog is temporarily unavailable")
		return
	}
	if models == nil {
		models = []domain.CatalogModel{}
	}
	respond(w, 200, map[string]any{
		"default_model": defaultModel,
		"models":        models,
	})
}

type llmCompleteRequest struct {
	Model          string              `json:"model"`
	Options        domain.ModelOptions `json:"options"`
	SessionID      string              `json:"session_id"`
	CorrelationID  string              `json:"correlation_id,omitempty"`
	ConversationID string              `json:"conversation_id,omitempty"`
	TurnID         string              `json:"turn_id"`
	Step           int                 `json:"step"`
	Messages       []domain.Message    `json:"messages"`
	Tools          []domain.Tool       `json:"tools"`
}

func (a *API) llmComplete(w http.ResponseWriter, r *http.Request) {
	if a.Billing == nil {
		fail(w, 503, "unavailable", "Model gateway is unavailable")
		return
	}
	var req llmCompleteRequest
	if !decodeBodyLimit(w, r, maxCodingCompleteBody, &req) {
		return
	}
	if len(req.Messages) == 0 {
		fail(w, 400, "invalid_request", "messages must not be empty")
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	ctx := r.Context()
	turnID := req.TurnID
	if turnID == "" {
		turnID = req.SessionID
	}
	convID := resolveCodingConversationID(req.ConversationID, req.SessionID)
	// Always supply a non-empty opaque session correlation ID to model providers
	// (such as x-opencode-session) even when Inspector persistence is disabled
	// (req.SessionID == "").
	providerSessionID := req.SessionID
	if providerSessionID == "" {
		providerSessionID = strings.TrimSpace(req.CorrelationID)
	}
	if providerSessionID == "" {
		providerSessionID = "ephemeral-" + agent.ID("coding-ephemeral", turnID, fmt.Sprint(req.Step), time.Now().UTC().Format(time.RFC3339Nano))
	}

	startedAt := time.Now().UTC()
	var trace *domain.Trace
	if req.SessionID != "" && a.Repo != nil {
		traceID := agent.ID(req.SessionID, turnID, "coding-model", fmt.Sprint(req.Step), startedAt.Format(time.RFC3339Nano))
		inRaw, _ := json.Marshal(map[string]any{
			"model":    req.Model,
			"options":  req.Options,
			"messages": agent.SanitizeTraceMessages(req.Messages),
			"tools":    req.Tools,
		})
		t := domain.Trace{
			ID:        traceID,
			SessionID: req.SessionID,
			TurnID:    turnID,
			Trigger:   "coding",
			Kind:      "model",
			Name:      "coding",
			ModelRole: domain.RoleCoding,
			Profile:   req.Model,
			Step:      req.Step,
			Attempt:   1,
			Status:    "running",
			StartedAt: startedAt,
			Input:     inRaw,
		}
		// Atomically validate the active coding session and record the running
		// trace under the session row lock so stale-session cleanup cannot close
		// a session whose next model turn has already been admitted.
		if err := a.Repo.SyncCodingSession(r.Context(), domain.CodingSyncBatch{
			SessionID:      req.SessionID,
			ConversationID: convID,
			OwnerID:        a.OwnerID,
			Traces:         []domain.Trace{t},
		}); err != nil {
			switch {
			case errors.Is(err, domain.ErrSessionForbidden):
				fail(w, 403, "forbidden", err.Error())
			case errors.Is(err, domain.ErrSessionClosed):
				fail(w, 409, "session_closed", err.Error())
			case errors.Is(err, domain.ErrSessionConflict):
				fail(w, 409, "conflict", err.Error())
			default:
				internal(w)
			}
			return
		}
		trace = &t
	}

	ctx = domain.WithModelRole(domain.WithModelTurn(domain.WithModelSession(ctx, providerSessionID), turnID), domain.RoleCoding)
	ctx = telemetry.WithFields(ctx, map[string]any{
		"conversation_id": convID,
		"session_id":      req.SessionID,
		"turn_id":         turnID,
		"trigger":         "coding",
	})
	ev := telemetry.Start(ctx, a.logger(), "llm.complete")
	ctx = ev.Context()
	ev.Set("step", req.Step)
	ev.Set("model_requested", req.Model)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil && errors.Is(err, http.ErrNotSupported) {
		ev.End(err)
		return
	}

	writeSSE := func(event domain.StreamEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}

	comp, err := a.Billing.CompleteModelStream(ctx, req.Model, req.Options, req.Messages, req.Tools, writeSSE)
	endedAt := time.Now().UTC()

	saveTraceFinal := func(tr domain.Trace) {
		if a.Repo == nil {
			return
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		_ = a.Repo.SaveTrace(saveCtx, tr)
	}

	if err != nil {
		ev.End(err)
		if trace != nil {
			trace.Status = "failed"
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				trace.Status = "cancelled"
			}
			trace.EndedAt = &endedAt
			trace.Error = err.Error()
			saveTraceFinal(*trace)
		}
		if r.Context().Err() == nil {
			_ = writeSSE(domain.StreamEvent{Type: "error", Error: err.Error()})
		}
		return
	}

	ev.Set("model", comp.Usage.Model)
	ev.Set("model_profile", comp.Usage.Profile)
	ev.Set("input_tokens", comp.InputTokens)
	ev.Set("output_tokens", comp.OutputTokens)
	ev.Set("tool_call_count", len(comp.Message.ToolCalls))
	ev.End(nil)

	if trace != nil {
		outRaw, _ := json.Marshal(agent.SanitizeTraceCompletion(comp))
		name := comp.Usage.Model
		if name == "" {
			name = comp.Model
		}
		trace.Name = name
		trace.Profile = comp.Usage.Profile
		trace.Status = "completed"
		trace.EndedAt = &endedAt
		trace.Output = outRaw
		saveTraceFinal(*trace)
	}
	_ = writeSSE(domain.StreamEvent{Type: "done", Completion: &comp})
}

func (a *API) agentContext(w http.ResponseWriter, r *http.Request) {
	if a.Activities == nil {
		fail(w, 503, "unavailable", "Agent tools are unavailable")
		return
	}
	ctxData, err := a.Activities.CodingContext(r.Context())
	if err != nil {
		internal(w)
		return
	}
	respond(w, 200, ctxData)
}

func (a *API) agentTool(w http.ResponseWriter, r *http.Request) {
	if a.Activities == nil {
		fail(w, 503, "unavailable", "Agent tools are unavailable")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		fail(w, 400, "invalid_request", "Tool name is required")
		return
	}
	var req agent.CodingToolRequest
	if !decodeBodyLimit(w, r, maxCodingToolBody, &req) {
		return
	}
	if req.SessionID != "" && a.Repo != nil {
		if status, code, msg, ok := a.checkCodingSession(r.Context(), req.SessionID, "", true); !ok {
			fail(w, status, code, msg)
			return
		}
	}
	req.Name = name
	res, err := a.Activities.ExecuteCodingTool(r.Context(), req)
	if err != nil {
		internal(w)
		return
	}
	respond(w, 200, res)
}

func (a *API) checkCodingSession(ctx context.Context, sessionID, expectedConvID string, requireActive bool) (int, string, string, bool) {
	if a.Repo == nil || sessionID == "" {
		return 0, "", "", true
	}
	existing, ok, err := a.Repo.Session(ctx, sessionID)
	if err != nil {
		return 500, "internal", "Failed to verify coding session", false
	}
	if !ok {
		return 0, "", "", true
	}
	if !strings.HasPrefix(existing.ConversationID, "coding:") {
		return 403, "forbidden", "Session does not belong to a coding conversation", false
	}
	if expectedConvID != "" && existing.ConversationID != expectedConvID {
		return 409, "conflict", "Session belongs to a different coding conversation", false
	}
	if requireActive && existing.Status != "active" {
		return 409, "session_closed", "Coding session is already closed", false
	}
	return 0, "", "", true
}

func resolveCodingConversationID(explicit, sessionID string) string {
	explicit = strings.TrimSpace(explicit)
	if strings.HasPrefix(explicit, "coding:") && len(explicit) > len("coding:") && !strings.ContainsAny(explicit, "/\r\n\x00 ") {
		return explicit
	}
	if sessionID == "" {
		return ""
	}
	return "coding:" + sessionID
}

type codingSessionCreateRequest struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	Title          string `json:"title"`
	CWD            string `json:"cwd"`
}

func (a *API) codingSessionCreate(w http.ResponseWriter, r *http.Request) {
	var req codingSessionCreateRequest
	if !decodeBodyLimit(w, r, 64<<10, &req) {
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" || strings.ContainsAny(req.SessionID, "/\r\n\x00 ") {
		fail(w, 400, "invalid_request", "Valid session_id is required")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(req.CWD)
	}
	if title == "" {
		title = "Coding · " + req.SessionID
	}
	convID := resolveCodingConversationID(req.ConversationID, req.SessionID)
	if err := a.Repo.SyncCodingSession(r.Context(), domain.CodingSyncBatch{
		SessionID:      req.SessionID,
		ConversationID: convID,
		Title:          title,
		OwnerID:        a.OwnerID,
	}); err != nil {
		switch {
		case errors.Is(err, domain.ErrSessionForbidden):
			fail(w, 403, "forbidden", err.Error())
		case errors.Is(err, domain.ErrSessionClosed):
			fail(w, 409, "session_closed", err.Error())
		case errors.Is(err, domain.ErrSessionConflict):
			fail(w, 409, "conflict", err.Error())
		default:
			internal(w)
		}
		return
	}
	sess, _, err := a.Repo.Session(r.Context(), req.SessionID)
	if err != nil {
		internal(w)
		return
	}
	conv, _, err := a.Repo.Conversation(r.Context(), convID)
	if err != nil {
		internal(w)
		return
	}
	respond(w, 200, map[string]any{"session": sess, "conversation": conv})
}

type codingSyncMessage struct {
	Key     string         `json:"key"`
	Message domain.Message `json:"message"`
}

type codingSyncRequest struct {
	ConversationID string              `json:"conversation_id,omitempty"`
	Title          string              `json:"title,omitempty"`
	Messages       []codingSyncMessage `json:"messages"`
	Traces         []domain.Trace      `json:"traces"`
	TurnSummary    *agent.Turn         `json:"turn_summary,omitempty"`
}

func (a *API) codingSessionSync(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" || strings.ContainsAny(sessionID, "/\r\n\x00 ") {
		fail(w, 400, "invalid_request", "Valid session id is required")
		return
	}
	var req codingSyncRequest
	if !decodeBodyLimit(w, r, maxCodingSyncBody, &req) {
		return
	}
	convID := resolveCodingConversationID(req.ConversationID, sessionID)
	title := strings.TrimSpace(req.Title)

	batch := domain.CodingSyncBatch{
		SessionID:      sessionID,
		ConversationID: convID,
		Title:          title,
		OwnerID:        a.OwnerID,
		Messages:       make([]domain.CodingSyncMessage, 0, len(req.Messages)),
		Traces:         make([]domain.Trace, 0, len(req.Traces)),
	}
	for _, item := range req.Messages {
		if strings.TrimSpace(item.Key) == "" {
			fail(w, 400, "invalid_request", "Each synced message requires a key")
			return
		}
		batch.Messages = append(batch.Messages, domain.CodingSyncMessage{
			Key:     item.Key,
			Message: item.Message,
		})
	}
	for _, tr := range req.Traces {
		if strings.TrimSpace(tr.ID) == "" {
			fail(w, 400, "invalid_request", "Each synced trace requires an id")
			return
		}
		// Scope client-supplied trace IDs to this coding session so they can never
		// collide with or overwrite traces belonging to another session.
		tr.ID = agent.ID("coding-trace", sessionID, tr.ID)
		tr.SessionID = sessionID
		if tr.Trigger == "" {
			tr.Trigger = "coding"
		}
		if tr.StartedAt.IsZero() {
			tr.StartedAt = time.Now().UTC()
		}
		batch.Traces = append(batch.Traces, tr)
	}
	if req.TurnSummary != nil && strings.TrimSpace(req.TurnSummary.Input.Key) != "" {
		turn := *req.TurnSummary
		rawKey := strings.TrimSpace(turn.Input.Key)
		turn.Input.Key = "coding/" + sessionID + "/" + rawKey
		turn.Input.SessionID = sessionID
		turn.Input.ConversationID = convID
		turn.Done = true
		turn.Messages = nil
		raw, _ := json.Marshal(turn)
		batch.CheckpointKey = "turn-done/coding/" + sessionID + "/" + rawKey
		batch.CheckpointPayload = raw
	}
	if err := a.Repo.SyncCodingSession(r.Context(), batch); err != nil {
		switch {
		case errors.Is(err, domain.ErrSessionForbidden):
			fail(w, 403, "forbidden", err.Error())
		case errors.Is(err, domain.ErrSessionClosed):
			fail(w, 409, "session_closed", err.Error())
		case errors.Is(err, domain.ErrSessionConflict):
			fail(w, 409, "conflict", err.Error())
		default:
			internal(w)
		}
		return
	}
	respond(w, 200, map[string]any{
		"synced_messages": len(req.Messages),
		"synced_traces":   len(req.Traces),
	})
}

func (a *API) codingSessionClose(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" || strings.ContainsAny(sessionID, "/\r\n\x00 ") {
		fail(w, 400, "invalid_request", "Valid session id is required")
		return
	}
	existing, ok, err := a.Repo.Session(r.Context(), sessionID)
	if err != nil {
		internal(w)
		return
	}
	if !ok {
		fail(w, 404, "not_found", "Coding session not found")
		return
	}
	if !strings.HasPrefix(existing.ConversationID, "coding:") {
		fail(w, 403, "forbidden", "Session does not belong to a coding conversation")
		return
	}
	if err := a.Repo.EndSession(r.Context(), sessionID, ""); err != nil {
		fail(w, 404, "not_found", "Coding session not found")
		return
	}
	respond(w, 200, map[string]any{"closed": true, "session_id": sessionID})
}
