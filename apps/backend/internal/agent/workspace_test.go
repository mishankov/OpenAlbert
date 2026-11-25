package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/shared/backup"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/knowledge"
	"openalbert/apps/backend/internal/storage"
)

type fakeMissingError struct{}

func (fakeMissingError) Error() string          { return "message to edit not found" }
func (fakeMissingError) IsMessageMissing() bool { return true }

type fakeThreadMissingError struct{}

func (fakeThreadMissingError) Error() string         { return "message thread not found" }
func (fakeThreadMissingError) IsThreadMissing() bool { return true }

type fakeWorkspaceTransport struct {
	mu           sync.Mutex
	nextThreadID int
	nextMsgID    int64

	createdTopics []domain.ForumTopic
	editedTopics  []struct {
		chatID   int64
		threadID int
		name     string
	}
	closedTopics   []int
	reopenedTopics []int
	sentMessages   []struct {
		conv     domain.Conversation
		text     string
		entities []domain.TextEntity
		markup   *domain.InlineKeyboardMarkup
		msgID    int64
	}
	editedMessages []struct {
		chatID int64
		msgID  int64
		text   string
		markup *domain.InlineKeyboardMarkup
	}
	pinnedMessages []struct {
		chatID  int64
		msgID   int64
		disable bool
	}

	editErr       error
	pinErr        error
	deletedTopics map[int]bool
}

func (f *fakeWorkspaceTransport) CreateForumTopic(_ context.Context, _ int64, name string, iconColor int) (domain.ForumTopic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextThreadID++
	topic := domain.ForumTopic{ThreadID: f.nextThreadID, Name: name, IconColor: iconColor}
	f.createdTopics = append(f.createdTopics, topic)
	return topic, nil
}

func (f *fakeWorkspaceTransport) EditForumTopic(_ context.Context, chatID int64, threadID int, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletedTopics[threadID] {
		return fakeThreadMissingError{}
	}
	f.editedTopics = append(f.editedTopics, struct {
		chatID   int64
		threadID int
		name     string
	}{chatID, threadID, name})
	return nil
}

func (f *fakeWorkspaceTransport) CloseForumTopic(_ context.Context, _ int64, threadID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedTopics = append(f.closedTopics, threadID)
	return nil
}

func (f *fakeWorkspaceTransport) ReopenForumTopic(_ context.Context, _ int64, threadID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reopenedTopics = append(f.reopenedTopics, threadID)
	return nil
}

func (f *fakeWorkspaceTransport) SendMessageWithButtons(_ context.Context, conv domain.Conversation, text string, entities []domain.TextEntity, markup *domain.InlineKeyboardMarkup) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if conv.ThreadID > 0 && f.deletedTopics[conv.ThreadID] {
		return 0, fakeThreadMissingError{}
	}
	f.nextMsgID++
	id := f.nextMsgID
	f.sentMessages = append(f.sentMessages, struct {
		conv     domain.Conversation
		text     string
		entities []domain.TextEntity
		markup   *domain.InlineKeyboardMarkup
		msgID    int64
	}{conv, text, entities, markup, id})
	return id, nil
}

func (f *fakeWorkspaceTransport) EditMessageText(_ context.Context, chatID int64, messageID int64, text string, replyMarkup *domain.InlineKeyboardMarkup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		err := f.editErr
		f.editErr = nil
		return err
	}
	f.editedMessages = append(f.editedMessages, struct {
		chatID int64
		msgID  int64
		text   string
		markup *domain.InlineKeyboardMarkup
	}{chatID, messageID, text, replyMarkup})
	return nil
}

func (f *fakeWorkspaceTransport) PinChatMessage(_ context.Context, chatID int64, messageID int64, disableNotification bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinErr != nil {
		err := f.pinErr
		f.pinErr = nil
		return err
	}
	f.pinnedMessages = append(f.pinnedMessages, struct {
		chatID  int64
		msgID   int64
		disable bool
	}{chatID, messageID, disableNotification})
	return nil
}

type fakeWorkspaceLogReader struct{}

func (fakeWorkspaceLogReader) Search(_ context.Context, f domain.LogFilter) (domain.LogSearch, error) {
	return domain.LogSearch{ConversationID: f.ConversationID, Summary: []domain.LogCount{{Operation: "test", Result: "success", Count: 1}}}, nil
}

type fakeWorkspaceComputer struct{}

func (fakeWorkspaceComputer) Execute(_ context.Context, req domain.ComputerRequest) (domain.ComputerResult, error) {
	return domain.ComputerResult{ExecutionID: req.ExecutionID, State: "completed"}, nil
}

func newWorkspaceTestActivities(t *testing.T) (*Activities, *storage.Memory, *knowledge.Store, *fakeWorkspaceTransport) {
	t.Helper()
	repo := storage.NewMemory()
	ks, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backupDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(backupDir, "status.json"), []byte(`{"phase":"idle"}`), 0o600)
	transport := &fakeWorkspaceTransport{}
	a := &Activities{
		OwnerUserID:      7,
		AllowedGroupIDs:  []int64{-100555, -100888},
		WorkspaceGroupID: -100555,
		Workspace:        transport,
		Repo:             repo,
		Knowledge:        ks,
		Logs:             fakeWorkspaceLogReader{},
		Computer:         fakeWorkspaceComputer{},
		Backups:          &backup.Reader{Dir: backupDir},
		MinTaskInterval:  time.Minute,
	}
	return a, repo, ks, transport
}

