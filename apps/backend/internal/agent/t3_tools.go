package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"openalbert/apps/backend/internal/domain"
)

func t3Definitions() []domain.Tool {
	return []domain.Tool{
		{
			Name:        "t3_hosts",
			Description: "List registered LAN T3 Code hosts, their dynamic online status, last_seen_at timestamp, projects, and available provider/model catalog. Available only in owner-authorized contexts.",
			Parameters:  schema(``),
		},
		{
			Name:        "t3_threads_list",
			Description: "List tracked T3 Code threads stored in OpenAlbert, optionally filtered by host_id (or environment_id), conversation_id, project_id, or status.",
			Parameters:  schema(`"host_id":{"type":"string"},"conversation_id":{"type":"string"},"project_id":{"type":"string"},"status":{"type":"string"}`),
		},
		{
			Name:        "t3_thread_launch",
			Description: "Launch a new T3 Code thread on a registered LAN host. Defaults conversation_id to the current conversation. Waits briefly for immediate receipt from an online host, or returns status=\"queued\" with command_id if the host is offline.",
			Parameters:  schema(`"host_id":{"type":"string"},"project_id":{"type":"string"},"title":{"type":"string"},"message":{"type":"string"},"model_selection":{"type":"object"},"workspace_strategy":{"type":"object"},"output_schema":{"type":"object"},"conversation_id":{"type":"string"},"task_id":{"type":"string"},"attachment_ids":{"type":"array","items":{"type":"string"}}`, "host_id", "project_id", "title", "message", "model_selection"),
		},
		{
			Name:        "t3_thread_send",
			Description: "Send a follow-up message to an existing tracked T3 Code thread. Mode defaults to \"auto\" (supports \"auto\", \"steer\", \"queue\", \"restart\").",
			Parameters:  schema(`"thread_id":{"type":"string"},"message":{"type":"string"},"mode":{"type":"string","enum":["auto","steer","queue","restart"]},"output_schema":{"type":"object"},"attachment_ids":{"type":"array","items":{"type":"string"}}`, "thread_id", "message"),
		},
		{
			Name:        "t3_thread_read",
			Description: "Read messages or activity from a tracked T3 Code thread on its host. View defaults to \"messages\" (supports \"messages\" or \"activity\").",
			Parameters:  schema(`"thread_id":{"type":"string"},"view":{"type":"string","enum":["messages","activity"]},"limit":{"type":"integer","minimum":1,"maximum":100}`, "thread_id"),
		},
		{
			Name:        "t3_thread_respond",
			Description: "Answer a pending user question (pending_request) on a tracked T3 Code thread and clear its pending_request once accepted.",
			Parameters:  schema(`"thread_id":{"type":"string"},"request_id":{"type":"string"},"answers":{"type":"object"}`, "thread_id", "request_id", "answers"),
		},
		{
			Name:        "t3_thread_control",
			Description: "Control a tracked T3 Code thread: interrupt, settle, archive, or sync.",
			Parameters:  schema(`"thread_id":{"type":"string"},"action":{"type":"string","enum":["interrupt","settle","archive","sync"]}`, "thread_id", "action"),
		},
		{
			Name:        "t3_artifact_fetch",
			Description: "Fetch an image or file (up to 20 MiB) created by a T3 Code worker on its LAN host (within project workspace, thread worktree, ~/.t3, or system temp) and save it as an OpenAlbert artifact in the current conversation. Returns artifact_id for send_message({text, artifact_id}).",
			Parameters:  schema(`"thread_id":{"type":"string"},"path":{"type":"string"},"name":{"type":"string"}`, "thread_id", "path"),
		},
	}
}

func isT3Tool(name string) bool {
	switch name {
	case "t3_hosts", "t3_threads_list", "t3_thread_launch", "t3_thread_send", "t3_thread_read", "t3_thread_respond", "t3_thread_control", "t3_artifact_fetch":
		return true
	default:
		return false
	}
}

