package desktopapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/t3"
)

const maxT3BodyBytes = 4 << 20

func decodeT3Body(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxT3BodyBytes)
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "too_large", "Request body is too large")
		} else {
			fail(w, 400, "invalid_request", "Invalid JSON request")
		}
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request", "Expected one JSON object")
		return false
	}
	return true
}

func (a *API) requireT3(w http.ResponseWriter) bool {
	if a.T3 == nil {
		fail(w, 503, "unavailable", "T3 control plane is unavailable")
		return false
	}
	return true
}

func (a *API) t3Error(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, t3.ErrInvalidInput):
		fail(w, 400, "invalid_request", err.Error())
	case errors.Is(err, t3.ErrEnvironmentMismatch):
		fail(w, 403, "forbidden", "Command belongs to another environment")
	case errors.Is(err, t3.ErrHostNotFound), errors.Is(err, t3.ErrCommandNotFound), errors.Is(err, t3.ErrThreadNotFound):
		fail(w, 404, "not_found", err.Error())
	default:
		internal(w)
	}
}

func (a *API) t3HostRegister(w http.ResponseWriter, r *http.Request) {
	if !a.requireT3(w) {
		return
	}
	var req t3.RegisterHostRequest
	if !decodeT3Body(w, r, &req) {
		return
	}
	host, err := a.T3.RegisterHost(r.Context(), req)
	if err != nil {
		a.t3Error(w, err)
		return
	}
	respond(w, 200, map[string]any{
		"host_id":        host.HostID,
		"environment_id": host.EnvironmentID,
		"registered_at":  host.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

func (a *API) t3Heartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.requireT3(w) {
		return
	}
	var req struct {
		EnvironmentID   string   `json:"environment_id"`
		ActiveThreadIDs []string `json:"active_thread_ids"`
	}
	if !decodeT3Body(w, r, &req) {
		return
	}
	serverTime, err := a.T3.Heartbeat(r.Context(), req.EnvironmentID, req.ActiveThreadIDs)
	if err != nil {
		a.t3Error(w, err)
		return
	}
	respond(w, 200, map[string]any{
		"ok":          true,
		"server_time": serverTime.UTC().Format(time.RFC3339),
	})
}

type t3StreamCommand struct {
	CommandID string          `json:"command_id"`
	Action    string          `json:"action"`
	Payload   json.RawMessage `json:"payload"`
}

func toStreamCommand(cmd domain.T3Command) t3StreamCommand {
	payload := cmd.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return t3StreamCommand{
		CommandID: cmd.ID,
		Action:    cmd.Action,
		Payload:   payload,
	}
}

func wantsOneShotJSON(r *http.Request) bool {
	if once := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("once"))); once == "true" || once == "1" {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/event-stream")
}

func (a *API) t3CommandStream(w http.ResponseWriter, r *http.Request) {
	if !a.requireT3(w) {
		return
	}
	envID := strings.TrimSpace(r.URL.Query().Get("environment_id"))
	if envID == "" {
		fail(w, 400, "invalid_request", "environment_id query parameter is required")
		return
	}
	if wantsOneShotJSON(r) {
		cmds, err := a.T3.ClaimPendingCommands(r.Context(), envID, 100)
		if err != nil {
			a.t3Error(w, err)
			return
		}
		out := make([]t3StreamCommand, 0, len(cmds))
		for _, c := range cmds {
			out = append(out, toStreamCommand(c))
		}
		respond(w, 200, map[string]any{"commands": out})
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	wakeup, unsubscribe, err := a.T3.SubscribeCommands(r.Context(), envID)
	if err != nil {
		a.t3Error(w, err)
		return
	}
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}

	emitPending := func() error {
		for {
			if r.Context().Err() != nil {
				return r.Context().Err()
			}
			// Claim one command at a time so a mid-batch disconnect never strands
			// unwritten commands in dispatched state.
			cmds, err := a.T3.ClaimPendingCommands(r.Context(), envID, 1)
			if err != nil {
				return err
			}
			if len(cmds) == 0 {
				return nil
			}
			cmd := cmds[0]
			data, err := json.Marshal(toStreamCommand(cmd))
			if err != nil {
				_ = a.T3.RequeueCommand(context.WithoutCancel(r.Context()), cmd.ID)
				return err
			}
			if _, err := fmt.Fprintf(w, "event: command\nid: %s\ndata: %s\n\n", cmd.ID, data); err != nil {
				_ = a.T3.RequeueCommand(context.WithoutCancel(r.Context()), cmd.ID)
				return err
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				_ = a.T3.RequeueCommand(context.WithoutCancel(r.Context()), cmd.ID)
				return err
			}
		}
	}

	if err := emitPending(); err != nil {
		return
	}

	keepalive := time.NewTicker(a.T3.KeepaliveInterval())
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-wakeup:
			if err := emitPending(); err != nil {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return
			}
			if err := emitPending(); err != nil {
				return
			}
		}
	}
}