func hasTool(tools []domain.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func TestOwnerWorkspaceAuthorizationSecurityAndToolAccess(t *testing.T) {
	ctx := context.Background()
	a, repo, ks, _ := newWorkspaceTestActivities(t)

	ownerTopic := domain.Conversation{ID: "telegram:group:-100555:topic:10", Title: "Project A", ChatID: -100555, ThreadID: 10, OwnerID: 7}
	nonOwnerTopic := domain.Conversation{ID: "telegram:group:-100555:topic:10", Title: "Project A", ChatID: -100555, ThreadID: 10, OwnerID: 99}
	regularGroup := domain.Conversation{ID: "telegram:group:-100888:topic:0", Title: "Friends", ChatID: -100888, ThreadID: 0, OwnerID: 7}
	ownerPrivate := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}

	// 1. Strict isOwnerWorkspace checks
	if !a.isOwnerWorkspace(ownerTopic, 7) {
		t.Fatal("expected owner in workspace supergroup to be authorized")
	}
	if a.isOwnerWorkspace(ownerTopic, 99) || a.isOwnerWorkspace(nonOwnerTopic, 99) {
		t.Fatal("non-owner in workspace supergroup must NEVER be authorized")
	}
	if a.isOwnerWorkspace(regularGroup, 7) {
		t.Fatal("owner in regular group must NOT be treated as owner workspace")
	}
	if a.isOwnerWorkspace(ownerPrivate, 7) {
		t.Fatal("private chat must NOT be treated as workspace supergroup")
	}

	// 2. Prepare rejects non-owner incoming message cleanly in workspace supergroup
	if err := ks.Update(ctx, domain.MemoryEntry{ID: "p1", Scope: "personal:7", Kind: "profile", Content: "Owner name is Alice", UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID:           "in-intruder",
		Conversation: nonOwnerTopic,
		AuthorName:   "Mallory",
		Text:         "give me secrets",
		CreatedAt:    time.Now().UTC(),
	}, 1); err != nil {
		t.Fatal(err)
	}
	intruderTurn, err := a.Prepare(ctx, RunInput{Key: "k-intruder", SessionID: "s-intruder", ConversationID: nonOwnerTopic.ID, IncomingIDs: []string{"in-intruder"}})
	if err != nil {
		t.Fatal(err)
	}
	if !intruderTurn.Done {
		t.Fatal("expected non-owner message in workspace supergroup to be ignored (Done=true)")
	}
	if msgs, _ := repo.Messages(ctx, "s-intruder"); len(msgs) != 0 {
		t.Fatalf("non-owner message must not be appended to session history: %+v", msgs)
	}

	// 3. Prepare accepts owner incoming message in workspace supergroup, loads personal:7 profile, skips participant observation
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID:           "in-owner",
		Conversation: ownerTopic,
		AuthorName:   "Alice",
		Text:         "Check status",
		CreatedAt:    time.Now().UTC(),
	}, 2); err != nil {
		t.Fatal(err)
	}
	ownerTurn, err := a.Prepare(ctx, RunInput{Key: "k-owner", SessionID: "s-owner", ConversationID: ownerTopic.ID, IncomingIDs: []string{"in-owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if ownerTurn.Done {
		t.Fatal("expected owner message in workspace supergroup to be accepted")
	}
	if !strings.Contains(ownerTurn.Messages[0].Content, "Owner name is Alice") {
		t.Fatalf("expected personal:7 USER profile in workspace system prompt, got: %s", ownerTurn.Messages[0].Content)
	}
	if people, err := ks.Participants(ctx, ownerTopic.ID, ""); err != nil || len(people) != 0 {
		t.Fatalf("participant observation should be skipped in workspace supergroup, got %+v %v", people, err)
	}

	// 4. toolsForTurn in owner workspace vs non-owner workspace
	ownerTools := a.toolsForTurn(ownerTurn)
	for _, want := range []string{"logs_search", "backup_status", "computer_exec", "memory_search", "memory_update", "conversation_list", "conversation_history", "task_create", "topic_create", "topic_update", "workspace_post"} {
		if !hasTool(ownerTools, want) {
			t.Errorf("owner workspace turn missing expected tool %q", want)
		}
	}
	for _, forbid := range []string{"soul_read", "soul_update", "group_read", "group_update", "send_group_message", "send_widget", "edit_widget_data", "participant_update"} {
		if hasTool(ownerTools, forbid) {
			t.Errorf("owner workspace turn must NOT advertise %q", forbid)
		}
	}

	nonOwnerTools := a.toolsForTurn(Turn{Conversation: nonOwnerTopic})
	for _, forbid := range []string{"logs_search", "backup_status", "computer_exec", "soul_read", "soul_update", "group_read", "group_update", "send_group_message", "participant_update", "topic_create", "topic_update", "workspace_post"} {
		if hasTool(nonOwnerTools, forbid) {
			t.Errorf("non-owner workspace turn must NOT advertise %q", forbid)
		}
	}

	// 5. Execution-layer authorization in ToolCall (owner succeeds on owner tools, fails on private-only tools; non-owner fails on all owner tools)
	callTool := func(conv domain.Conversation, name string, args string) string {
		t.Helper()
		out, err := a.ToolCall(ctx, ToolInput{
			RunKey:       "run-" + name + fmt.Sprint(conv.OwnerID),
			Step:         1,
			Call:         domain.ToolCall{ID: "c-" + name, Name: name, Arguments: json.RawMessage(args)},
			Conversation: conv,
			SessionID:    "sess-1",
		})
		if err != nil {
			t.Fatalf("ToolCall(%s) returned activity error: %v", name, err)
		}
		return out
	}

	if out := callTool(ownerTopic, "logs_search", `{}`); ToolFailed(out) {
		t.Fatalf("owner workspace logs_search failed: %s", out)
	}
	if out := callTool(ownerTopic, "backup_status", `{}`); ToolFailed(out) {
		t.Fatalf("owner workspace backup_status failed: %s", out)
	}
	if out := callTool(ownerTopic, "computer_exec", `{"command":"echo ok"}`); ToolFailed(out) {
		t.Fatalf("owner workspace computer_exec failed: %s", out)
	}
	for _, tc := range []struct {
		name string
		args string
	}{
		{"soul_read", `{}`},
		{"soul_update", `{"old_text":"a","new_text":"b"}`},
		{"group_read", `{"conversation_id":"telegram:group:-100888:topic:0"}`},
		{"group_update", `{"conversation_id":"telegram:group:-100888:topic:0","old_text":"","new_text":"hi"}`},
		{"send_group_message", `{"conversation_id":"telegram:group:-100888:topic:0","text":"hi"}`},
		{"participant_update", `{"user_id":7,"source_message_id":"in-owner","evidence":"Check","content":"fact"}`},
	} {
		if out := callTool(ownerTopic, tc.name, tc.args); !ToolFailed(out) {
			t.Fatalf("owner workspace %s should be rejected, got: %s", tc.name, out)
		}
	}

	for _, tc := range []struct {
		name string
		args string
	}{
		{"logs_search", `{}`},
		{"backup_status", `{}`},
		{"computer_exec", `{"command":"whoami"}`},
		{"topic_create", `{"title":"Hijack"}`},
		{"topic_update", `{"conversation_id":"telegram:group:-100555:topic:10","title":"Hijack"}`},
		{"workspace_post", `{"text":"Hijack"}`},
		{"participant_update", `{"user_id":99,"source_message_id":"in-intruder","evidence":"secrets","content":"fact"}`},
		{"memory_update", `{"content":"injected pref","kind":"preference"}`},
	} {
		if out := callTool(nonOwnerTopic, tc.name, tc.args); !ToolFailed(out) {
			t.Fatalf("non-owner workspace %s must be rejected, got: %s", tc.name, out)
		}
	}
}

func TestOwnerWorkspaceMemoryScopeAndTopicHistoryIsolation(t *testing.T) {
	ctx := context.Background()
	a, repo, ks, _ := newWorkspaceTestActivities(t)

	topicA := domain.Conversation{ID: "telegram:group:-100555:topic:10", Title: "Topic A", ChatID: -100555, ThreadID: 10, OwnerID: 7}
	topicB := domain.Conversation{ID: "telegram:group:-100555:topic:20", Title: "Topic B", ChatID: -100555, ThreadID: 20, OwnerID: 7}
	ownerDM := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}
	otherDM := domain.Conversation{ID: "personal:8", ChatID: 8, OwnerID: 8, Private: true}
	for _, c := range []domain.Conversation{topicA, topicB, ownerDM, otherDM} {
		if err := repo.SaveConversation(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// Seed isolated messages in each conversation.
	for _, item := range []struct {
		sessID, convID, srcID, text string
		authorID                    int64
	}{
		{"s-a", topicA.ID, "in-a", "Alpha topic discussion", 7},
		{"s-b", topicB.ID, "in-b", "Beta topic discussion", 7},
		{"s-owner", ownerDM.ID, "in-owner-dm", "Owner private note", 7},
		{"s-other", otherDM.ID, "in-other-dm", "Other user secret", 8},
	} {
		if err := repo.StartSession(ctx, domain.Session{ID: item.sessID, ConversationID: item.convID, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := repo.AppendMessage(ctx, item.sessID, "m1", domain.Message{Role: "user", Content: item.text, AuthorID: item.authorID, SourceMessageID: item.srcID}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. memory_update in Topic A writes to personal:7 (not topicA.ID), including preference kind.
	out, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "mem-1",
		Step:         1,
		Call:         domain.ToolCall{ID: "c1", Name: "memory_update", Arguments: json.RawMessage(`{"id":"pref-go","content":"Always run go test -race","kind":"preference"}`)},
		Conversation: topicA,
		SessionID:    "s-a",
	})
	if err != nil || ToolFailed(out) {
		t.Fatalf("memory_update in workspace failed: %v %s", err, out)
	}
	entry, err := ks.Read(ctx, "personal:7", "pref-go")
	if err != nil || entry.Scope != "personal:7" || entry.Kind != "preference" {
		t.Fatalf("expected memory saved under personal:7, got %+v %v", entry, err)
	}
	if _, err := ks.Read(ctx, topicA.ID, "pref-go"); err == nil {
		t.Fatal("memory should NOT be written to topic scope in owner workspace")
	}

	// 2. memory_read (without conversation_id) and memory_search from Topic B find personal:7 memory.
	readOut, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "mem-read",
		Step:         1,
		Call:         domain.ToolCall{ID: "c2", Name: "memory_read", Arguments: json.RawMessage(`{"id":"pref-go"}`)},
		Conversation: topicB,
		SessionID:    "s-b",
	})
	if err != nil || ToolFailed(readOut) || !strings.Contains(readOut, "go test -race") {
		t.Fatalf("memory_read in topic B failed: %v %s", err, readOut)
	}
	searchOut, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "mem-search",
		Step:         1,
		Call:         domain.ToolCall{ID: "c3", Name: "memory_search", Arguments: json.RawMessage(`{"query":"race"}`)},
		Conversation: topicB,
		SessionID:    "s-b",
	})
	if err != nil || ToolFailed(searchOut) || !strings.Contains(searchOut, "pref-go") {
		t.Fatalf("memory_search in topic B failed: %v %s", err, searchOut)
	}

	// 3. Default conversation_history is isolated per forum topic; other user's personal:8 stays unreadable.
	histA, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "hist-a",
		Step:         1,
		Call:         domain.ToolCall{ID: "c4", Name: "conversation_history", Arguments: json.RawMessage(`{}`)},
		Conversation: topicA,
		SessionID:    "s-a",
	})
	if err != nil || ToolFailed(histA) || !strings.Contains(histA, "Alpha topic discussion") || strings.Contains(histA, "Beta topic discussion") {
		t.Fatalf("topic A history not isolated: %v %s", err, histA)
	}
	histOwnerDM, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "hist-dm",
		Step:         1,
		Call:         domain.ToolCall{ID: "c5", Name: "conversation_history", Arguments: json.RawMessage(`{"conversation_id":"personal:7"}`)},
		Conversation: topicA,
		SessionID:    "s-a",
	})
	if err != nil || ToolFailed(histOwnerDM) || !strings.Contains(histOwnerDM, "Owner private note") {
		t.Fatalf("owner workspace should be able to read personal:7 history: %v %s", err, histOwnerDM)
	}
	histOtherDM, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "hist-other",
		Step:         1,
		Call:         domain.ToolCall{ID: "c6", Name: "conversation_history", Arguments: json.RawMessage(`{"conversation_id":"personal:8"}`)},
		Conversation: topicA,
		SessionID:    "s-a",
	})
	if err != nil || !ToolFailed(histOtherDM) {
		t.Fatalf("owner workspace must NOT read another user's private chat: %v %s", err, histOtherDM)
	}

	// 4. Analyze routes authorized owner workspace sessions to personal:7, and suppresses unauthorized workspace sessions.
	a.Model = stubModel{reply: `{"summary":"Discussed deployment","memories":[{"content":"Prefers canary rollouts","kind":"preference"}]}`}
	if err := a.Analyze(ctx, domain.Session{ID: "s-a", ConversationID: topicA.ID}); err != nil {
		t.Fatalf("Analyze owner workspace failed: %v", err)
	}
	found, err := ks.Search(ctx, "personal:7", "canary")
	if err != nil || len(found) != 1 {
		t.Fatalf("expected analyzed workspace memory in personal:7, got %+v %v", found, err)
	}
	// S2 regression: earlier participant session in topicA must NOT write to personal:7
	// even after the owner later posts in topicA (making conversation.OwnerID == 7),
	// including when replaying a cached analysis checkpoint.
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID:           "in-earlier-participant",
		Conversation: domain.Conversation{ID: topicA.ID, ChatID: topicA.ChatID, ThreadID: topicA.ThreadID, OwnerID: 99},
		Text:         "I prefer tabs over spaces",
		CreatedAt:    time.Now().UTC(),
	}, 10); err != nil {
		t.Fatal(err)
	}
	_ = repo.StartSession(ctx, domain.Session{ID: "s-earlier-participant", ConversationID: topicA.ID, Status: "active"})
	_ = repo.AppendMessage(ctx, "s-earlier-participant", "m1", domain.Message{Role: "user", Content: "I prefer tabs over spaces", AuthorID: 99, SourceMessageID: "in-earlier-participant"})
	// Owner later posts in topicA, overwriting conversation.OwnerID to 7.
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
		ID:           "in-owner-later",
		Conversation: topicA,
		Text:         "Owner follow-up",
		CreatedAt:    time.Now().UTC(),
	}, 11); err != nil {
		t.Fatal(err)
	}
	a.Model = stubModel{reply: `{"summary":"Tabs","memories":[{"content":"Participant prefers tabs","kind":"preference"}]}`}
	if err := a.Analyze(ctx, domain.Session{ID: "s-earlier-participant", ConversationID: topicA.ID}); err != nil {
		t.Fatalf("Analyze earlier participant workspace session failed: %v", err)
	}
	if poison, err := ks.Search(ctx, "personal:7", "Participant prefers tabs"); err != nil || len(poison) != 0 {
		t.Fatalf("earlier participant session leaked into personal:7 after owner posted: %+v", poison)
	}
	// Replaying a cached analysis checkpoint for a non-owner session must also never write to personal:7.
	_ = repo.StartSession(ctx, domain.Session{ID: "s-replay-participant", ConversationID: topicA.ID, Status: "active"})
	_ = repo.AppendMessage(ctx, "s-replay-participant", "m1", domain.Message{Role: "user", Content: "Replay poison", AuthorID: 99, SourceMessageID: "in-earlier-participant"})
	cachedPayload := `{"summary":"Cached","memories":[{"content":"Replayed poison preference","kind":"preference"}],"_suppress_memory_writes":false}`
	if err := repo.SaveCheckpoint(ctx, "analysis/s-replay-participant", json.RawMessage(cachedPayload)); err != nil {
		t.Fatal(err)
	}
	if err := a.Analyze(ctx, domain.Session{ID: "s-replay-participant", ConversationID: topicA.ID}); err != nil {
		t.Fatalf("Analyze checkpoint replay failed: %v", err)
	}
	if poison, err := ks.Search(ctx, "personal:7", "Replayed poison"); err != nil || len(poison) != 0 {
		t.Fatalf("replayed non-owner analysis checkpoint leaked into personal:7: %+v", poison)
	}
}

