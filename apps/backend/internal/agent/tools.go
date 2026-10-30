package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
	"openalbert/apps/backend/internal/scheduling"
)

func ID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
func schema(properties string, required ...string) json.RawMessage {
	b, _ := json.Marshal(required)
	if required == nil {
		b = []byte("[]")
	}
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` + properties + `},"required":` + string(b) + `}`)
}

const sendMessageDescription = `Send a real Telegram message or saved artifact to the current conversation.

Use for direct answers, greetings that need acknowledgement, clarifications, due reminders, requested results, useful new information, or questions blocking progress. An ordinary final assistant response is internal only and is never delivered to Telegram. If the user expects a reply, call this tool with the answer even when no research or other tools are needed.

Background checks do not require notifications. If nothing warrants a message, finish with a final assistant response and no tool calls. It stays in internal history and need not be empty. No special marker or tool is required.

Examples: unchanged monitoring results need no message; a user asking "Any news?" needs an answer even if there is no news. Send routine status reports only when requested. Never send placeholders or announce a decision to stay silent.

Send once after completing the work. Success means queued delivery and ends the turn after the current tool batch. Do not send a follow-up confirmation.

To send an image or file with a description in one message, supply both text and artifact_id in the same call. Text that fits a 1024 UTF-16-unit caption after Markdown rendering is attached to the artifact. Longer descriptions are sent separately without truncation. Supported images are sent as photos; other artifacts are sent as documents.