func (a *API) t3CommandResult(w http.ResponseWriter, r *http.Request) {
	if !a.requireT3(w) {
		return
	}
	commandID := strings.TrimSpace(r.PathValue("command_id"))
	if commandID == "" {
		fail(w, 400, "invalid_request", "command_id is required")
		return
	}
	var req t3.CommandResultRequest
	if !decodeT3Body(w, r, &req) {
		return
	}
	if err := a.T3.RecordCommandResult(r.Context(), commandID, req); err != nil {
		a.t3Error(w, err)
		return
	}
	respond(w, 200, map[string]any{"ok": true})
}

func (a *API) t3Event(w http.ResponseWriter, r *http.Request) {
	if !a.requireT3(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxT3BodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "too_large", "Request body is too large")
		} else {
			fail(w, 400, "invalid_request", "Invalid JSON request")
		}
		return
	}
	var wire struct {
		EventID            string                   `json:"event_id"`
		EnvironmentID      string                   `json:"environment_id"`
		ThreadID           string                   `json:"thread_id"`
		Kind               string                   `json:"kind"`
		Status             string                   `json:"status"`
		Title              string                   `json:"title"`
		Link               string                   `json:"link"`
		ProjectID          string                   `json:"project_id"`
		ProviderInstanceID string                   `json:"provider_instance_id"`
		Model              string                   `json:"model"`
		Branch             string                   `json:"branch"`
		PRURL              string                   `json:"pr_url"`
		PendingRequest     *domain.T3PendingRequest `json:"pending_request"`
		Output             json.RawMessage          `json:"output"`
		Summary            string                   `json:"summary"`
		Error              string                   `json:"error"`
		OccurredAt         string                   `json:"occurred_at"`
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	if err := d.Decode(&wire); err != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request", "Invalid JSON request")
		return
	}
	var occurredAt time.Time
	if s := strings.TrimSpace(wire.OccurredAt); s != "" {
		occurredAt, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			fail(w, 400, "invalid_request", "occurred_at must be RFC3339")
			return
		}
	}
	ev := domain.T3Event{
		EventID:            wire.EventID,
		EnvironmentID:      wire.EnvironmentID,
		ThreadID:           wire.ThreadID,
		Kind:               wire.Kind,
		Status:             wire.Status,
		Title:              wire.Title,
		Link:               wire.Link,
		ProjectID:          wire.ProjectID,
		ProviderInstanceID: wire.ProviderInstanceID,
		Model:              wire.Model,
		Branch:             wire.Branch,
		PRURL:              wire.PRURL,
		PendingRequest:     wire.PendingRequest,
		Output:             wire.Output,
		Summary:            wire.Summary,
		Error:              wire.Error,
		OccurredAt:         occurredAt,
	}
	deduplicated, err := a.T3.IngestEvent(r.Context(), ev, json.RawMessage(raw))
	if err != nil {
		a.t3Error(w, err)
		return
	}
	respond(w, 200, map[string]any{
		"ok":           true,
		"deduplicated": deduplicated,
	})
}