type stubModel struct {
	reply string
}

func (m stubModel) Complete(_ context.Context, _ []domain.Message, _ []domain.Tool) (domain.Completion, error) {
	return domain.Completion{Message: domain.Message{Role: "assistant", Content: m.reply}}, nil
}

type captureModel struct {
	fn func([]domain.Message, []domain.Tool)
}

func (m captureModel) Complete(_ context.Context, msgs []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	if m.fn != nil {
		m.fn(msgs, tools)
	}
	return domain.Completion{Message: domain.Message{Role: "assistant", Content: "ok"}}, nil
}

func TestWorkspaceToolsAndCodingUnlock(t *testing.T) {
	ctx := context.Background()
	a, repo, _, transport := newWorkspaceTestActivities(t)

	// 1. CodingContext unlocks topic_create, topic_update, workspace_post while keeping send_message/send_group_message blocked.
	codingCtx, err := a.CodingContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"topic_create", "topic_update", "workspace_post"} {
		if !hasTool(codingCtx.Tools, want) {
			t.Fatalf("CodingContext missing unlocked workspace tool %q", want)
		}
	}
	for _, forbid := range []string{"send_message", "send_rich_message", "send_group_message", "send_widget"} {
		if hasTool(codingCtx.Tools, forbid) {
			t.Fatalf("CodingContext should still exclude %q", forbid)
		}
	}

	// 2. Execute topic_create via ExecuteCodingTool (ACP path).
	res, err := a.ExecuteCodingTool(ctx, CodingToolRequest{
		SessionID: "acp-1",
		TurnID:    "t1",
		CallID:    "call-create",
		Name:      "topic_create",
		Arguments: json.RawMessage(`{"title":"PR Review","icon_color":7322096}`),
	})
	if err != nil || res.Failed {
		t.Fatalf("coding topic_create failed: err=%v res=%s", err, res.Output)
	}
	var created struct {
		ConversationID string `json:"conversation_id"`
		ChatID         int64  `json:"chat_id"`
		ThreadID       int    `json:"thread_id"`
		Title          string `json:"title"`
	}
	if err := json.Unmarshal(res.Output, &created); err != nil {
		t.Fatal(err)
	}
	if created.ConversationID != "telegram:group:-100555:topic:1" || created.ChatID != -100555 || created.ThreadID != 1 || created.Title != "PR Review" {
		t.Fatalf("unexpected created topic: %+v", created)
	}
	saved, ok, err := repo.Conversation(ctx, created.ConversationID)
	if err != nil || !ok || saved.Title != "PR Review" || saved.OwnerID != 7 {
		t.Fatalf("topic conversation not persisted: %+v ok=%v err=%v", saved, ok, err)
	}

	// 3. Execute topic_update (rename + close, then reopen) from a background task.
	ownerPrivate := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}
	out, err := a.ToolCall(ctx, ToolInput{
		RunKey:       "task-upd-1",
		Step:         1,
		TaskID:       "task-1",
		Call:         domain.ToolCall{ID: "u1", Name: "topic_update", Arguments: json.RawMessage(`{"conversation_id":"telegram:group:-100555:topic:1","title":"✅ PR Review","status":"closed"}`)},
		Conversation: ownerPrivate,
		SessionID:    "s-task",
	})
	if err != nil || ToolFailed(out) {
		t.Fatalf("topic_update close failed: %v %s", err, out)
	}
	saved, _, _ = repo.Conversation(ctx, created.ConversationID)
	if saved.Title != "✅ PR Review" {
		t.Fatalf("expected persisted title update, got %q", saved.Title)
	}
	res, err = a.ExecuteCodingTool(ctx, CodingToolRequest{
		SessionID: "acp-1",
		TurnID:    "t2",
		CallID:    "call-reopen",
		Name:      "topic_update",
		Arguments: json.RawMessage(`{"conversation_id":"telegram:group:-100555:topic:1","status":"open"}`),
	})
	if err != nil || res.Failed {
		t.Fatalf("coding topic_update reopen failed: %v %s", err, res.Output)
	}
	if len(transport.closedTopics) != 1 || transport.closedTopics[0] != 1 || len(transport.reopenedTopics) != 1 || transport.reopenedTopics[0] != 1 {
		t.Fatalf("expected close + reopen calls, got closed=%v reopened=%v", transport.closedTopics, transport.reopenedTopics)
	}

	// 4. workspace_post from ACP: to created topic with buttons, to default #Control, and to owner DM.
	res, err = a.ExecuteCodingTool(ctx, CodingToolRequest{
		SessionID: "acp-1",
		TurnID:    "t3",
		CallID:    "call-post-topic",
		Name:      "workspace_post",
		Arguments: json.RawMessage(`{"conversation_id":"telegram:group:-100555:topic:1","text":"**Build green!** Ready to merge?","buttons":[{"text":"Approve","callback_data":"t3:approve:1"},{"text":"Reject","callback_data":"t3:reject:1"}]}`),
	})
	if err != nil || res.Failed {
		t.Fatalf("workspace_post to topic failed: %v %s", err, res.Output)
	}
	// Default target (omitted conversation_id) posts to #Control (topic:0).
	res, err = a.ExecuteCodingTool(ctx, CodingToolRequest{
		SessionID: "acp-1",
		TurnID:    "t4",
		CallID:    "call-post-control",
		Name:      "workspace_post",
		Arguments: json.RawMessage(`{"text":"Control board update"}`),
	})
	if err != nil || res.Failed {
		t.Fatalf("workspace_post to #Control failed: %v %s", err, res.Output)
	}
	// Owner DM target.
	res, err = a.ExecuteCodingTool(ctx, CodingToolRequest{
		SessionID: "acp-1",
		TurnID:    "t5",
		CallID:    "call-post-dm",
		Name:      "workspace_post",
		Arguments: json.RawMessage(`{"conversation_id":"personal:7","text":"Direct ping"}`),
	})
	if err != nil || res.Failed {
		t.Fatalf("workspace_post to owner DM failed: %v %s", err, res.Output)
	}
	// External group and other user's DM must be rejected.
	_ = repo.SaveConversation(ctx, domain.Conversation{ID: "telegram:group:-100888:topic:0", ChatID: -100888, OwnerID: 7})
	for _, target := range []string{"telegram:group:-100888:topic:0", "personal:8"} {
		res, err = a.ExecuteCodingTool(ctx, CodingToolRequest{
			SessionID: "acp-1",
			TurnID:    "t6",
			CallID:    "call-bad-" + target,
			Name:      "workspace_post",
			Arguments: json.RawMessage(fmt.Sprintf(`{"conversation_id":%q,"text":"leak"}`, target)),
		})
		if err != nil || !res.Failed {
			t.Fatalf("workspace_post to %q should be rejected, got err=%v out=%s", target, err, res.Output)
		}
	}

	if len(transport.sentMessages) != 3 {
		t.Fatalf("expected 3 sent messages, got %d", len(transport.sentMessages))
	}
	if transport.sentMessages[0].conv.ThreadID != 1 || transport.sentMessages[0].markup == nil || len(transport.sentMessages[0].markup.InlineKeyboard) != 1 || len(transport.sentMessages[0].markup.InlineKeyboard[0]) != 2 {
		t.Fatalf("unexpected topic post: %+v", transport.sentMessages[0])
	}
	if transport.sentMessages[1].conv.ChatID != -100555 || transport.sentMessages[1].conv.ThreadID != 0 {
		t.Fatalf("expected #Control target, got: %+v", transport.sentMessages[1].conv)
	}
	if !transport.sentMessages[2].conv.Private || transport.sentMessages[2].conv.ChatID != 7 {
		t.Fatalf("expected owner DM target, got: %+v", transport.sentMessages[2].conv)
	}
}