Use original Rich Markdown without MarkdownV2 escaping. Limits: 32000 UTF-8 bytes, 500 blocks, 20 table columns, 16 nesting levels. Save larger content as an artifact.`

func Definitions() []domain.Tool {
	return []domain.Tool{
		{Name: "report_progress", Description: "Update the single temporary status message for the current interactive request, without finishing the turn. Use during lengthy work at meaningful stage changes: briefly state what you have actually done, what you are doing next, or a concrete obstacle. Plain text, at most 600 characters, in the user's language. Do not invent findings, percentages, completion estimates or activity; do not include internal reasoning. The application delays the first status by 30 seconds, coalesces updates and edits at most once a minute; it may suppress quick requests. This is not a reply or delivery confirmation. Continue working and use send_message for the final answer. Never send intermediate updates via send_message.", Parameters: schema(`"text":{"type":"string"}`, "text")},
		{Name: "web_search", Description: "Search the public web with Yandex; returns sources. Read sources before asserting facts.", Parameters: schema(`"query":{"type":"string"}`, "query")},
		{Name: "web_read", Description: "Fetch plain text from a known public HTTP(S) URL. Use for static articles, documentation or plain-text files when only their text is needed. Does not run JavaScript or share browser cookies. HTML extraction discards link destinations and flattens headings, lists and tables; long results may be truncated. For search results, catalogs, following links, structured tables or JavaScript content, use the listed browser tools. An HTTP success may contain a CAPTCHA or an empty page; check that the requested content is actually present. Private network addresses are forbidden.", Parameters: schema(`"url":{"type":"string"}`, "url")},
		{Name: "send_message", Description: sendMessageDescription, Parameters: schema(`"text":{"type":"string","description":"User-facing Rich Markdown: headings, GFM tables, lists, code, spoilers, formulas and details are supported. Use an empty string for an artifact without a caption."},"artifact_id":{"type":"string","description":"Optional ID of a saved artifact in this conversation to attach."}`, "text")},
		{Name: "send_group_message", Description: "Post the text your owner explicitly requests to a known allowed Telegram group or topic, only from the owner's interactive private chat. Get the exact conversation_id from conversation_list; ask if the intended group/topic is ambiguous. Use original Rich Markdown, up to 32000 UTF-8 bytes. Send only the intended public text, never private instructions/history. Returns queued, not confirmed delivered. Does not finish this turn: acknowledge the queued post privately via send_message. Do not repost it to check delivery. Text only; attachments are not supported by this tool.", Parameters: schema(`"conversation_id":{"type":"string"},"text":{"type":"string"}`, "conversation_id", "text")},
		{Name: "task_create", Description: "Create an independent durable task. Specify next_run_at RFC3339 for a reminder; schedule_seconds>0 for repeating checks. Explain reason for self-initiated tasks. Task continues after session closes.", Parameters: schema(`"title":{"type":"string"},"goal":{"type":"string"},"reason":{"type":"string"},"next_run_at":{"type":"string"},"schedule_seconds":{"type":"integer","minimum":0},"schedule_cron":{"type":"string"},"schedule_timezone":{"type":"string"},"maintenance":{"type":"boolean"}`, "title", "goal")},
		{Name: "task_find", Description: "Find assignments, reminders and monitoring tasks in the current conversation, including those created in earlier sessions. Use for schedules, progress, results and status questions, and before creating a possible duplicate; these belong to tasks, not memory_search. query is a literal case-insensitive substring of title or goal, or an exact task ID; omitted or empty query returns all saved tasks in this conversation, including completed ones. Inspect status and use task_read for details.", Parameters: schema(`"query":{"type":"string","description":"A literal substring of task title or goal, or an exact ID. Omit or use an empty string to list all saved tasks in this conversation."}`)},
		{Name: "task_read", Description: "Read saved task status, result, last update, next check. Reading does not execute it.", Parameters: schema(`"id":{"type":"string"}`, "id")},
		{Name: "task_update", Description: "Update a task goal, result, status or schedule. Status: queued, running, waiting, paused, completed, cancelled, failed. Completed and cancelled tasks stop. To resume use queued. Use for progress and stop requests.", Parameters: schema(`"id":{"type":"string"},"goal":{"type":"string"},"result":{"type":"string"},"status":{"type":"string"},"next_run_at":{"type":"string"},"schedule_seconds":{"type":"integer","minimum":0},"schedule_cron":{"type":"string"},"schedule_timezone":{"type":"string"},"maintenance":{"type":"boolean"}`, "id")},
		{Name: "conversation_list", Description: "List readable saved conversations. In the owner's private chat includes all known groups; elsewhere only the current conversation. Use before recalling group discussions. Does not send messages to those chats.", Parameters: schema(``)},
		{Name: "conversation_history", Description: "Search or read saved user/assistant messages across sessions, including active sessions. conversation_id defaults to this chat; only the owner's private chat can read groups. Results newest first with original scope, session and sequence. Use for earlier messages and who said what; memory_search is not a complete message log. Optional query is one literal case-insensitive substring of the message, not keywords combined with AND or OR; use a short distinctive fragment. Omit query or use an empty string to read recent messages. Use next_before_seq as before_seq to page backwards. session_started_at is the session start, not the message timestamp. Only messages actually received and processed by this bot are available. tool_calls lists the IDs and names of actual send_message calls; their text is included in content. An ordinary assistant text without send_message is internal run history. A send_message call is a request to send, not confirmation of delivery.", Parameters: schema(`"conversation_id":{"type":"string"},"query":{"type":"string","description":"One literal substring of a saved message, not a list of keywords. Omit or use an empty string to read recent messages."},"before_seq":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":50}`)},
		{Name: "memory_search", Description: "Search saved memory entries by literal, case-insensitive substrings, NOT semantic search. Every whitespace-separated query term must occur in the SAME entry (content, source or kind). Punctuation is literal; commas do not mean OR. No stemming, translation, synonyms, regex or wildcard syntax. Start with one distinctive name or keyword, for example Polka; search unrelated topics in separate calls. After an empty result, shorten the query or try an alternative spelling; adding words narrows the search. An empty query returns up to 50 most recently updated readable entries, not a complete export. An empty result means no matching entries, not that no memories exist. In the owner's private chat searches this chat and saved groups by default; optional conversation_id selects an authorized scope. Results include scope and source; do not attribute group participants' facts to the owner. Use task_find/task_read for assignments and schedules, conversation_history for earlier messages.", Parameters: schema(`"query":{"type":"string","description":"Literal terms that must all match one entry. Prefer a single distinctive term, e.g. Polka. Empty string lists recent entries; do not use comma-separated topics."},"conversation_id":{"type":"string","description":"Optional authorized conversation scope. Omit to search all scopes readable from this chat."}`, "query")},
		{Name: "memory_read", Description: "Read a memory and its source. For a group result found from the owner's private chat, supply its conversation_id from the result scope.", Parameters: schema(`"id":{"type":"string"},"conversation_id":{"type":"string"}`, "id")},
		{Name: "memory_update", Description: "Save useful user memory with provenance. Use profile for core user facts (name, home city, timezone), preference only for explicitly stated lasting preferences, fact for contextual facts, episode for past events, hypothesis for uncertain inferences. Profile and preference rebuild USER.md automatically. A one-time request/reminder is task state, not a lasting preference; do not save it as one. Update the same ID for corrections; delete obsolete memory using delete=true.", Parameters: schema(`"id":{"type":"string"},"content":{"type":"string"},"kind":{"type":"string","enum":["profile","fact","preference","episode","hypothesis"]},"delete":{"type":"boolean"}`)},
		{Name: "participant_search", Description: "Find group participants by name, username, Telegram ID or remembered facts. Within a group searches only that conversation; in the owner's private chat searches known groups. Optional conversation_id narrows scope. The same user in different groups has separate profiles.", Parameters: schema(`"query":{"type":"string"},"conversation_id":{"type":"string"}`)},
		{Name: "participant_read", Description: "Read a participant's group-scoped profile and facts with source quotes. Use user_id from Telegram metadata or participant_search, not a guessed identity. Owner private chat must specify a readable group conversation_id. Large profiles are bounded; use memory_read with a fact ID and conversation_id for its full content.", Parameters: schema(`"user_id":{"type":"integer","minimum":1},"conversation_id":{"type":"string"}`, "user_id")},
		{Name: "participant_update", Description: "Automatically remember useful facts a group participant explicitly stated about themselves, without waiting for 'remember'. Save their Telegram user_id, source_message_id and exact evidence quote from their own text. Use profile for core facts, preference for explicitly lasting preferences, fact for context. Never infer a person's preference from another speaker, assistant/tool output, jokes or a one-time request. Updates and deletions use an existing fact ID from participant_read. Only the originating group is writable; private chats cannot write group profiles. delete=true forgets that fact after quoting the participant's own request.", Parameters: schema(`"user_id":{"type":"integer","minimum":1},"source_message_id":{"type":"string"},"evidence":{"type":"string"},"id":{"type":"string"},"content":{"type":"string"},"kind":{"type":"string","enum":["profile","preference","fact"]},"delete":{"type":"boolean"}`, "user_id", "source_message_id", "evidence")},
		{Name: "soul_read", Description: "Read the current SOUL.md: your shared name, identity and personality. Use before editing to get the exact current text.", Parameters: schema(``)},
		{Name: "soul_update", Description: "Edit your SOUL.md when the user requests a change in a private conversation, for example renaming you. Replace one exact old_text fragment with new_text; old_text must match once, so include surrounding text when needed. Preserve unrelated instructions. Changes persist across sessions and apply when the next turn is prepared. Never use for group requests, background tasks or initiative checks.", Parameters: schema(`"old_text":{"type":"string"},"new_text":{"type":"string"}`, "old_text", "new_text")},
		{Name: "skill_read", Description: "Read a user-approved active skill; all active skill descriptions are in context.", Parameters: schema(`"name":{"type":"string"}`, "name")},
		{Name: "skill_propose_change", Description: "Save a proposed skill for joint discussion; does NOT publish it. User reviews files under proposals and installs accepted skills.", Parameters: schema(`"name":{"type":"string"},"description":{"type":"string"},"content":{"type":"string"}`, "name", "description", "content")},
		{Name: "artifact_save", Description: "Save a named text result or document to persistent storage; return an artifact ID to read or send later.", Parameters: schema(`"name":{"type":"string"},"content":{"type":"string"}`, "name", "content")},
		{Name: "artifact_read", Description: "Read a saved artifact belonging to this conversation.", Parameters: schema(`"id":{"type":"string"}`, "id")},
	}
}