func (a *Activities) isT3AuthorizedConversation(c domain.Conversation) bool {
	return a.isOwnerAuthorized(c)
}

func (a *Activities) t3Access(in ToolInput) error {
	if !a.isT3AuthorizedConversation(in.Conversation) || in.Background {
		return errors.New("T3 control plane tools are only available in owner-authorized contexts")
	}
	if a.T3 == nil {
		return errors.New("T3 control plane is disabled")
	}
	return nil
}

// t3ConversationContext builds a compact live summary of T3 Code threads bound
// to the current owner-authorized conversation (or active threads when speaking
// in the workspace #Control topic) so follow-up questions like "Как там дела?"
// work even when a topic was opened by a zero-LLM launch card with no prior
// LLM messages in session history.
func (a *Activities) t3ConversationContext(ctx context.Context, conv domain.Conversation) string {
	if a.T3 == nil || !a.isT3AuthorizedConversation(conv) || strings.TrimSpace(conv.ID) == "" {
		return ""
	}
	threads, err := a.T3.ListThreads(ctx, domain.T3ThreadFilter{
		ConversationID: conv.ID,
		Limit:          10,
	})
	if err != nil {
		return ""
	}
	header := "T3 Code threads bound to this topic/conversation (live state from OpenAlbert):"
	if len(threads) == 0 && a.WorkspaceGroupID < 0 && conv.ID == fmt.Sprintf("telegram:group:%d:topic:0", a.WorkspaceGroupID) {
		all, listErr := a.T3.ListThreads(ctx, domain.T3ThreadFilter{Limit: 15})
		if listErr != nil {
			return ""
		}
		for _, th := range all {
			switch strings.ToLower(strings.TrimSpace(th.Status)) {
			case "running", "starting", "queued", "waiting_input":
				threads = append(threads, th)
			}
			if len(threads) >= 10 {
				break
			}
		}
		header = "Active T3 Code threads across workspace (live state from OpenAlbert):"
	}
	if len(threads) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	for _, th := range threads {
		fmt.Fprintf(&b, "\n- thread_id=%s | title=%q | status=%s", th.ThreadID, clipRunes(strings.TrimSpace(th.Title), 100), th.Status)
		if m := strings.TrimSpace(th.Model); m != "" {
			fmt.Fprintf(&b, " | model=%s", clipRunes(m, 60))
		}
		if br := strings.TrimSpace(th.Branch); br != "" {
			fmt.Fprintf(&b, " | branch=%s", clipRunes(br, 80))
		}
		if pr := strings.TrimSpace(th.PRURL); pr != "" {
			fmt.Fprintf(&b, " | pr=%s", clipRunes(pr, 160))
		}
		if th.PendingRequest != nil && strings.TrimSpace(th.PendingRequest.RequestID) != "" {
			fmt.Fprintf(&b, " | pending_request_id=%s", th.PendingRequest.RequestID)
		}
		if !th.UpdatedAt.IsZero() {
			fmt.Fprintf(&b, " | updated_at=%s", th.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
		}
		if s := strings.TrimSpace(th.LastSummary); s != "" {
			fmt.Fprintf(&b, "\n  last_summary: %s", clipRunes(strings.ReplaceAll(s, "\n", " "), 240))
		}
		if e := strings.TrimSpace(th.LastError); e != "" {
			fmt.Fprintf(&b, "\n  last_error: %s", clipRunes(strings.ReplaceAll(e, "\n", " "), 200))
		}
	}
	b.WriteString("\nWhen the owner asks for status, progress, or follow-ups in this topic, refer directly to the thread(s) above: call t3_thread_read(thread_id) to inspect live worker progress/messages, t3_thread_send(thread_id, message) to pass instructions, or t3_artifact_fetch(thread_id, path) to pull a screenshot/file from the host into this conversation and send it with send_message({text, artifact_id}). Never claim there is no context when a T3 thread is listed here.")
	return b.String()
}

func decodeStrictToolJSON(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	if len(trimmed) > 100000 || trimmed[0] != '{' {
		return errors.New("invalid T3 tool arguments")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid T3 tool arguments")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("invalid T3 tool arguments")
	}
	return nil
}

func t3CommandID(in ToolInput) string {
	if in.RunKey == "" || in.Call.ID == "" {
		return ""
	}
	return "t3cmd-" + ID("t3", in.RunKey, fmt.Sprint(in.Step), in.Call.ID)
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && json.Valid(trimmed)
}

func (a *Activities) executeT3(ctx context.Context, in ToolInput) (any, error) {
	if err := a.t3Access(in); err != nil {
		return nil, err
	}
	switch in.Call.Name {
	case "t3_hosts":
		var p struct{}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		hosts, err := a.T3.ListHosts(ctx)
		if err != nil {
			return nil, err
		}
		if hosts == nil {
			hosts = []domain.T3Host{}
		}
		return map[string]any{"hosts": hosts}, nil

	case "t3_threads_list":
		var p struct {
			HostID         string `json:"host_id"`
			ConversationID string `json:"conversation_id"`
			ProjectID      string `json:"project_id"`
			Status         string `json:"status"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		threads, err := a.T3.ListThreads(ctx, domain.T3ThreadFilter{
			HostID:         strings.TrimSpace(p.HostID),
			ConversationID: strings.TrimSpace(p.ConversationID),
			ProjectID:      strings.TrimSpace(p.ProjectID),
			Status:         strings.TrimSpace(p.Status),
		})
		if err != nil {
			return nil, err
		}
		if threads == nil {
			threads = []domain.T3Thread{}
		}
		return map[string]any{"threads": threads}, nil

	case "t3_thread_launch":
		var p struct {
			HostID            string          `json:"host_id"`
			ProjectID         string          `json:"project_id"`
			Title             string          `json:"title"`
			Message           string          `json:"message"`
			ModelSelection    json.RawMessage `json:"model_selection"`
			WorkspaceStrategy json.RawMessage `json:"workspace_strategy,omitempty"`
			OutputSchema      json.RawMessage `json:"output_schema,omitempty"`
			ConversationID    string          `json:"conversation_id,omitempty"`
			TaskID            string          `json:"task_id,omitempty"`
			AttachmentIDs     []string        `json:"attachment_ids,omitempty"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.HostID) == "" || strings.TrimSpace(p.ProjectID) == "" || strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Message) == "" {
			return nil, errors.New("host_id, project_id, title, and message are required")
		}
		if !isJSONObject(p.ModelSelection) {
			return nil, errors.New("model_selection must be a JSON object")
		}
		if len(bytes.TrimSpace(p.WorkspaceStrategy)) > 0 && string(bytes.TrimSpace(p.WorkspaceStrategy)) != "null" && !isJSONObject(p.WorkspaceStrategy) {
			return nil, errors.New("workspace_strategy must be a JSON object")
		}
		if len(bytes.TrimSpace(p.OutputSchema)) > 0 && string(bytes.TrimSpace(p.OutputSchema)) != "null" && !isJSONObject(p.OutputSchema) {
			return nil, errors.New("output_schema must be a JSON object")
		}
		host, err := a.T3.ResolveHost(ctx, p.HostID)
		if err != nil {
			return nil, err
		}
		convID := strings.TrimSpace(p.ConversationID)
		if convID == "" {
			convID = in.Conversation.ID
		}
		taskID := strings.TrimSpace(p.TaskID)
		if taskID == "" {
			taskID = in.TaskID
		}
		payload := map[string]any{
			"project_id":      strings.TrimSpace(p.ProjectID),
			"title":           strings.TrimSpace(p.Title),
			"message":         p.Message,
			"model_selection": json.RawMessage(bytes.TrimSpace(p.ModelSelection)),
			"conversation_id": convID,
		}
		if taskID != "" {
			payload["task_id"] = taskID
		}
		if isJSONObject(p.WorkspaceStrategy) {
			payload["workspace_strategy"] = json.RawMessage(bytes.TrimSpace(p.WorkspaceStrategy))
		}
		if isJSONObject(p.OutputSchema) {
			payload["output_schema"] = json.RawMessage(bytes.TrimSpace(p.OutputSchema))
		}
		if len(p.AttachmentIDs) > 0 {
			payload["attachment_ids"] = p.AttachmentIDs
			payload["attachment_conversation_id"] = in.Conversation.ID
		}
		res, err := a.T3.DispatchAndWait(ctx, t3CommandID(in), host.EnvironmentID, domain.T3ActionThreadLaunch, payload, 0)
		if err != nil {
			return nil, err
		}
		if res.Status == domain.T3CommandStatusFailed {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "T3 host rejected thread_launch command"
			}
			return nil, errors.New(msg)
		}
		return res, nil

	case "t3_thread_send":
		var p struct {
			ThreadID      string          `json:"thread_id"`
			Message       string          `json:"message"`
			Mode          string          `json:"mode,omitempty"`
			OutputSchema  json.RawMessage `json:"output_schema,omitempty"`
			AttachmentIDs []string        `json:"attachment_ids,omitempty"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		threadID := strings.TrimSpace(p.ThreadID)
		if threadID == "" || strings.TrimSpace(p.Message) == "" {
			return nil, errors.New("thread_id and message are required")
		}
		mode := strings.TrimSpace(p.Mode)
		if mode == "" {
			mode = "auto"
		}
		switch mode {
		case "auto", "steer", "queue", "restart":
		default:
			return nil, errors.New("mode must be one of auto, steer, queue, restart")
		}
		if len(bytes.TrimSpace(p.OutputSchema)) > 0 && string(bytes.TrimSpace(p.OutputSchema)) != "null" && !isJSONObject(p.OutputSchema) {
			return nil, errors.New("output_schema must be a JSON object")
		}
		thread, err := a.T3.Thread(ctx, threadID)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{
			"thread_id": thread.ThreadID,
			"message":   p.Message,
			"mode":      mode,
		}
		if isJSONObject(p.OutputSchema) {
			payload["output_schema"] = json.RawMessage(bytes.TrimSpace(p.OutputSchema))
		}
		if len(p.AttachmentIDs) > 0 {
			payload["attachment_ids"] = p.AttachmentIDs
			payload["attachment_conversation_id"] = in.Conversation.ID
		}
		res, err := a.T3.DispatchAndWait(ctx, t3CommandID(in), thread.EnvironmentID, domain.T3ActionThreadSend, payload, 0)
		if err != nil {
			return nil, err
		}
		if res.Status == domain.T3CommandStatusFailed {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "T3 host rejected thread_send command"
			}
			return nil, errors.New(msg)
		}
		return res, nil

	case "t3_thread_read":
		var p struct {
			ThreadID string `json:"thread_id"`
			View     string `json:"view,omitempty"`
			Limit    *int   `json:"limit,omitempty"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		threadID := strings.TrimSpace(p.ThreadID)
		if threadID == "" {
			return nil, errors.New("thread_id is required")
		}
		view := strings.TrimSpace(p.View)
		if view == "" {
			view = "messages"
		}
		if view != "messages" && view != "activity" {
			return nil, errors.New("view must be messages or activity")
		}
		if p.Limit != nil && (*p.Limit < 1 || *p.Limit > 100) {
			return nil, errors.New("limit must be between 1 and 100")
		}
		thread, err := a.T3.Thread(ctx, threadID)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{
			"thread_id": thread.ThreadID,
			"view":      view,
		}
		if p.Limit != nil {
			payload["limit"] = *p.Limit
		}
		res, err := a.T3.DispatchAndWait(ctx, t3CommandID(in), thread.EnvironmentID, domain.T3ActionThreadRead, payload, 0)
		if err != nil {
			return nil, err
		}
		if res.Status == domain.T3CommandStatusFailed {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "T3 host rejected thread_read command"
			}
			return nil, errors.New(msg)
		}
		return res, nil

	case "t3_thread_respond":
		var p struct {
			ThreadID  string          `json:"thread_id"`
			RequestID string          `json:"request_id"`
			Answers   json.RawMessage `json:"answers"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		threadID := strings.TrimSpace(p.ThreadID)
		requestID := strings.TrimSpace(p.RequestID)
		if threadID == "" || requestID == "" {
			return nil, errors.New("thread_id and request_id are required")
		}
		if !isJSONObject(p.Answers) {
			return nil, errors.New("answers must be a JSON object")
		}
		thread, err := a.T3.Thread(ctx, threadID)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{
			"thread_id":  thread.ThreadID,
			"request_id": requestID,
			"answers":    json.RawMessage(bytes.TrimSpace(p.Answers)),
		}
		res, err := a.T3.DispatchAndWait(ctx, t3CommandID(in), thread.EnvironmentID, domain.T3ActionThreadRespond, payload, 0)
		if err != nil {
			return nil, err
		}
		if res.Status == domain.T3CommandStatusFailed {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "T3 host rejected thread_respond command"
			}
			return nil, errors.New(msg)
		}
		return res, nil

	case "t3_thread_control":
		var p struct {
			ThreadID string `json:"thread_id"`
			Action   string `json:"action"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		threadID := strings.TrimSpace(p.ThreadID)
		action := strings.TrimSpace(p.Action)
		if threadID == "" {
			return nil, errors.New("thread_id is required")
		}
		switch action {
		case "interrupt", "settle", "archive", "sync":
		default:
			return nil, errors.New("action must be one of interrupt, settle, archive, sync")
		}
		thread, err := a.T3.Thread(ctx, threadID)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{
			"thread_id": thread.ThreadID,
			"action":    action,
		}
		res, err := a.T3.DispatchAndWait(ctx, t3CommandID(in), thread.EnvironmentID, domain.T3ActionThreadControl, payload, 0)
		if err != nil {
			return nil, err
		}
		if res.Status == domain.T3CommandStatusFailed {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "T3 host rejected thread_control command"
			}
			return nil, errors.New(msg)
		}
		return res, nil

	case "t3_artifact_fetch":
		var p struct {
			ThreadID string `json:"thread_id"`
			Path     string `json:"path"`
			Name     string `json:"name,omitempty"`
		}
		if err := decodeStrictToolJSON(in.Call.Arguments, &p); err != nil {
			return nil, err
		}
		threadID := strings.TrimSpace(p.ThreadID)
		filePath := strings.TrimSpace(p.Path)
		if threadID == "" || filePath == "" {
			return nil, errors.New("thread_id and path are required")
		}
		thread, err := a.T3.Thread(ctx, threadID)
		if err != nil {
			return nil, err
		}
		workspaceRoot := ""
		if host, hostErr := a.T3.ResolveHost(ctx, thread.EnvironmentID); hostErr == nil {
			for _, proj := range host.Projects {
				if proj.ID == thread.ProjectID && strings.TrimSpace(proj.WorkspaceRoot) != "" {
					workspaceRoot = strings.TrimSpace(proj.WorkspaceRoot)
					break
				}
			}
		}
		payload := map[string]any{
			"thread_id":       thread.ThreadID,
			"path":            filePath,
			"conversation_id": in.Conversation.ID,
		}
		if name := strings.TrimSpace(p.Name); name != "" {
			payload["name"] = name
		}
		if workspaceRoot != "" {
			payload["workspace_root"] = workspaceRoot
		}
		res, err := a.T3.DispatchAndWait(ctx, t3CommandID(in), thread.EnvironmentID, domain.T3ActionArtifactFetch, payload, 0)
		if err != nil {
			return nil, err
		}
		if res.Status == domain.T3CommandStatusFailed {
			msg := strings.TrimSpace(res.Error)
			if msg == "" {
				msg = "T3 host rejected artifact_fetch command"
			}
			return nil, errors.New(msg)
		}
		return res, nil

	default:
		return nil, errors.New("unknown T3 tool")
	}
}