func TestWorkspaceControllerPrefixAndUpsertPinnedStatus(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMemory()
	transport := &fakeWorkspaceTransport{}
	ctrl := &WorkspaceController{
		Repo:             repo,
		Transport:        transport,
		OwnerUserID:      7,
		WorkspaceGroupID: -100555,
	}

	// 1. UpdateTopicPrefix
	name, err := ctrl.UpdateTopicPrefix(ctx, 12, "⏳", "Refactor Auth")
	if err != nil || name != "⏳ Refactor Auth" {
		t.Fatalf("UpdateTopicPrefix failed: %q %v", name, err)
	}
	// Idempotent when already prefixed
	name, err = ctrl.UpdateTopicPrefix(ctx, 12, "⏳", "⏳ Refactor Auth")
	if err != nil || name != "⏳ Refactor Auth" {
		t.Fatalf("UpdateTopicPrefix double-prefixed: %q %v", name, err)
	}
	saved, ok, err := repo.Conversation(ctx, "telegram:group:-100555:topic:12")
	if err != nil || !ok || saved.Title != "⏳ Refactor Auth" {
		t.Fatalf("expected updated conversation persisted: %+v ok=%v err=%v", saved, ok, err)
	}

	// 2. UpsertPinnedStatus: first call sends + pins silently
	id1, err := ctrl.UpsertPinnedStatus(ctx, "Status: 1 active")
	if err != nil || id1 != 1 {
		t.Fatalf("first UpsertPinnedStatus failed: id=%d err=%v", id1, err)
	}
	if len(transport.sentMessages) != 1 || len(transport.pinnedMessages) != 1 || !transport.pinnedMessages[0].disable {
		t.Fatalf("expected 1 send + 1 silent pin, got sends=%d pins=%+v", len(transport.sentMessages), transport.pinnedMessages)
	}

	// 3. Second call edits in place (no new send, no extra pin)
	id2, err := ctrl.UpsertPinnedStatus(ctx, "Status: 2 active")
	if err != nil || id2 != id1 {
		t.Fatalf("second UpsertPinnedStatus failed: id=%d err=%v", id2, err)
	}
	if len(transport.sentMessages) != 1 || len(transport.editedMessages) != 1 || transport.editedMessages[0].text != "Status: 2 active" {
		t.Fatalf("expected in-place edit, got sends=%d edits=%+v", len(transport.sentMessages), transport.editedMessages)
	}

	// 4. Transient edit error does NOT create a duplicate message
	transport.editErr = errors.New("temporary network glitch")
	if _, err := ctrl.UpsertPinnedStatus(ctx, "Status: 3 active"); err == nil {
		t.Fatal("expected transient error to surface")
	}
	if len(transport.sentMessages) != 1 {
		t.Fatalf("transient error must not send a duplicate message, got %d sends", len(transport.sentMessages))
	}

	// 5. Deleted pinned message (MessageMissing) recreates and pins a fresh message
	transport.editErr = fakeMissingError{}
	id3, err := ctrl.UpsertPinnedStatus(ctx, "Status: recreated")
	if err != nil || id3 != 2 {
		t.Fatalf("expected fresh message after missing error, got id=%d err=%v", id3, err)
	}
	if len(transport.sentMessages) != 2 || len(transport.pinnedMessages) != 2 {
		t.Fatalf("expected recreated send + pin, got sends=%d pins=%d", len(transport.sentMessages), len(transport.pinnedMessages))
	}

	// 6. Failed pin saves message_id so next UpsertPinnedStatus edits + retries pin without duplicating send
	ctrl2 := &WorkspaceController{Repo: storage.NewMemory(), Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555}
	transport.pinErr = errors.New("pin failed temporarily")
	id4, err := ctrl2.UpsertPinnedStatus(ctx, "First try")
	if err == nil || id4 != 3 {
		t.Fatalf("expected pin error with saved message ID 3, got id=%d err=%v", id4, err)
	}
	id5, err := ctrl2.UpsertPinnedStatus(ctx, "Second try")
	if err != nil || id5 != id4 {
		t.Fatalf("expected retry to reuse message ID %d and pin, got id=%d err=%v", id4, id5, err)
	}
	if len(transport.sentMessages) != 3 {
		t.Fatalf("failed pin retry must not duplicate sent message, got %d sends", len(transport.sentMessages))
	}
}