type toolArgs struct {
	UserID           int64   `json:"user_id"`
	SourceMessageID  string  `json:"source_message_id"`
	Evidence         string  `json:"evidence"`
	ConversationID   string  `json:"conversation_id"`
	BeforeSeq        int64   `json:"before_seq"`
	Limit            int     `json:"limit"`
	Query            string  `json:"query"`
	URL              string  `json:"url"`
	Text             string  `json:"text"`
	ArtifactID       string  `json:"artifact_id"`
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Goal             string  `json:"goal"`
	Reason           string  `json:"reason"`
	Status           string  `json:"status"`
	Result           string  `json:"result"`
	NextRunAt        string  `json:"next_run_at"`
	ScheduleSeconds  *int    `json:"schedule_seconds"`
	ScheduleCron     *string `json:"schedule_cron"`
	ScheduleTimezone *string `json:"schedule_timezone"`
	Maintenance      *bool   `json:"maintenance"`
	Name             string  `json:"name"`
	Description      string  `json:"description"`
	Content          string  `json:"content"`
	Kind             string  `json:"kind"`
	OldText          string  `json:"old_text"`
	NewText          string  `json:"new_text"`
	Delete           bool    `json:"delete"`
}

type ToolInput struct {
	RunKey       string
	Step         int
	Call         domain.ToolCall
	Conversation domain.Conversation
	SessionID    string
	TaskID       string
	Background   bool
	StartedAt    time.Time
	ReceivedAt   time.Time
}

