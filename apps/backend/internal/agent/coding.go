package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/telemetry"
)

// CodingSkill summarizes an active OpenAlbert server skill for coding clients.
type CodingSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CodingContext supplies owner personality, profile, server skills and remote
// server tools available to an authenticated coding agent session.
type CodingContext struct {
	OwnerID     int64         `json:"owner_id"`
	Soul        string        `json:"soul"`
	UserProfile string        `json:"user_profile"`
	Skills      []CodingSkill `json:"skills"`
	Tools       []domain.Tool `json:"tools"`
}

// CodingToolRequest invokes one authorized server-side OpenAlbert tool on
// behalf of an authenticated coding session.
type CodingToolRequest struct {
	SessionID        string          `json:"session_id"`
	CorrelationID    string          `json:"correlation_id,omitempty"`
	TurnID           string          `json:"turn_id"`
	Step             int             `json:"step"`
	CallID           string          `json:"call_id"`
	ParentToolCallID string          `json:"parent_tool_call_id,omitempty"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
}

// CodingToolResult returns the JSON tool payload and optional image views.
type CodingToolResult struct {
	Output json.RawMessage `json:"output"`
	Failed bool            `json:"failed,omitempty"`
	Images []string        `json:"images,omitempty"`
}

func (a *Activities) ownerCodingConversation() domain.Conversation {
	return domain.Conversation{
		ID:              fmt.Sprintf("personal:%d", a.OwnerUserID),
		ChatID:          a.OwnerUserID,
		OwnerID:         a.OwnerUserID,
		Private:         true,
		DeliveryChannel: "desktop",
	}
}

func isCodingExcludedTool(name string) bool {
	switch name {
	case "send_message", "send_rich_message", "send_voice", "send_widget", "edit_widget_data", "report_progress", "send_group_message":
		return true
	default:
		return false
	}
}

// codingTools returns all authorized owner server tools exposed to coding agents.
// Kept unexported so Temporal struct-activity registration on *Activities only
// sees valid (T, error) activity methods.
func (a *Activities) codingTools() []domain.Tool {
	turn := Turn{Conversation: a.ownerCodingConversation()}
	all := a.toolsForTurn(turn)
	out := make([]domain.Tool, 0, len(all))
	for _, tool := range all {
		if isCodingExcludedTool(tool.Name) {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// CodingContext gathers SOUL.md, USER.md, server skills and remote tools.
func (a *Activities) CodingContext(ctx context.Context) (CodingContext, error) {
	conv := a.ownerCodingConversation()
	var soul, profile string
	var skills []CodingSkill
	if a.Knowledge != nil {
		var err error
		soul, err = a.Knowledge.Soul(ctx)
		if err != nil {
			return CodingContext{}, err
		}
		profile, err = a.Knowledge.Profile(ctx, conv.ID)
		if err != nil {
			return CodingContext{}, err
		}
		rawSkills, err := a.Knowledge.Skills(ctx)
		if err != nil {
			return CodingContext{}, err
		}
		skills = make([]CodingSkill, 0, len(rawSkills))
		for _, s := range rawSkills {
			skills = append(skills, CodingSkill{Name: s.Name, Description: s.Description})
		}
	}
	return CodingContext{
		OwnerID:     a.OwnerUserID,
		Soul:        clip(soul, 12000),
		UserProfile: clip(profile, 8000),
		Skills:      skills,
		Tools:       a.codingTools(),
	}, nil
}

// ExecuteCodingTool runs one authorized owner server tool for a coding session,
// enforcing the same access rules and writing a trace for Inspector.
func (a *Activities) ExecuteCodingTool(ctx context.Context, req CodingToolRequest) (CodingToolResult, error) {
	allowed := false
	for _, tool := range a.codingTools() {
		if tool.Name == req.Name {
			allowed = true
			break
		}
	}
	if !allowed {
		raw, _ := json.Marshal(map[string]string{"error": "tool is unavailable for coding sessions", "error_code": "tool_unavailable"})
		return CodingToolResult{Output: raw, Failed: true}, nil
	}
	args := req.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		raw, _ := json.Marshal(map[string]string{"error": "invalid tool JSON", "error_code": "invalid_request"})
		return CodingToolResult{Output: raw, Failed: true}, nil
	}
	conv := a.ownerCodingConversation()
	callID := req.CallID
	if callID == "" {
		callID = ID(req.SessionID, req.TurnID, fmt.Sprint(req.Step), req.Name, string(args))
	}
	runKey := req.TurnID
	if runKey == "" {
		runKey = req.SessionID
	}
	if runKey == "" {
		runKey = req.CorrelationID
	}
	execSessionID := req.SessionID
	if execSessionID == "" {
		execSessionID = req.CorrelationID
	}
	if execSessionID == "" {
		execSessionID = runKey
	}
	if execSessionID == "" {
		execSessionID = conv.ID
	}
	in := ToolInput{
		RunKey:           runKey,
		Step:             req.Step,
		Call:             domain.ToolCall{ID: callID, Name: req.Name, Arguments: args},
		Conversation:     conv,
		SessionID:        execSessionID,
		StartedAt:        time.Now().UTC(),
		ParentToolCallID: req.ParentToolCallID,
	}

	ctx = telemetry.WithFields(ctx, map[string]any{
		"turn_id":         runKey,
		"conversation_id": conv.ID,
		"trigger":         "coding",
	})
	ev := a.event(ctx, "tool.execute")
	ctx = ev.Context()
	ev.Set("tool", req.Name)
	ev.Set("session_id", req.SessionID)
	ev.Set("step", req.Step)
	ev.Set("tool_call_id", callID)
	if req.ParentToolCallID != "" {
		ev.Set("parent_tool_call_id", req.ParentToolCallID)
	}
	a.toolMetadata(ev, in)

	var traceOut string
	var traceErr error
	var searchMeta *domain.SearchTrace
	finishTrace := func(any, error, string) {}
	if req.SessionID != "" && a.Repo != nil {
		finishTrace = a.startTrace(ctx, domain.Trace{
			SessionID: req.SessionID,
			TurnID:    runKey,
			Trigger:   "coding",
			Kind:      "tool",
			Name:      req.Name,
			Step:      req.Step,
			CallID:    callID,
		}, args, ev)
	}
	defer func() {
		finishTrace(toolTraceOutput{output: traceOut, search: searchMeta}, traceErr, "")
		ev.Set("activity_result", "success")
		if traceErr != nil {
			ev.Set("activity_result", "error")
		}
		ev.End(traceErr)
	}()

	var accessErr error
	switch req.Name {
	case "group_read", "group_update":
		accessErr = a.groupRulesAccess(in)
	case "computer_exec", "computer_execution_status", "computer_execution_cancel", "computer_upload", "computer_download":
		accessErr = a.computerAccess(in)
	case "backup_status":
		accessErr = a.backupAccess(in.Conversation)
	case "logs_search":
		accessErr = a.logSearchAccess(in)
	case "task_update":
		accessErr = taskUpdateAccess(in)
	}
	if accessErr != nil {
		ev.Fail(accessErr)
		ev.Set("tool_outcome", "error")
		raw, _ := json.Marshal(map[string]string{"error": accessErr.Error(), "error_code": telemetry.Classify(accessErr).Code})
		traceOut = string(raw)
		return CodingToolResult{Output: raw, Failed: true}, nil
	}

	result, execErr := a.execute(ctx, in)
	if req.Name == "web_search" {
		searchMeta = a.buildSearchTrace(args, result, execErr)
		if searchMeta != nil && searchMeta.RateLimited {
			ev.Set("search_rate_limited", true)
		}
		if req.SessionID == "" && searchMeta != nil && a.Repo != nil {
			now := time.Now().UTC()
			status := "completed"
			if execErr != nil {
				status = "failed"
				if errors.Is(execErr, context.Canceled) || ctx.Err() != nil {
					status = "cancelled"
				}
			}
			anon := domain.Trace{
				ID:        ID("search-usage", fmt.Sprint(in.StartedAt.UnixNano()), callID),
				Trigger:   "coding",
				Kind:      "tool",
				Name:      "web_search",
				Status:    status,
				StartedAt: in.StartedAt,
				EndedAt:   &now,
				Search:    searchMeta,
			}
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := a.Repo.SaveTrace(c, anon); err != nil && a.Logger != nil {
				a.Logger.Warn("Anonymous search usage metric could not be saved", "operation", "inspection.trace", "trace_id", anon.ID)
			}
			cancel()
		}
	}
	if errors.Is(execErr, domain.ErrBrowserCapacity) {
		ev.Fail(diagnosticError("browser_capacity"))
		raw, _ := json.Marshal(map[string]string{"error": "browser capacity reached", "error_code": "browser_capacity"})
		traceOut = string(raw)
		return CodingToolResult{Output: raw, Failed: true}, nil
	}
	failed := false
	if execErr != nil {
		failed = true
		ev.Fail(execErr)
		ev.Set("tool_outcome", "error")
		diag := telemetry.Classify(execErr)
		errResult := map[string]string{"error": execErr.Error(), "error_code": diag.Code}
		var uncertain *computerExecutionError
		if errors.As(execErr, &uncertain) {
			errResult["execution_id"] = uncertain.id
			errResult["state"] = "unknown"
			errResult["note"] = "Inspect this execution ID; do not blindly repeat the command."
			result = errResult
		} else if req.Name == "web_search" {
			if enriched := a.webSearchErrorResult(execErr, diag); enriched != nil {
				result = enriched
			} else {
				result = errResult
			}
		} else {
			result = errResult
		}
	} else {
		ev.Set("tool_outcome", "success")
	}
	toolResultMetadata(ev, result)
	if resp, ok := result.(domain.SearchResponse); ok {
		result = fitSearchResponseBytes(resp, codemodeResultBytes)
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		traceErr = marshalErr
		return CodingToolResult{}, marshalErr
	}
	if len(raw) > codemodeResultBytes {
		raw, _ = json.Marshal(map[string]string{"result": clip(string(raw), 6000), "note": "truncated"})
	}
	traceOut = string(raw)

	var images []string
	if !failed && req.Name == "artifact_read" {
		var p struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(args, &p) == nil && p.ID != "" {
			if view, err := a.ArtifactView(ctx, conv.ID, p.ID); err == nil && len(view.Images) > 0 {
				images = view.Images
			}
		}
	}
	return CodingToolResult{Output: raw, Failed: failed, Images: images}, nil
}