func TestWorkspaceRegressionsTaskAuthTitlePreservationAndUnsetGate(t *testing.T) {
	ctx := context.Background()

	t.Run("S1 legacy unprivileged task does not inherit owner privileges after owner posts", func(t *testing.T) {
		a, repo, ks, _ := newWorkspaceTestActivities(t)
		topic := domain.Conversation{ID: "telegram:group:-100555:topic:15", Title: "Shared", ChatID: -100555, ThreadID: 15, OwnerID: 7}
		if err := ks.Update(ctx, domain.MemoryEntry{ID: "p-secret", Scope: "personal:7", Kind: "profile", Content: "Owner secret profile", UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveConversation(ctx, topic); err != nil {
			t.Fatal(err)
		}
		// Pre-existing task created without OwnerAuthorized (e.g. from before group became workspace).
		if _, err := repo.CreateTask(ctx, domain.Task{
			ID: "legacy-task", ConversationID: topic.ID, Title: "Old task", Goal: "Read logs", Status: "queued",
			NextRunAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		turn, err := a.Prepare(ctx, RunInput{Key: "k-legacy", SessionID: "s-legacy", ConversationID: topic.ID, TaskID: "legacy-task"})
		if err != nil {
			t.Fatal(err)
		}
		if turn.Conversation.OwnerID != 0 {
			t.Fatalf("legacy workspace task inherited OwnerID=%d", turn.Conversation.OwnerID)
		}
		if strings.Contains(turn.Messages[0].Content, "Owner secret profile") {
			t.Fatal("legacy workspace task leaked personal:7 USER profile")
		}
		tools := a.toolsForTurn(turn)
		for _, forbid := range []string{"logs_search", "backup_status", "computer_exec", "workspace_post", "topic_create"} {
			if hasTool(tools, forbid) {
				t.Fatalf("legacy workspace task advertised owner tool %q", forbid)
			}
		}
		// Direct ToolCall with forged OwnerID=7 must still check task.OwnerAuthorized.
		out, err := a.ToolCall(ctx, ToolInput{
			RunKey: "k-forged", Step: 1, TaskID: "legacy-task", Conversation: topic, SessionID: "s-legacy",
			Call: domain.ToolCall{ID: "c1", Name: "logs_search", Arguments: json.RawMessage(`{}`)},
		})
		if err != nil || !ToolFailed(out) {
			t.Fatalf("forged legacy workspace task bypassed OwnerAuthorized check: %v %s", err, out)
		}
		// Resumed pre-upgrade ModelCall carrying OwnerID=7 must neither advertise owner tools
		// nor append host backup status to the prompt.
		var capturedMsgs []domain.Message
		var capturedTools []domain.Tool
		a.Model = captureModel{fn: func(msgs []domain.Message, tools []domain.Tool) {
			capturedMsgs = msgs
			capturedTools = tools
		}}
		if _, err := a.ModelCall(ctx, ModelInput{
			Turn: Turn{
				Input:        RunInput{Key: "k-resumed-legacy", SessionID: "s-legacy", ConversationID: topic.ID, TaskID: "legacy-task"},
				Conversation: topic, // Pre-upgrade Turn carrying latest sender OwnerID=7
				Messages:     []domain.Message{{Role: "system", Content: "base"}, {Role: "user", Content: "continue"}},
			},
			Step: 1,
		}); err != nil {
			t.Fatalf("ModelCall on resumed legacy task failed: %v", err)
		}
		for _, forbid := range []string{"logs_search", "backup_status", "computer_exec", "workspace_post", "topic_create"} {
			if hasTool(capturedTools, forbid) {
				t.Fatalf("resumed legacy ModelCall advertised owner tool %q", forbid)
			}
		}
		if len(capturedMsgs) > 0 && strings.Contains(strings.ToLower(capturedMsgs[0].Content), "backup") {
			t.Fatalf("resumed legacy ModelCall leaked backup status context: %s", capturedMsgs[0].Content)
		}

		// Owner-created task in workspace persists OwnerAuthorized=true and retains owner privileges.
		createOut, err := a.ToolCall(ctx, ToolInput{
			RunKey: "k-create-owner-task", Step: 1, Conversation: topic, SessionID: "s-owner",
			Call: domain.ToolCall{ID: "c-create", Name: "task_create", Arguments: json.RawMessage(`{"title":"Owner task","goal":"Check logs"}`)},
		})
		if err != nil || ToolFailed(createOut) {
			t.Fatalf("owner task_create failed: %v %s", err, createOut)
		}
		var created domain.Task
		if err := json.Unmarshal([]byte(createOut), &created); err != nil {
			t.Fatal(err)
		}
		if !created.OwnerAuthorized {
			t.Fatal("owner-created task missing OwnerAuthorized=true")
		}
		ownerTaskTurn, err := a.Prepare(ctx, RunInput{Key: "k-owner-task", SessionID: "s-owner-task", ConversationID: topic.ID, TaskID: created.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !a.isOwnerWorkspace(ownerTaskTurn.Conversation, ownerTaskTurn.Conversation.OwnerID) {
			t.Fatal("owner-authorized workspace task lost owner privileges on Prepare")
		}
		if !hasTool(a.toolsForTurn(ownerTaskTurn), "logs_search") {
			t.Fatal("owner-authorized workspace task missing logs_search")
		}
	})

	t.Run("S4 ordinary topic message preserves renamed topic title", func(t *testing.T) {
		a, repo, _, _ := newWorkspaceTestActivities(t)
		ownerDM := domain.Conversation{ID: "personal:7", ChatID: 7, OwnerID: 7, Private: true}
		createOut, err := a.ToolCall(ctx, ToolInput{
			RunKey: "r-create", Step: 1, Conversation: ownerDM, SessionID: "s1",
			Call: domain.ToolCall{ID: "c1", Name: "topic_create", Arguments: json.RawMessage(`{"title":"Sprint Alpha"}`)},
		})
		if err != nil || ToolFailed(createOut) {
			t.Fatalf("topic_create failed: %v %s", err, createOut)
		}
		updOut, err := a.ToolCall(ctx, ToolInput{
			RunKey: "r-rename", Step: 1, Conversation: ownerDM, SessionID: "s1",
			Call: domain.ToolCall{ID: "c2", Name: "topic_update", Arguments: json.RawMessage(`{"conversation_id":"telegram:group:-100555:topic:1","title":"🚀 Sprint Alpha"}`)},
		})
		if err != nil || ToolFailed(updOut) {
			t.Fatalf("topic_update failed: %v %s", err, updOut)
		}
		// Ingest an ordinary Telegram message carrying the supergroup title.
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
			ID: "in-topic-msg",
			Conversation: domain.Conversation{
				ID:       "telegram:group:-100555:topic:1",
				Title:    "Owner Workspace Supergroup",
				ChatID:   -100555,
				ThreadID: 1,
				OwnerID:  7,
			},
			Text:      "hello topic",
			CreatedAt: time.Now().UTC(),
		}, 50); err != nil {
			t.Fatal(err)
		}
		conv, ok, err := repo.Conversation(ctx, "telegram:group:-100555:topic:1")
		if err != nil || !ok || conv.Title != "🚀 Sprint Alpha" {
			t.Fatalf("topic title was overwritten by supergroup title: got %+v ok=%v err=%v", conv, ok, err)
		}
	})

	t.Run("S5 unset TELEGRAM_WORKSPACE_GROUP_ID hides and blocks workspace_post in coding and chat", func(t *testing.T) {
		a, _, _, transport := newWorkspaceTestActivities(t)
		a.WorkspaceGroupID = 0
		codingCtx, err := a.CodingContext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"workspace_post", "topic_create", "topic_update"} {
			if hasTool(codingCtx.Tools, name) {
				t.Fatalf("unset workspace must not expose %q in CodingContext", name)
			}
		}
		res, err := a.ExecuteCodingTool(ctx, CodingToolRequest{
			SessionID: "acp-unset", TurnID: "t1", CallID: "c1", Name: "workspace_post",
			Arguments: json.RawMessage(`{"text":"should not send to DM"}`),
		})
		if err != nil || !res.Failed {
			t.Fatalf("workspace_post must fail when workspace is unset: err=%v out=%s", err, res.Output)
		}
		if len(transport.sentMessages) != 0 {
			t.Fatalf("workspace_post must not send any message when workspace is unset: %+v", transport.sentMessages)
		}
	})
}

func TestWorkspacePostAutoRecreatesDeletedForumTopic(t *testing.T) {
	ctx := context.Background()
	repo := storage.NewMemory()
	transport := &fakeWorkspaceTransport{
		nextThreadID:  100,
		deletedTopics: map[int]bool{8: true},
	}
	if err := repo.SaveConversation(ctx, domain.Conversation{
		ID:       "telegram:group:-100555:topic:8",
		Title:    "🔄 Polka · баг #64",
		ChatID:   -100555,
		ThreadID: 8,
		OwnerID:  7,
	}); err != nil {
		t.Fatalf("save old topic: %v", err)
	}
	act := &Activities{
		Repo:             repo,
		Workspace:        transport,
		OwnerUserID:      7,
		WorkspaceGroupID: -100555,
	}
	raw, err := act.executeWorkspaceTool(ctx, ToolInput{
		Conversation: domain.Conversation{ID: "telegram:group:-100555:topic:0", ChatID: -100555, OwnerID: 7},
		Call: domain.ToolCall{
			Name:      "workspace_post",
			Arguments: json.RawMessage(`{"conversation_id":"telegram:group:-100555:topic:8","text":"Стартовый бриф"}`),
		},
	})
	if err != nil {
		t.Fatalf("workspace_post should recreate deleted topic: %v", err)
	}
	res := raw.(map[string]any)
	if res["recreated"] != true || res["conversation_id"] != "telegram:group:-100555:topic:101" {
		t.Fatalf("unexpected workspace_post result: %+v", res)
	}
	if len(transport.createdTopics) != 1 || transport.createdTopics[0].Name != "🔄 Polka · баг #64" {
		t.Fatalf("expected recreated topic with previous title, got %+v", transport.createdTopics)
	}
	if len(transport.sentMessages) != 1 || transport.sentMessages[0].conv.ThreadID != 101 {
		t.Fatalf("expected message delivered to recreated topic 101, got %+v", transport.sentMessages)
	}
}