func taskUpdateAccess(in ToolInput) error {
	if in.Background {
		return errors.New("initiative checks cannot update existing tasks; read tasks or create a justified new task, and keep the check result in the run history")
	}
	return nil
}

func (a *Activities) execute(ctx context.Context, in ToolInput) (any, error) {
	if strings.HasPrefix(in.Call.Name, "computer_") {
		return a.executeComputer(ctx, in)
	}
	if in.Call.Name == "backup_status" {
		return a.backupStatus(in)
	}
	if in.Call.Name == "logs_search" {
		return a.searchLogs(ctx, in)
	}
	if strings.HasPrefix(in.Call.Name, "browser_") {
		return a.executeBrowser(ctx, in)
	}
	var p toolArgs
	if len(in.Call.Arguments) > 100000 {
		return nil, errors.New("tool arguments too large")
	}
	if err := json.Unmarshal(in.Call.Arguments, &p); err != nil {
		return nil, errors.New("invalid tool JSON")
	}
	scope := in.Conversation.ID
	key := ID(in.RunKey, fmt.Sprint(in.Step), in.Call.ID)
	now := time.Now().UTC()
	switch in.Call.Name {
	case "web_search":
		if !a.searchEnabled() {
			return nil, errors.New("web_search API tool is disabled; use listed browser tools to find sources or web_read for a known public URL")
		}
		if strings.TrimSpace(p.Query) == "" {
			return nil, errors.New("query required")
		}
		return a.Search.Search(ctx, p.Query)
	case "web_read":
		return a.Search.Read(ctx, p.URL)
	case "send_message":
		hasText := visibleMessageText(p.Text)
		if !hasText && p.ArtifactID == "" {
			return nil, errors.New("message has no visible text or artifact; if there is nothing to communicate, finish with a normal final assistant response and no tool calls")
		}
		if !utf8.ValidString(p.Text) || len(p.Text) > 32000 {
			return nil, errors.New("message must be valid UTF-8 and at most 32000 bytes; save larger content as artifact")
		}
		if (in.Background || in.TaskID != "") && backgroundPlaceholder(p.Text) {
			return nil, errors.New("background placeholder is not a notification; if there is nothing to communicate, finish with a normal final assistant response and no tool calls")
		}
		if p.ArtifactID != "" {
			_, found, err := a.Repo.Artifact(ctx, scope, p.ArtifactID)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, errors.New("artifact not found in this conversation")
			}
			if hasText {
				caption := messagefmt.Markdown(p.Text)
				if messagefmt.FitsCaption(caption) {
					// One durable delivery owns both media and caption, so an
					// uncertain acknowledgement can never retry just one part.
					out := domain.Outgoing{ID: key + "/000", Conversation: in.Conversation, Text: caption.Text, Markdown: p.Text, Entities: caption.Entities, Formatted: true, ArtifactID: p.ArtifactID, State: "pending", TurnID: in.RunKey, SessionID: in.SessionID, TaskID: in.TaskID, Trigger: in.TriggerName(), StartedAt: in.StartedAt, ReceivedAt: in.ReceivedAt, EnqueuedAt: now, PartCount: 1}
					if err := a.Repo.EnqueueOutgoing(ctx, out); err != nil {
						return nil, err
					}
					return map[string]string{"delivery_id": key, "state": "queued"}, nil
				}
			}
		}
		partCount := 0
		if hasText {
			partCount++
		}
		if p.ArtifactID != "" {
			partCount++
		}
		trace := domain.Outgoing{TurnID: in.RunKey, SessionID: in.SessionID, TaskID: in.TaskID, Trigger: in.TriggerName(), StartedAt: in.StartedAt, ReceivedAt: in.ReceivedAt, EnqueuedAt: now, PartCount: partCount}
		partIndex := 0
		if hasText {
			out := trace
			out.ID, out.Conversation, out.Text, out.State, out.PartIndex = key+"/000", in.Conversation, p.Text, "pending", partIndex
			partIndex++
			if err := a.Repo.EnqueueOutgoing(ctx, out); err != nil {
				return nil, err
			}
		}
		if p.ArtifactID != "" {
			out := trace
			out.ID, out.Conversation, out.ArtifactID, out.State, out.PartIndex = key+"/999-file", in.Conversation, p.ArtifactID, "pending", partIndex
			if err := a.Repo.EnqueueOutgoing(ctx, out); err != nil {
				return nil, err
			}
		}
		return map[string]string{"delivery_id": key, "state": "queued"}, nil
	case "report_progress":
		return a.reportProgress(ctx, in, p.Text)
	case "send_group_message":
		return a.sendGroupMessage(ctx, in, p, key, now)
	case "task_create":
		if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Goal) == "" {
			return nil, errors.New("title and goal required")
		}
		if in.TaskID != "" {
			return nil, errors.New("a task cannot create nested tasks; update current task")
		}
		next := now
		if p.NextRunAt != "" {
			var err error
			next, err = time.Parse(time.RFC3339, p.NextRunAt)
			if err != nil {
				return nil, errors.New("next_run_at must be RFC3339")
			}
		}
		interval := 0
		if p.ScheduleSeconds != nil {
			interval = *p.ScheduleSeconds
		}
		if interval < 0 || (interval > 0 && time.Duration(interval)*time.Second < a.MinTaskInterval) {
			return nil, fmt.Errorf("schedule must be at least %d seconds", int(a.MinTaskInterval.Seconds()))
		}
		task := domain.Task{ID: key, ConversationID: scope, Title: p.Title, Goal: p.Goal, Reason: p.Reason, Status: "queued", ScheduleSeconds: interval, NextRunAt: next, CreatedAt: now, UpdatedAt: now}
		if p.Maintenance != nil {
			if in.Background {
				return nil, errors.New("only an interactive request may configure maintenance authority")
			}
			task.Maintenance = *p.Maintenance
		}
		if p.ScheduleCron != nil {
			task.ScheduleCron = *p.ScheduleCron
		}
		if p.ScheduleTimezone != nil {
			task.ScheduleTimezone = *p.ScheduleTimezone
		}
		if err := scheduling.Validate(task); err != nil {
			return nil, err
		}
		if task.ScheduleCron != "" && p.NextRunAt == "" {
			var err error
			task.NextRunAt, err = scheduling.Next(task, now)
			if err != nil {
				return nil, err
			}
		}
		return a.Repo.CreateTask(ctx, task)
	case "task_find":
		return a.Repo.FindTasks(ctx, scope, p.Query)
	case "task_read":
		t, found, err := a.Repo.Task(ctx, scope, p.ID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("task not found")
		}
		return t, nil
	case "task_update":
		if err := taskUpdateAccess(in); err != nil {
			return nil, err
		}
		t, found, err := a.Repo.Task(ctx, scope, p.ID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New("task not found")
		}
		maintenance := in.TaskID != "" && in.TaskID != t.ID
		if maintenance {
			actor, found, err := a.Repo.Task(ctx, scope, in.TaskID)
			if err != nil {
				return nil, err
			}
			if !found || !actor.Maintenance {
				return nil, errors.New("task run may only update itself")
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(in.Call.Arguments, &fields); err != nil {
				return nil, err
			}
			for field := range fields {
				if field != "id" && field != "status" && field != "next_run_at" {
					return nil, errors.New("maintenance may only update next_run_at and status")
				}
			}
		}
		if p.Maintenance != nil && in.TaskID != "" {
			return nil, errors.New("task runs cannot change maintenance authority")
		}
		if !maintenance && in.TaskID != "" && (t.Status == "cancelled" || t.Status == "paused") {
			return nil, errors.New("task stopped by user")
		}
		if p.Goal != "" {
			t.Goal = p.Goal
		}
		if p.Status != "" {
			if !validStatus(p.Status) {
				return nil, errors.New("invalid task status")
			}
			t.Status = p.Status
		}
		if p.Result != "" {
			t.Result = p.Result
		}
		if p.NextRunAt != "" {
			t.NextRunAt, err = time.Parse(time.RFC3339, p.NextRunAt)
			if err != nil {
				return nil, err
			}
		}
		if p.ScheduleSeconds != nil {
			if *p.ScheduleSeconds < 0 || (*p.ScheduleSeconds > 0 && time.Duration(*p.ScheduleSeconds)*time.Second < a.MinTaskInterval) {
				return nil, errors.New("task interval below configured minimum")
			}
			t.ScheduleSeconds = *p.ScheduleSeconds
		}
		if p.ScheduleCron != nil {
			t.ScheduleCron = *p.ScheduleCron
		}
		if p.ScheduleTimezone != nil {
			t.ScheduleTimezone = *p.ScheduleTimezone
		}
		if p.Maintenance != nil {
			t.Maintenance = *p.Maintenance
		}
		if err := scheduling.Validate(t); err != nil {
			return nil, err
		}
		t.UpdatedAt = now
		var updated domain.Task
		var updateErr error
		if maintenance {
			audit := a.event(ctx, "task.maintenance")
			audit.Set("actor_task_id", in.TaskID)
			audit.Set("target_task_id", t.ID)
			audit.Set("mutation_id", key)
			audit.Set("next_run_at", t.NextRunAt)
			audit.Set("status", t.Status)
			updated, updateErr = a.Repo.MaintainTask(audit.Context(), in.TaskID, in.RunKey, key, t)
			audit.End(updateErr)
		} else {
			updated, updateErr = a.Repo.UpdateTask(ctx, t)
		}
		if updateErr == nil && a.Browser != nil && (terminal(updated.Status) || updated.Status == "paused") {
			a.Browser.CloseTask(updated.ID)
		}
		if updateErr == nil && a.TaskChanged != nil {
			_ = a.TaskChanged(ctx, scope, t.ID)
		}
		return updated, updateErr
	case "conversation_list":
		return a.readableConversations(ctx, in.Conversation)
	case "conversation_history":
		return a.conversationHistory(ctx, in.Conversation, p)
	case "memory_search":
		return a.searchMemories(ctx, in.Conversation, p)
	case "memory_read":
		target, err := a.readScope(ctx, in.Conversation, p.ConversationID)
		if err != nil {
			return nil, err
		}
		return a.Knowledge.Read(ctx, target, p.ID)
	case "memory_update":
		if p.Delete && p.ID != "" {
			previous, err := a.Knowledge.Read(ctx, scope, p.ID)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if previous.SubjectUserID > 0 {
				return nil, errors.New("use participant_update to change or delete a participant fact")
			}
		}
		if !in.Conversation.Private && (p.Kind == "profile" || p.Kind == "preference") {
			return nil, errors.New("use participant_update with author evidence for a participant profile or preference")
		}

		if p.ID == "" {
			p.ID = key
		}
		if p.Delete {
			return map[string]string{"deleted": p.ID}, a.Knowledge.Delete(ctx, scope, p.ID)
		}
		if strings.TrimSpace(p.Content) == "" {
			return nil, errors.New("memory content required")
		}
		if p.Kind == "" {
			p.Kind = "fact"
		}
		if p.Kind != "profile" && p.Kind != "fact" && p.Kind != "preference" && p.Kind != "episode" && p.Kind != "hypothesis" {
			return nil, errors.New("invalid memory kind")
		}
		entry := domain.MemoryEntry{ID: p.ID, Scope: scope, Content: p.Content, Source: in.SessionID, Kind: p.Kind, UpdatedAt: now}
		return entry, a.Knowledge.Update(ctx, entry)
	case "participant_search":
		return a.searchParticipants(ctx, in.Conversation, p)
	case "participant_read":
		target, err := a.participantScope(ctx, in.Conversation, p.ConversationID)
		if err != nil {
			return nil, err
		}
		profile, err := a.Knowledge.Participant(ctx, target, p.UserID)
		if err != nil {
			return nil, err
		}
		return participantProfileView(profile), nil
	case "participant_update":
		return a.updateParticipant(ctx, in, p, key, now)
	case "soul_read":
		content, err := a.Knowledge.Soul(ctx)
		return map[string]string{"content": content}, err
	case "soul_update":
		if !in.Conversation.Private || in.Background || in.TaskID != "" {
			return nil, errors.New("SOUL.md can only be changed in an interactive private conversation")
		}
		if err := a.Knowledge.UpdateSoul(ctx, p.OldText, p.NewText); err != nil {
			return nil, err
		}
		return map[string]string{"state": "updated", "applies": "next_turn"}, nil
	case "skill_read":
		return a.Knowledge.ReadSkill(ctx, p.Name)
	case "skill_propose_change":
		proposal := p.Name + "-" + key
		return map[string]string{"state": "proposal_only", "id": proposal}, a.Knowledge.ProposeSkill(ctx, proposal, p.Description, p.Content)
	case "artifact_save":
		if p.Name == "" || p.Content == "" {
			return nil, errors.New("name and content required")
		}
		if len(p.Content) > 1<<20 {
			return nil, errors.New("artifact too large")
		}
		art := domain.Artifact{ID: key, ConversationID: scope, Name: p.Name, MIME: "text/plain; charset=utf-8", Data: []byte(p.Content), CreatedAt: now}
		return map[string]string{"id": key, "name": p.Name}, a.Repo.SaveArtifact(ctx, art)
	case "artifact_read":
		art, ok, err := a.Repo.Artifact(ctx, scope, p.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("artifact not found")
		}
		if strings.HasPrefix(art.MIME, "text/") || strings.Contains(art.MIME, "json") {
			return map[string]string{"id": art.ID, "name": art.Name, "content": clip(string(art.Data), 30000)}, nil
		}
		return map[string]any{"id": art.ID, "name": art.Name, "mime": art.MIME, "bytes": len(art.Data), "hint": "send_message can attach this artifact"}, nil
	default:
		return nil, errors.New("unknown tool")
	}
}
func validStatus(s string) bool {
	switch s {
	case "queued", "running", "waiting", "paused", "completed", "cancelled", "failed":
		return true
	}
	return false
}
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "\n[truncated]"
	}
	return s
}

func (a *Activities) searchEnabled() bool {
	if a.Search == nil {
		return false
	}
	if capability, ok := a.Search.(interface{ SearchEnabled() bool }); ok {
		return capability.SearchEnabled()
	}
	return true
}
func visibleMessageText(text string) bool {
	for _, r := range text {
		if unicode.IsGraphic(r) && !unicode.IsSpace(r) && !unicode.IsMark(r) {
			return true
		}
	}
	return false
}

// Only recognize standalone protocol markers, not ordinary prose or quoted examples.
// Interactive replies may legitimately repeat a marker at the user's request.
func backgroundPlaceholder(text string) bool {
	marker := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, text)
	switch marker {
	case "placeholder", "noreply", "nomessage", "noop":
		return true
	}
	return false
}

func (a *Activities) toolDefinitions() []domain.Tool {
	tools := Definitions()
	if a.Backups != nil {
		tools = append(tools, backupDefinition())
	}
	if a.Logs != nil {
		tools = append(tools, logDefinition())
	}
	if a.Browser != nil {
		tools = append(tools, browserDefinitions()...)
	}
	if a.Computer != nil {
		tools = append(tools, computerDefinitions()...)
	}
	available := make([]domain.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name != "web_search" || a.searchEnabled() {
			available = append(available, tool)
		}
	}
	return available
}

// Global personality changes are only available in interactive private turns.
func (a *Activities) toolsForTurn(turn Turn) []domain.Tool {
	tools := a.toolDefinitions()
	interactive := turn.Input.TaskID == "" && !turn.Input.Initiative
	filtered := make([]domain.Tool, 0, len(tools))
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "computer_") && (!a.isOwnerPrivate(turn.Conversation) || turn.Input.Initiative) {
			continue
		}
		if tool.Name == "backup_status" && !a.isOwnerPrivate(turn.Conversation) {
			continue
		}
		if tool.Name == "task_update" && turn.Input.Initiative {
			continue
		}
		if tool.Name == "logs_search" && (!a.isOwnerPrivate(turn.Conversation) || turn.Input.Initiative) {
			continue
		}
		if tool.Name == "report_progress" && !interactive {
			continue
		}
		if tool.Name == "participant_update" && turn.Conversation.Private {
			continue
		}
		if tool.Name == "soul_update" && (!turn.Conversation.Private || !interactive) {
			continue
		}
		if tool.Name == "send_group_message" && (!a.isOwnerPrivate(turn.Conversation) || !interactive) {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}
