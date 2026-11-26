package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/messagefmt"
	"openalbert/apps/backend/internal/t3"
)

// T3CallbackPrefix routes Telegram inline keyboard callbacks created by
// T3WorkspaceBridge through integrations.CallbackRouter.
const T3CallbackPrefix = "t3:"

var t3TopicStatusPrefixes = []string{"🔄", "❓", "👀", "✅", "❌"}

const (
	t3BoardThreadLimit       = 20
	t3BoardMaxUTF16Units     = 3900
	t3CallbackMaxDispatch    = 6 * time.Second
	t3AnswerCheckpointBase   = "t3/cb/a/"
	t3ControlCheckpointBase  = "t3/cb/c/"
	t3CommandCheckpointBase  = "t3/cb/cmd/"
	t3LaunchedCheckpointBase = "t3/launched/"
)

// T3WorkspaceBridgeConfig configures the zero-LLM bridge between the T3 Control
// Plane (EventObserver) and the Telegram Owner Workspace Supergroup.
type T3WorkspaceBridgeConfig struct {
	ControlPlane     *t3.ControlPlane
	Repo             domain.Repository
	Transport        domain.WorkspaceTransport
	OwnerUserID      int64
	WorkspaceGroupID int64
	ControlThreadID  int // Defaults to 0 (#Control / General topic).
	Logger           *slog.Logger
}

// T3WorkspaceBridge observes T3 Control Plane events to update workspace forum
// topic prefixes, maintain the pinned #Control status board, post interactive
// inline buttons, and handle owner button callbacks directly in Go with zero LLM turns.
type T3WorkspaceBridge struct {
	cp               *t3.ControlPlane
	repo             domain.Repository
	transport        domain.WorkspaceTransport
	controller       *WorkspaceController
	ownerUserID      int64
	workspaceGroupID int64
	logger           *slog.Logger

	topicMu           sync.Mutex
	lastTopicTitle    map[int]string
	recreatedTopicIDs map[string]int // old conversation_id -> newly recreated forum thread_id

	boardMu       sync.Mutex
	lastBoardText string

	cbMu                   sync.Mutex
	answerRecords          map[string]*t3AnswerCallbackRecord
	controlRecords         map[string]*t3ControlCallbackRecord
	postedPRMessages       map[string]bool
	postedLaunchedMessages map[string]bool
	pendingByCommand       map[string]string // command_id -> answer callback token
	inFlightTokens         map[string]bool   // token currently waiting inside DispatchAndWait
	tokenLocks             map[string]*sync.Mutex
}

type t3QuestionOption struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type t3ParsedQuestion struct {
	Key         string             `json:"key"`
	Text        string             `json:"text"`
	Options     []t3QuestionOption `json:"options"`
	MultiSelect bool               `json:"multi_select,omitempty"`
}

type t3AnswerCallbackRecord struct {
	Token            string             `json:"token"`
	ThreadID         string             `json:"thread_id"`
	EnvironmentID    string             `json:"environment_id"`
	RequestID        string             `json:"request_id"`
	Title            string             `json:"title,omitempty"`
	Questions        []t3ParsedQuestion `json:"questions"`
	CurrentIndex     int                `json:"current_index,omitempty"`
	Answers          map[string]any     `json:"answers,omitempty"`
	AnswerLabels     map[string]string  `json:"answer_labels,omitempty"`
	MultiSelected    map[int][]int      `json:"multi_selected,omitempty"`
	ChatID           int64              `json:"chat_id"`
	ThreadTopicID    int                `json:"thread_topic_id"`
	MessageID        int64              `json:"message_id"`
	Attempt          int                `json:"attempt,omitempty"`
	PendingCommandID string             `json:"pending_command_id,omitempty"`
	PendingSummary   string             `json:"pending_summary,omitempty"`
	NeedsRefresh     bool               `json:"needs_refresh,omitempty"`
	Answered         bool               `json:"answered,omitempty"`
	AnsweredText     string             `json:"answered_text,omitempty"`
	FinalEdited      bool               `json:"final_edited,omitempty"`
}

type t3ControlCallbackRecord struct {
	Token         string `json:"token"`
	ThreadID      string `json:"thread_id"`
	EnvironmentID string `json:"environment_id"`
	Title         string `json:"title,omitempty"`
	PRURL         string `json:"pr_url,omitempty"`
	ChatID        int64  `json:"chat_id"`
	ThreadTopicID int    `json:"thread_topic_id"`
}

var (
	_ t3.EventObserver        = (*T3WorkspaceBridge)(nil)
	_ t3.HandledEventObserver = (*T3WorkspaceBridge)(nil)
	_ domain.CallbackHandler  = (*T3WorkspaceBridge)(nil)
)

// NewT3WorkspaceBridge constructs a T3WorkspaceBridge wired to a WorkspaceController.
func NewT3WorkspaceBridge(cfg T3WorkspaceBridgeConfig) *T3WorkspaceBridge {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctrl := &WorkspaceController{
		Repo:             cfg.Repo,
		Transport:        cfg.Transport,
		OwnerUserID:      cfg.OwnerUserID,
		WorkspaceGroupID: cfg.WorkspaceGroupID,
		ControlThreadID:  cfg.ControlThreadID,
	}
	return &T3WorkspaceBridge{
		cp:                     cfg.ControlPlane,
		repo:                   cfg.Repo,
		transport:              cfg.Transport,
		controller:             ctrl,
		ownerUserID:            cfg.OwnerUserID,
		workspaceGroupID:       cfg.WorkspaceGroupID,
		logger:                 logger,
		lastTopicTitle:         map[int]string{},
		recreatedTopicIDs:      map[string]int{},
		answerRecords:          map[string]*t3AnswerCallbackRecord{},
		controlRecords:         map[string]*t3ControlCallbackRecord{},
		postedPRMessages:       map[string]bool{},
		postedLaunchedMessages: map[string]bool{},
		pendingByCommand:       map[string]string{},
		inFlightTokens:         map[string]bool{},
		tokenLocks:             map[string]*sync.Mutex{},
	}
}

func (b *T3WorkspaceBridge) enabled() bool {
	return b != nil && b.cp != nil && b.transport != nil && b.workspaceGroupID < 0 && b.ownerUserID > 0
}

// OnHostUpdated refreshes the pinned #Control status board whenever a T3 host
// registers, reconnects, or heartbeats.
func (b *T3WorkspaceBridge) OnHostUpdated(ctx context.Context, _ domain.T3Host) error {
	if !b.enabled() {
		return nil
	}
	return b.refreshStatusBoard(ctx)
}

// OnThreadEvent satisfies t3.EventObserver by delegating to OnThreadEventHandled.
func (b *T3WorkspaceBridge) OnThreadEvent(ctx context.Context, event domain.T3Event) error {
	_, err := b.OnThreadEventHandled(ctx, event)
	return err
}

// OnThreadEventHandled updates the workspace forum topic title prefix, posts
// zero-LLM inline buttons for structured pending questions or completed PRs,
// and refreshes the pinned #Control status board. It returns handled=true when
// a thread_waiting_input event was delivered via interactive inline buttons so
// ControlPlane does not queue a redundant LLM wake turn.
func (b *T3WorkspaceBridge) OnThreadEventHandled(ctx context.Context, event domain.T3Event) (bool, error) {
	if !b.enabled() {
		return false, nil
	}

	// Finalize any queued Telegram answer button message once its command succeeds.
	if strings.HasPrefix(event.EventID, "cmd:") {
		b.finalizePendingCommand(ctx, strings.TrimPrefix(event.EventID, "cmd:"))
	}

	var errs []error
	handled := false

	// Check current persisted thread state so stale/out-of-order events cannot
	// overwrite a newer topic prefix or post obsolete buttons.
	current := true
	if th, err := b.cp.Thread(ctx, event.ThreadID); err == nil {
		current = eventMatchesCurrentThread(event, th)
	}

	if current {
		threadID, ok := b.workspaceTopicThreadID(event.ConversationID)
		createdNow := false
		if !ok && b.isWorkspaceControlConversation(event.ConversationID) && strings.TrimSpace(event.ThreadID) != "" {
			prefix := statusPrefixForEvent(event)
			newConvID, newTopicID, newlyCreated, createErr := b.ensureWorkspaceTopicForControlThread(ctx, event, prefix)
			if createErr != nil {
				errs = append(errs, createErr)
			} else if newTopicID > 0 {
				event.ConversationID = newConvID
				threadID = newTopicID
				ok = true
				createdNow = newlyCreated
			}
		}
		if ok {
			prefix := statusPrefixForEvent(event)
			recreated := false
			recreateOnce := func() bool {
				if recreated {
					return false
				}
				newConvID, newTopicID, recErr := b.recreateWorkspaceTopic(ctx, event, event.ConversationID, prefix)
				if recErr != nil {
					errs = append(errs, recErr)
					return false
				}
				event.ConversationID = newConvID
				threadID = newTopicID
				recreated = true
				return true
			}

			if prefix != "" && !createdNow {
				stillCurrent, err := b.syncTopicTitleForEvent(ctx, event, threadID, prefix)
				if err != nil && isThreadMissingError(err) && recreateOnce() {
					err = nil
				}
				if err != nil {
					errs = append(errs, err)
				}
				if !stillCurrent {
					current = false
				}
			}
			if current {
				switch event.Kind {
				case domain.T3EventThreadLaunched:
					err := b.postLaunchedCard(ctx, event, threadID)
					if err != nil && isThreadMissingError(err) && recreateOnce() {
						err = b.postLaunchedCard(ctx, event, threadID)
					}
					if err != nil {
						errs = append(errs, err)
					}
				case domain.T3EventThreadWaitingInput:
					if event.PendingRequest != nil {
						posted, err := b.postWaitingInputButtons(ctx, event, threadID)
						if !posted && err != nil && isThreadMissingError(err) && recreateOnce() {
							posted, err = b.postWaitingInputButtons(ctx, event, threadID)
						}
						if posted {
							handled = true
						}
						if err != nil {
							errs = append(errs, err)
						}
					}
				case domain.T3EventThreadCompleted:
					if strings.TrimSpace(event.PRURL) != "" {
						err := b.postCompletedPRButtons(ctx, event, threadID)
						if err != nil && isThreadMissingError(err) && recreateOnce() {
							err = b.postCompletedPRButtons(ctx, event, threadID)
						}
						if err != nil {
							errs = append(errs, err)
						}
					}
				}
			}
		}
	}

	if err := b.refreshStatusBoard(ctx); err != nil {
		errs = append(errs, err)
	}
	return handled, errors.Join(errs...)
}

func eventMatchesCurrentThread(event domain.T3Event, th domain.T3Thread) bool {
	st := strings.ToLower(strings.TrimSpace(th.Status))
	switch event.Kind {
	case domain.T3EventThreadWaitingInput:
		if th.PendingRequest == nil {
			return st == "waiting_input" && event.PendingRequest == nil
		}
		if event.PendingRequest != nil && strings.TrimSpace(event.PendingRequest.RequestID) != "" {
			return strings.TrimSpace(th.PendingRequest.RequestID) == strings.TrimSpace(event.PendingRequest.RequestID)
		}
		return true
	case domain.T3EventThreadCompleted:
		return st == "completed"
	case domain.T3EventThreadFailed:
		return isFailedThreadStatus(st)
	case domain.T3EventThreadLaunched:
		return th.PendingRequest == nil && (st == "" || st == "running" || st == "starting" || st == "queued")
	case domain.T3EventThreadProgress:
		if th.PendingRequest != nil {
			return false
		}
		evSt := strings.ToLower(strings.TrimSpace(event.Status))
		if evSt == "running" || evSt == "starting" || evSt == "queued" {
			return st == "" || st == "running" || st == "starting" || st == "queued"
		}
		return st == evSt
	default:
		return true
	}
}

func isFailedThreadStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "cancelled", "interrupted", "rolled_back", "error":
		return true
	default:
		return false
	}
}

func (b *T3WorkspaceBridge) workspaceTopicThreadID(conversationID string) (int, bool) {
	if b.workspaceGroupID >= 0 {
		return 0, false
	}
	prefix := fmt.Sprintf("telegram:group:%d:topic:", b.workspaceGroupID)
	if !strings.HasPrefix(conversationID, prefix) {
		return 0, false
	}
	raw := strings.TrimPrefix(conversationID, prefix)
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (b *T3WorkspaceBridge) isWorkspaceControlConversation(conversationID string) bool {
	if b.workspaceGroupID >= 0 {
		return false
	}
	return conversationID == fmt.Sprintf("telegram:group:%d:topic:0", b.workspaceGroupID)
}

// ensureWorkspaceTopicForControlThread creates a dedicated Telegram forum topic
// for a thread that was launched from the workspace #Control (topic:0) topic,
// only after the launch actually succeeded. Guarded by b.topicMu so concurrent
// events for the same thread never create duplicate topics.
func (b *T3WorkspaceBridge) ensureWorkspaceTopicForControlThread(ctx context.Context, event domain.T3Event, prefix string) (string, int, bool, error) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()

	baseTitle := stripT3StatusPrefix(event.Title)
	if b.cp != nil && event.ThreadID != "" {
		if th, err := b.cp.Thread(ctx, event.ThreadID); err == nil {
			if existingTopicID, ok := b.workspaceTopicThreadID(th.ConversationID); ok {
				return th.ConversationID, existingTopicID, false, nil
			}
			if baseTitle == "" {
				baseTitle = stripT3StatusPrefix(th.Title)
			}
		}
	}
	if baseTitle == "" {
		baseTitle = "Задача"
	}
	if prefix == "" {
		prefix = "🔄"
	}
	desiredTitle, err := formatTopicTitle(prefix, baseTitle)
	if err != nil {
		desiredTitle = "🔄 Задача"
	}

	topic, err := b.transport.CreateForumTopic(ctx, b.workspaceGroupID, desiredTitle, 0)
	if err != nil {
		return "", 0, false, err
	}
	name := strings.TrimSpace(topic.Name)
	if name == "" {
		name = desiredTitle
	}
	newConvID := fmt.Sprintf("telegram:group:%d:topic:%d", b.workspaceGroupID, topic.ThreadID)
	if b.repo != nil {
		conv := domain.Conversation{
			ID:       newConvID,
			Title:    name,
			ChatID:   b.workspaceGroupID,
			ThreadID: topic.ThreadID,
			OwnerID:  b.ownerUserID,
			Private:  false,
		}
		if err := b.repo.SaveConversation(ctx, conv); err != nil {
			return "", 0, false, err
		}
	}
	if b.cp != nil && event.ThreadID != "" {
		if _, err := b.cp.RebindThreadConversation(ctx, event.ThreadID, newConvID); err != nil {
			return "", 0, false, err
		}
	}
	b.lastTopicTitle[topic.ThreadID] = name
	return newConvID, topic.ThreadID, true, nil
}

func statusPrefixForEvent(event domain.T3Event) string {
	switch event.Kind {
	case domain.T3EventThreadLaunched:
		return "🔄"
	case domain.T3EventThreadProgress:
		switch strings.ToLower(strings.TrimSpace(event.Status)) {
		case "running", "starting", "queued":
			return "🔄"
		default:
			return ""
		}
	case domain.T3EventThreadWaitingInput:
		return "❓"
	case domain.T3EventThreadCompleted:
		if strings.TrimSpace(event.PRURL) != "" {
			return "👀"
		}
		return "✅"
	case domain.T3EventThreadFailed:
		return "❌"
	default:
		return ""
	}
}

func stripT3StatusPrefix(title string) string {
	s := strings.TrimSpace(title)
	for {
		matched := false
		for _, p := range t3TopicStatusPrefixes {
			if strings.HasPrefix(s, p) {
				s = strings.TrimSpace(strings.TrimPrefix(s, p))
				matched = true
				break
			}
		}
		if !matched {
			return s
		}
	}
}

func (b *T3WorkspaceBridge) syncTopicTitleForEvent(ctx context.Context, event domain.T3Event, threadID int, prefix string) (bool, error) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()

	if th, err := b.cp.Thread(ctx, event.ThreadID); err == nil {
		if !eventMatchesCurrentThread(event, th) {
			return false, nil
		}
	}
	return true, b.syncTopicTitleLocked(ctx, event.ConversationID, threadID, prefix, event.Title)
}

func (b *T3WorkspaceBridge) syncTopicTitleLocked(ctx context.Context, conversationID string, threadID int, prefix, fallbackTitle string) error {

	currentTitle := ""
	useConvTitle := false
	if b.repo != nil {
		conv, found, err := b.repo.Conversation(ctx, conversationID)
		if err != nil {
			return err
		}
		if found {
			if conv.Private || conv.ChatID != b.workspaceGroupID || conv.ThreadID != threadID {
				return nil
			}
			// Only treat conv.Title as a known forum topic name when it was not
			// populated purely as a supergroup chat-title fallback.
			if !conv.TitleFromChat {
				candidate := strings.TrimSpace(conv.Title)
				if candidate != "" && !b.isWorkspaceChatTitleFallback(ctx, candidate) {
					currentTitle = candidate
					useConvTitle = true
				}
			}
		}
	}
	if !useConvTitle {
		currentTitle = strings.TrimSpace(b.lastTopicTitle[threadID])
	}

	baseTitle := stripT3StatusPrefix(currentTitle)
	if baseTitle == "" {
		baseTitle = stripT3StatusPrefix(fallbackTitle)
	}
	if baseTitle == "" {
		return nil
	}

	desiredTitle, err := formatTopicTitle(prefix, baseTitle)
	if err != nil {
		return err
	}
	if currentTitle == desiredTitle {
		b.lastTopicTitle[threadID] = desiredTitle
		return nil
	}
	updated, err := b.controller.UpdateTopicPrefix(ctx, threadID, prefix, baseTitle)
	if err != nil {
		return err
	}
	b.lastTopicTitle[threadID] = updated
	return nil
}

func (b *T3WorkspaceBridge) recreateWorkspaceTopic(ctx context.Context, event domain.T3Event, oldConvID, prefix string) (string, int, error) {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()

	if existingTopicID, ok := b.recreatedTopicIDs[oldConvID]; ok && existingTopicID > 0 {
		newConvID := fmt.Sprintf("telegram:group:%d:topic:%d", b.workspaceGroupID, existingTopicID)
		if b.cp != nil && event.ThreadID != "" {
			_, _ = b.cp.RebindThreadConversation(ctx, event.ThreadID, newConvID)
		}
		return newConvID, existingTopicID, nil
	}

	baseTitle := ""
	if b.repo != nil && oldConvID != "" {
		if conv, found, err := b.repo.Conversation(ctx, oldConvID); err == nil && found && !conv.TitleFromChat {
			baseTitle = stripT3StatusPrefix(conv.Title)
		}
	}
	if baseTitle == "" {
		baseTitle = stripT3StatusPrefix(event.Title)
	}
	if baseTitle == "" {
		baseTitle = "Задача"
	}
	if prefix == "" {
		prefix = "🔄"
	}
	desiredTitle, err := formatTopicTitle(prefix, baseTitle)
	if err != nil {
		desiredTitle = "🔄 Задача"
	}

	topic, err := b.transport.CreateForumTopic(ctx, b.workspaceGroupID, desiredTitle, 0)
	if err != nil {
		return "", 0, err
	}
	name := strings.TrimSpace(topic.Name)
	if name == "" {
		name = desiredTitle
	}
	newConvID := fmt.Sprintf("telegram:group:%d:topic:%d", b.workspaceGroupID, topic.ThreadID)
	if b.repo != nil {
		conv := domain.Conversation{
			ID:       newConvID,
			Title:    name,
			ChatID:   b.workspaceGroupID,
			ThreadID: topic.ThreadID,
			OwnerID:  b.ownerUserID,
			Private:  false,
		}
		if err := b.repo.SaveConversation(ctx, conv); err != nil {
			return "", 0, err
		}
	}
	if b.cp != nil {
		if event.ThreadID != "" {
			_, _ = b.cp.RebindThreadConversation(ctx, event.ThreadID, newConvID)
		}
		if oldConvID != "" {
			if threads, listErr := b.cp.ListThreads(ctx, domain.T3ThreadFilter{ConversationID: oldConvID}); listErr == nil {
				for _, th := range threads {
					if th.ThreadID == event.ThreadID {
						continue
					}
					_, _ = b.cp.RebindThreadConversation(ctx, th.ThreadID, newConvID)
				}
			}
		}
	}
	b.lastTopicTitle[topic.ThreadID] = name
	if oldConvID != "" {
		b.recreatedTopicIDs[oldConvID] = topic.ThreadID
	}
	return newConvID, topic.ThreadID, nil
}

func (b *T3WorkspaceBridge) postLaunchedCard(ctx context.Context, event domain.T3Event, threadID int) error {
	dedupKey := strings.TrimSpace(event.ThreadID)
	if dedupKey == "" {
		return nil
	}
	b.cbMu.Lock()
	if b.postedLaunchedMessages[dedupKey] {
		b.cbMu.Unlock()
		return nil
	}
	b.cbMu.Unlock()

	cpKey := t3LaunchedCheckpointBase + shortCallbackToken("t3l", dedupKey)
	if b.repo != nil {
		if _, found, err := b.repo.LoadCheckpoint(ctx, cpKey); err == nil && found {
			b.cbMu.Lock()
			b.postedLaunchedMessages[dedupKey] = true
			b.cbMu.Unlock()
			return nil
		}
	}

	title := stripT3StatusPrefix(event.Title)
	if title == "" {
		title = dedupKey
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "🔄 Воркер запущен: %s", clipRunes(title, 120))
	if model := strings.TrimSpace(event.Model); model != "" {
		fmt.Fprintf(&msg, "\nМодель: %s", clipRunes(model, 80))
	}
	if branch := strings.TrimSpace(event.Branch); branch != "" {
		fmt.Fprintf(&msg, "\nВетка: %s", clipRunes(branch, 80))
	}
	if link := formatT3WebThreadLink(event.Link, event.EnvironmentID, event.ThreadID); link != "" {
		fmt.Fprintf(&msg, "\n%s", clipRunes(link, 240))
	}
	text := clipUTF16(msg.String(), messagefmt.TextLimit)
	conv := domain.Conversation{
		ID:       event.ConversationID,
		ChatID:   b.workspaceGroupID,
		ThreadID: threadID,
		OwnerID:  b.ownerUserID,
		Private:  false,
	}
	if _, err := b.transport.SendMessageWithButtons(ctx, conv, text, nil, nil); err != nil {
		return err
	}
	b.cbMu.Lock()
	b.postedLaunchedMessages[dedupKey] = true
	b.cbMu.Unlock()
	if b.repo != nil {
		_ = b.repo.SaveCheckpoint(ctx, cpKey, json.RawMessage(`true`))
	}
	return nil
}

// formatT3WebThreadLink converts internal t3-thread:// or markdown links into a
// plain clickable https://app.t3.codes/threads/... URL for Telegram messages.
func formatT3WebThreadLink(rawLink, envID, threadID string) string {
	s := strings.TrimSpace(rawLink)
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return s
	}
	if idx := strings.Index(s, "t3-thread://v2/"); idx >= 0 {
		rest := s[idx+len("t3-thread://v2/"):]
		if end := strings.IndexAny(rest, ")\r\n\t "); end >= 0 {
			rest = rest[:end]
		}
		if slash := strings.IndexByte(rest, '/'); slash > 0 && slash < len(rest)-1 {
			env := strings.TrimSpace(rest[:slash])
			tid := strings.TrimSpace(rest[slash+1:])
			if env != "" && tid != "" {
				return fmt.Sprintf("https://app.t3.codes/threads/%s/%s", url.PathEscape(env), url.PathEscape(tid))
			}
		}
	}
	if strings.TrimSpace(envID) != "" && strings.TrimSpace(threadID) != "" {
		return fmt.Sprintf("https://app.t3.codes/threads/%s/%s", url.PathEscape(strings.TrimSpace(envID)), url.PathEscape(strings.TrimSpace(threadID)))
	}
	return s
}

func (b *T3WorkspaceBridge) isWorkspaceChatTitleFallback(ctx context.Context, candidate string) bool {
	if b.repo == nil || b.workspaceGroupID >= 0 {
		return false
	}
	if stripT3StatusPrefix(candidate) != strings.TrimSpace(candidate) {
		return false
	}
	controlID := fmt.Sprintf("telegram:group:%d:topic:0", b.workspaceGroupID)
	ctrlConv, found, err := b.repo.Conversation(ctx, controlID)
	if err != nil || !found {
		return false
	}
	return strings.TrimSpace(ctrlConv.Title) != "" && strings.TrimSpace(ctrlConv.Title) == strings.TrimSpace(candidate)
}

func (b *T3WorkspaceBridge) refreshStatusBoard(ctx context.Context) error {
	if b.repo == nil {
		return nil
	}
	b.boardMu.Lock()
	defer b.boardMu.Unlock()

	hosts, err := b.cp.ListHosts(ctx)
	if err != nil {
		return err
	}
	threads, err := b.cp.ListThreads(ctx, domain.T3ThreadFilter{Limit: t3BoardThreadLimit})
	if err != nil {
		return err
	}
	text := renderT3ControlBoard(hosts, threads)
	if text == b.lastBoardText {
		return nil
	}
	if _, err := b.controller.UpsertPinnedStatus(ctx, text); err != nil {
		return err
	}
	b.lastBoardText = text
	return nil
}

func renderT3ControlBoard(hosts []domain.T3Host, threads []domain.T3Thread) string {
	sortedHosts := append([]domain.T3Host(nil), hosts...)
	sort.Slice(sortedHosts, func(i, j int) bool {
		if sortedHosts[i].Online != sortedHosts[j].Online {
			return sortedHosts[i].Online
		}
		li, lj := strings.ToLower(sortedHosts[i].Label), strings.ToLower(sortedHosts[j].Label)
		if li != lj {
			return li < lj
		}
		return sortedHosts[i].HostID < sortedHosts[j].HostID
	})

	var b strings.Builder
	b.WriteString("T3 Control Board\n\nHosts:")
	if len(sortedHosts) == 0 {
		b.WriteString("\n(none)")
	} else {
		for _, h := range sortedHosts {
			dot := "⚪"
			if h.Online {
				dot = "🟢"
			}
			label := strings.TrimSpace(h.Label)
			if label == "" {
				label = h.HostID
			}
			line := fmt.Sprintf("\n%s %s", dot, clipRunes(label, 60))
			if n := len(h.Projects); n == 1 {
				line += " (1 project)"
			} else if n > 1 {
				line += fmt.Sprintf(" (%d projects)", n)
			}
			b.WriteString(line)
		}
	}

	b.WriteString("\n\nThreads:")
	if len(threads) == 0 {
		b.WriteString("\n(none)")
	} else {
		for _, th := range threads {
			emoji := threadBoardEmoji(th)
			title := stripT3StatusPrefix(th.Title)
			if title == "" {
				title = th.ThreadID
			}
			line := fmt.Sprintf("\n%s %s", emoji, clipRunes(title, 80))
			if pr := strings.TrimSpace(th.PRURL); pr != "" {
				line += " — " + pr
			} else if br := strings.TrimSpace(th.Branch); br != "" {
				line += " [" + clipRunes(br, 40) + "]"
			}
			if utf16Len(b.String()+line) > t3BoardMaxUTF16Units {
				b.WriteString("\n…")
				break
			}
			b.WriteString(line)
		}
	}
	return clipUTF16(b.String(), t3BoardMaxUTF16Units)
}

func threadBoardEmoji(th domain.T3Thread) string {
	status := strings.ToLower(strings.TrimSpace(th.Status))
	if th.PendingRequest != nil && strings.TrimSpace(th.PendingRequest.RequestID) != "" && status != "completed" && !isFailedThreadStatus(status) && status != "settled" && status != "archived" {
		return "❓"
	}
	switch status {
	case "waiting_input", "waiting":
		return "❓"
	case "completed":
		if strings.TrimSpace(th.PRURL) != "" {
			return "👀"
		}
		return "✅"
	case "idle":
		if strings.TrimSpace(th.LastError) != "" {
			return "❌"
		}
		if strings.TrimSpace(th.PRURL) != "" {
			return "👀"
		}
		return "✅"
	case "settled":
		return "✅"
	case "archived":
		return "📦"
	case "failed", "cancelled", "interrupted", "rolled_back", "error":
		return "❌"
	default:
		return "🔄"
	}
}

func shortCallbackToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6]) // 12 lowercase hex chars
}

func parsePendingQuestions(req *domain.T3PendingRequest) ([]t3ParsedQuestion, bool) {
	if req == nil || strings.TrimSpace(req.RequestID) == "" || len(req.Questions) == 0 {
		return nil, false
	}
	parsed := make([]t3ParsedQuestion, 0, len(req.Questions))
	usedKeys := map[string]bool{}
	for idx, rawQ := range req.Questions {
		raw := bytes.TrimSpace(rawQ)
		if len(raw) == 0 || raw[0] != '{' {
			return nil, false
		}
		var q struct {
			ID             string            `json:"id"`
			Header         string            `json:"header"`
			Question       string            `json:"question"`
			Prompt         string            `json:"prompt"`
			Text           string            `json:"text"`
			Label          string            `json:"label"`
			MultiSelect    bool              `json:"multiSelect"`
			SnakeMulti     bool              `json:"multi_select"`
			IsMultiSelect  bool              `json:"is_multi_select"`
			Options        []json.RawMessage `json:"options"`
			Choices        []json.RawMessage `json:"choices"`
			SuggestedReply []json.RawMessage `json:"suggested_options"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, false
		}
		rawOpts := q.Options
		if len(rawOpts) == 0 {
			rawOpts = q.Choices
		}
		if len(rawOpts) == 0 {
			rawOpts = q.SuggestedReply
		}
		if len(rawOpts) == 0 {
			return nil, false
		}

		opts := make([]t3QuestionOption, 0, len(rawOpts))
		for _, ro := range rawOpts {
			trimmed := bytes.TrimSpace(ro)
			if len(trimmed) == 0 {
				continue
			}
			var strOpt string
			if json.Unmarshal(trimmed, &strOpt) == nil {
				strOpt = strings.TrimSpace(strOpt)
				if strOpt != "" {
					opts = append(opts, t3QuestionOption{Label: strOpt, Value: strOpt})
				}
				continue
			}
			var obj struct {
				ID          string `json:"id"`
				Label       string `json:"label"`
				Text        string `json:"text"`
				Title       string `json:"title"`
				Value       string `json:"value"`
				Description string `json:"description"`
			}
			if json.Unmarshal(trimmed, &obj) != nil {
				continue
			}
			label := firstNonEmptyString(obj.Label, obj.Text, obj.Title, obj.Value, obj.ID)
			if label == "" {
				continue
			}
			val := firstNonEmptyString(obj.Value, obj.Label, obj.Text, obj.Title, obj.ID)
			opts = append(opts, t3QuestionOption{
				Label:       label,
				Value:       val,
				Description: strings.TrimSpace(obj.Description),
			})
			if len(opts) >= 20 {
				break
			}
		}
		if len(opts) == 0 {
			return nil, false
		}
		text := firstNonEmptyString(q.Question, q.Prompt, q.Text, q.Header, q.Label, "Select an option:")
		key := firstNonEmptyString(q.ID, q.Question, q.Header, q.Prompt, q.Text, fmt.Sprintf("q%d", idx+1))
		if usedKeys[key] {
			key = fmt.Sprintf("%s_%d", key, idx+1)
		}
		usedKeys[key] = true
		parsed = append(parsed, t3ParsedQuestion{
			Key:         key,
			Text:        text,
			Options:     opts,
			MultiSelect: q.MultiSelect || q.SnakeMulti || q.IsMultiSelect,
		})
	}
	return parsed, len(parsed) > 0
}

func renderQuestionPrompt(rec *t3AnswerCallbackRecord) (string, *domain.InlineKeyboardMarkup) {
	var msg strings.Builder
	title := stripT3StatusPrefix(rec.Title)
	if title != "" {
		fmt.Fprintf(&msg, "❓ %s\n", clipRunes(title, 120))
	} else {
		msg.WriteString("❓ Input needed\n")
	}
	for i := 0; i < rec.CurrentIndex && i < len(rec.Questions); i++ {
		q := rec.Questions[i]
		lbl := answerDisplayValue(rec, q.Key)
		if lbl != "" {
			fmt.Fprintf(&msg, "\n✓ %s: %s", clipRunes(q.Text, 80), clipRunes(lbl, 80))
		}
	}

	qIdx := rec.CurrentIndex
	if qIdx < 0 || qIdx >= len(rec.Questions) {
		return clipUTF16(msg.String(), messagefmt.TextLimit), &domain.InlineKeyboardMarkup{InlineKeyboard: [][]domain.InlineButton{}}
	}
	q := rec.Questions[qIdx]
	if len(rec.Questions) > 1 {
		fmt.Fprintf(&msg, "\n\n(%d/%d) %s", qIdx+1, len(rec.Questions), q.Text)
	} else {
		fmt.Fprintf(&msg, "\n%s", q.Text)
	}
	if q.MultiSelect {
		msg.WriteString(" (select one or more, then tap Confirm)")
	}

	hasDesc := false
	for _, opt := range q.Options {
		if opt.Description != "" {
			hasDesc = true
			break
		}
	}
	if hasDesc {
		msg.WriteString("\n")
		for i, opt := range q.Options {
			if opt.Description != "" {
				fmt.Fprintf(&msg, "\n%d. %s — %s", i+1, opt.Label, opt.Description)
			} else {
				fmt.Fprintf(&msg, "\n%d. %s", i+1, opt.Label)
			}
		}
	}

	selectedSet := map[int]bool{}
	if q.MultiSelect && rec.MultiSelected != nil {
		for _, idx := range rec.MultiSelected[qIdx] {
			selectedSet[idx] = true
		}
	}

	perRow := 2
	for _, opt := range q.Options {
		if utf8.RuneCountInString(opt.Label) > 22 {
			perRow = 1
			break
		}
	}
	var rows [][]domain.InlineButton
	for i := 0; i < len(q.Options); i += perRow {
		end := min(i+perRow, len(q.Options))
		row := make([]domain.InlineButton, 0, end-i)
		for optIdx := i; optIdx < end; optIdx++ {
			label := q.Options[optIdx].Label
			if q.MultiSelect {
				if selectedSet[optIdx] {
					label = "✓ " + label
				} else {
					label = "○ " + label
				}
			}
			cbData := fmt.Sprintf("t3:a:%s:%d:%d", rec.Token, qIdx, optIdx)
			row = append(row, domain.InlineButton{
				Text:         clipRunes(label, 64),
				CallbackData: cbData,
			})
		}
		rows = append(rows, row)
	}
	if q.MultiSelect {
		rows = append(rows, []domain.InlineButton{{
			Text:         "✓ Confirm",
			CallbackData: fmt.Sprintf("t3:a:%s:%d:s", rec.Token, qIdx),
		}})
	}
	return clipUTF16(msg.String(), messagefmt.TextLimit-200), &domain.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func renderFinalAnsweredMessage(rec *t3AnswerCallbackRecord, summary string) string {
	var msg strings.Builder
	title := stripT3StatusPrefix(rec.Title)
	if title != "" {
		fmt.Fprintf(&msg, "❓ %s", clipRunes(title, 120))
	} else {
		msg.WriteString("❓ Input needed")
	}
	if len(rec.Questions) == 1 {
		fmt.Fprintf(&msg, "\n\n%s\n\n✓ Answered: %s", rec.Questions[0].Text, summary)
	} else {
		msg.WriteString("\n")
		for _, q := range rec.Questions {
			lbl := answerDisplayValue(rec, q.Key)
			fmt.Fprintf(&msg, "\n• %s: %s", clipRunes(q.Text, 80), lbl)
		}
		fmt.Fprintf(&msg, "\n\n✓ Answered: %s", summary)
	}
	return clipUTF16(msg.String(), messagefmt.TextLimit)
}

func answerDisplayValue(rec *t3AnswerCallbackRecord, key string) string {
	if lbl := rec.AnswerLabels[key]; lbl != "" {
		return lbl
	}
	switch v := rec.Answers[key].(type) {
	case string:
		return v
	case []string:
		return strings.Join(v, ", ")
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		return ""
	}
}

func (b *T3WorkspaceBridge) postWaitingInputButtons(ctx context.Context, event domain.T3Event, threadID int) (bool, error) {
	questions, ok := parsePendingQuestions(event.PendingRequest)
	if !ok {
		return false, nil
	}
	token := shortCallbackToken("t3a", event.ThreadID, event.PendingRequest.RequestID)
	unlock := b.lockToken("a:" + token)
	defer unlock()

	if existing, found, err := b.loadAnswerRecord(ctx, token); err == nil && found && existing.MessageID > 0 {
		return true, nil
	}

	rec := &t3AnswerCallbackRecord{
		Token:         token,
		ThreadID:      event.ThreadID,
		EnvironmentID: event.EnvironmentID,
		RequestID:     event.PendingRequest.RequestID,
		Title:         stripT3StatusPrefix(event.Title),
		Questions:     questions,
		CurrentIndex:  0,
		Answers:       map[string]any{},
		AnswerLabels:  map[string]string{},
		MultiSelected: map[int][]int{},
		ChatID:        b.workspaceGroupID,
		ThreadTopicID: threadID,
	}
	text, markup := renderQuestionPrompt(rec)

	// Save before sending so an immediate button tap can resolve the token.
	if err := b.saveAnswerRecord(ctx, rec); err != nil {
		return false, err
	}
	// Recheck right before sending in case completion arrived during the checkpoint write.
	if th, err := b.cp.Thread(ctx, event.ThreadID); err == nil && !eventMatchesCurrentThread(event, th) {
		return false, nil
	}

	conv := domain.Conversation{
		ID:       event.ConversationID,
		ChatID:   b.workspaceGroupID,
		ThreadID: threadID,
		OwnerID:  b.ownerUserID,
		Private:  false,
	}
	msgID, err := b.transport.SendMessageWithButtons(ctx, conv, text, nil, markup)
	if err != nil {
		return false, err
	}
	rec.MessageID = msgID
	if err := b.saveAnswerRecord(ctx, rec); err != nil {
		return true, err
	}
	return true, nil
}

func (b *T3WorkspaceBridge) postCompletedPRButtons(ctx context.Context, event domain.T3Event, threadID int) error {
	prURL := strings.TrimSpace(event.PRURL)
	if prURL == "" {
		return nil
	}
	dedupKey := event.EventID
	if dedupKey == "" {
		dedupKey = event.ThreadID + "\x00" + prURL
	}
	b.cbMu.Lock()
	if b.postedPRMessages[dedupKey] {
		b.cbMu.Unlock()
		return nil
	}
	b.cbMu.Unlock()

	token := shortCallbackToken("t3c", event.ThreadID)
	rec := &t3ControlCallbackRecord{
		Token:         token,
		ThreadID:      event.ThreadID,
		EnvironmentID: event.EnvironmentID,
		Title:         stripT3StatusPrefix(event.Title),
		PRURL:         prURL,
		ChatID:        b.workspaceGroupID,
		ThreadTopicID: threadID,
	}
	if err := b.saveControlRecord(ctx, rec); err != nil {
		return err
	}

	row := make([]domain.InlineButton, 0, 2)
	if isValidHTTPURL(prURL) {
		row = append(row, domain.InlineButton{
			Text: "🔗 Открыть PR",
			URL:  prURL,
		})
	}
	row = append(row, domain.InlineButton{
		Text:         "🔄 Обновить",
		CallbackData: fmt.Sprintf("t3:c:%s:sync", token),
	})
	markup := &domain.InlineKeyboardMarkup{InlineKeyboard: [][]domain.InlineButton{row}}

	title := rec.Title
	if title == "" {
		title = event.ThreadID
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "👀 %s\n%s", clipRunes(title, 120), prURL)
	if summary := strings.TrimSpace(event.Summary); summary != "" {
		fmt.Fprintf(&msg, "\n\n%s", clipRunes(summary, 500))
	}
	text := clipUTF16(msg.String(), messagefmt.TextLimit)

	conv := domain.Conversation{
		ID:       event.ConversationID,
		ChatID:   b.workspaceGroupID,
		ThreadID: threadID,
		OwnerID:  b.ownerUserID,
		Private:  false,
	}
	if _, err := b.transport.SendMessageWithButtons(ctx, conv, text, nil, markup); err != nil {
		return err
	}
	b.cbMu.Lock()
	b.postedPRMessages[dedupKey] = true
	b.cbMu.Unlock()
	return nil
}

// HandleCallback processes owner-authorized Telegram inline keyboard button
// presses for T3 waiting-input options and thread control actions with zero LLM calls.
func (b *T3WorkspaceBridge) HandleCallback(ctx context.Context, q domain.CallbackQuery) (domain.CallbackResult, error) {
	if !b.enabled() {
		return domain.CallbackResult{Text: "Action unavailable"}, errors.New("T3 workspace bridge is disabled")
	}
	if !b.authorizedCallback(q) {
		return domain.CallbackResult{Text: "Unauthorized"}, errors.New("callback query is not authorized")
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) < 4 || parts[0] != "t3" {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}
	switch parts[1] {
	case "a":
		if len(parts) == 4 {
			optIdx, err := strconv.Atoi(parts[3])
			if err != nil {
				return domain.CallbackResult{Text: "Expired or unknown action"}, nil
			}
			return b.handleAnswerCallback(ctx, q, parts[2], 0, optIdx, false)
		}
		if len(parts) == 5 {
			qIdx, err := strconv.Atoi(parts[3])
			if err != nil {
				return domain.CallbackResult{Text: "Expired or unknown action"}, nil
			}
			if parts[4] == "s" {
				return b.handleAnswerCallback(ctx, q, parts[2], qIdx, -1, true)
			}
			optIdx, err := strconv.Atoi(parts[4])
			if err != nil {
				return domain.CallbackResult{Text: "Expired or unknown action"}, nil
			}
			return b.handleAnswerCallback(ctx, q, parts[2], qIdx, optIdx, false)
		}
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	case "c":
		if len(parts) != 4 {
			return domain.CallbackResult{Text: "Expired or unknown action"}, nil
		}
		return b.handleControlCallback(ctx, q, parts[2], parts[3])
	default:
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}
}

func (b *T3WorkspaceBridge) authorizedCallback(q domain.CallbackQuery) bool {
	if b.ownerUserID <= 0 || q.UserID != b.ownerUserID || q.FromBot {
		return false
	}
	if q.ChatID == 0 {
		return true
	}
	if q.Private && q.ChatID == b.ownerUserID {
		return true
	}
	return b.workspaceGroupID < 0 && !q.Private && q.ChatID == b.workspaceGroupID
}

func (b *T3WorkspaceBridge) callbackDispatchTimeout() time.Duration {
	d := b.cp.DispatchTimeout()
	if d <= 0 || d > t3CallbackMaxDispatch {
		return t3CallbackMaxDispatch
	}
	return d
}

func (b *T3WorkspaceBridge) handleAnswerCallback(ctx context.Context, q domain.CallbackQuery, token string, qIdx, optIdx int, submitMulti bool) (domain.CallbackResult, error) {
	unlock := b.lockToken("a:" + token)
	defer unlock()

	rec, found, err := b.loadAnswerRecord(ctx, token)
	if err != nil {
		return domain.CallbackResult{Text: "Action failed"}, err
	}
	if !found || qIdx < 0 || qIdx >= len(rec.Questions) {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}
	if q.ChatID != 0 && rec.ChatID != 0 && q.ChatID != rec.ChatID {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}
	if rec.MessageID <= 0 && q.MessageID > 0 {
		rec.MessageID = q.MessageID
	}
	if rec.ChatID == 0 && q.ChatID != 0 {
		rec.ChatID = q.ChatID
	}
	if rec.Answered {
		if !rec.FinalEdited {
			if edited, editErr := b.editFinalAnsweredMessage(ctx, q, rec); edited && editErr == nil {
				rec.FinalEdited = true
				_ = b.saveAnswerRecord(ctx, rec)
			}
		}
		return domain.CallbackResult{Text: "Already answered: " + clipRunes(rec.AnsweredText, 120)}, nil
	}

	// Check if a previously queued command has already settled or is still in flight.
	if rec.PendingCommandID != "" {
		cmd, ok, cmdErr := b.cp.Command(ctx, rec.PendingCommandID)
		if cmdErr == nil && ok {
			switch cmd.Status {
			case domain.T3CommandStatusPending, domain.T3CommandStatusDispatched:
				return domain.CallbackResult{Text: "Answer already queued: " + clipRunes(rec.PendingSummary, 120)}, nil
			case domain.T3CommandStatusSucceeded:
				b.finalizeAnsweredRecord(ctx, q, rec, rec.PendingSummary, nil)
				return domain.CallbackResult{Text: "Already answered: " + clipRunes(rec.AnsweredText, 120)}, nil
			case domain.T3CommandStatusFailed:
				b.clearPendingCommandRef(rec.PendingCommandID)
				rec.PendingCommandID = ""
				rec.PendingSummary = ""
				rec.Attempt++
				_ = b.saveAnswerRecord(ctx, rec)
			}
		} else if cmdErr == nil && !ok {
			b.clearPendingCommandRef(rec.PendingCommandID)
			rec.PendingCommandID = ""
			rec.PendingSummary = ""
			rec.Attempt++
			_ = b.saveAnswerRecord(ctx, rec)
		}
	}

	// Verify the pending request is STILL active on the thread (covers owner
	// answering directly in T3 UI, thread resuming, or thread finishing).
	th, threadErr := b.cp.Thread(ctx, rec.ThreadID)
	if errors.Is(threadErr, t3.ErrThreadNotFound) {
		rec.Answered = true
		rec.AnsweredText = "no longer pending"
		_ = b.saveAnswerRecord(ctx, rec)
		b.clearQuestionKeyboard(ctx, q, rec, "Question is no longer pending")
		return domain.CallbackResult{Text: "Question is no longer pending"}, nil
	}
	if threadErr != nil {
		return domain.CallbackResult{Text: "Temporary error, please retry", ShowAlert: true}, threadErr
	}
	thread := &th
	envID := firstNonEmptyString(th.EnvironmentID, rec.EnvironmentID)
	if th.PendingRequest == nil || strings.TrimSpace(th.PendingRequest.RequestID) != rec.RequestID {
		rec.Answered = true
		rec.AnsweredText = "no longer pending"
		_ = b.saveAnswerRecord(ctx, rec)
		b.clearQuestionKeyboard(ctx, q, rec, "Question is no longer pending")
		return domain.CallbackResult{Text: "Question is no longer pending"}, nil
	}
	if strings.TrimSpace(envID) == "" {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}

	if qIdx != rec.CurrentIndex {
		if qIdx < rec.CurrentIndex {
			if editErr := b.editQuestionMessage(ctx, q, rec); editErr != nil {
				return domain.CallbackResult{Text: "Failed to refresh question, tap again", ShowAlert: true}, editErr
			}
			return domain.CallbackResult{Text: fmt.Sprintf("Showing question %d/%d", rec.CurrentIndex+1, len(rec.Questions))}, nil
		}
		return domain.CallbackResult{Text: "Question already advanced"}, nil
	}
	question := rec.Questions[qIdx]
	if rec.Answers == nil {
		rec.Answers = map[string]any{}
	}
	if rec.AnswerLabels == nil {
		rec.AnswerLabels = map[string]string{}
	}
	if rec.MultiSelected == nil {
		rec.MultiSelected = map[int][]int{}
	}

	if question.MultiSelect {
		if rec.NeedsRefresh {
			if editErr := b.editQuestionMessage(ctx, q, rec); editErr != nil {
				return domain.CallbackResult{Text: "Failed to refresh selection, tap again", ShowAlert: true}, editErr
			}
			rec.NeedsRefresh = false
			_ = b.saveAnswerRecord(ctx, rec)
			return domain.CallbackResult{Text: "Refreshed selection"}, nil
		}
		if !submitMulti {
			if optIdx < 0 || optIdx >= len(question.Options) {
				return domain.CallbackResult{Text: "Expired or unknown action"}, nil
			}
			toggled := toggleIntSlice(rec.MultiSelected[qIdx], optIdx)
			rec.MultiSelected[qIdx] = toggled
			rec.NeedsRefresh = true
			if err := b.saveAnswerRecord(ctx, rec); err != nil {
				return domain.CallbackResult{Text: "Action failed"}, err
			}
			if editErr := b.editQuestionMessage(ctx, q, rec); editErr != nil {
				return domain.CallbackResult{Text: "Failed to update selection, tap again", ShowAlert: true}, editErr
			}
			rec.NeedsRefresh = false
			_ = b.saveAnswerRecord(ctx, rec)
			return domain.CallbackResult{Text: "Updated selection"}, nil
		}
		chosenIndices := rec.MultiSelected[qIdx]
		if len(chosenIndices) == 0 {
			return domain.CallbackResult{Text: "Select at least one option"}, nil
		}
		sort.Ints(chosenIndices)
		vals := make([]string, 0, len(chosenIndices))
		labels := make([]string, 0, len(chosenIndices))
		for _, i := range chosenIndices {
			if i >= 0 && i < len(question.Options) {
				vals = append(vals, question.Options[i].Value)
				labels = append(labels, question.Options[i].Label)
			}
		}
		rec.Answers[question.Key] = vals
		rec.AnswerLabels[question.Key] = strings.Join(labels, ", ")
	} else {
		if submitMulti || optIdx < 0 || optIdx >= len(question.Options) {
			return domain.CallbackResult{Text: "Expired or unknown action"}, nil
		}
		chosen := question.Options[optIdx]
		rec.Answers[question.Key] = chosen.Value
		rec.AnswerLabels[question.Key] = chosen.Label
	}

	// If more questions remain in this pending_request, advance in place without dispatching yet.
	if rec.CurrentIndex+1 < len(rec.Questions) {
		rec.CurrentIndex++
		if err := b.saveAnswerRecord(ctx, rec); err != nil {
			return domain.CallbackResult{Text: "Action failed"}, err
		}
		if editErr := b.editQuestionMessage(ctx, q, rec); editErr != nil {
			return domain.CallbackResult{Text: "Failed to load next question, tap again", ShowAlert: true}, editErr
		}
		return domain.CallbackResult{Text: fmt.Sprintf("Recorded (%d/%d)", rec.CurrentIndex, len(rec.Questions))}, nil
	}

	// All questions answered -> dispatch ONE thread_respond with complete answers map.
	answersAny := make(map[string]any, len(rec.Answers))
	for k, v := range rec.Answers {
		answersAny[k] = v
	}
	summary := summarizeAnswers(rec)
	payload := map[string]any{
		"thread_id":  rec.ThreadID,
		"request_id": rec.RequestID,
		"answers":    answersAny,
	}
	cmdID := "t3cb-" + ID("t3cb-answer", rec.ThreadID, rec.RequestID, strconv.Itoa(rec.Attempt))

	// Persist command->token association BEFORE dispatch so fast completions
	// and backend restarts can always finalize the queued message.
	rec.PendingCommandID = cmdID
	rec.PendingSummary = summary
	if err := b.saveAnswerRecord(ctx, rec); err != nil {
		return domain.CallbackResult{Text: "Action failed"}, err
	}
	if err := b.savePendingCommandRef(ctx, cmdID, token); err != nil {
		rec.PendingCommandID = ""
		rec.PendingSummary = ""
		_ = b.saveAnswerRecord(ctx, rec)
		return domain.CallbackResult{Text: "Action failed", ShowAlert: true}, err
	}

	b.cbMu.Lock()
	b.inFlightTokens[token] = true
	b.cbMu.Unlock()
	res, err := b.cp.DispatchAndWait(ctx, cmdID, envID, domain.T3ActionThreadRespond, payload, b.callbackDispatchTimeout())
	b.cbMu.Lock()
	delete(b.inFlightTokens, token)
	b.cbMu.Unlock()

	if err != nil {
		b.clearPendingCommandRef(cmdID)
		rec.Attempt++
		rec.PendingCommandID = ""
		rec.PendingSummary = ""
		_ = b.saveAnswerRecord(ctx, rec)
		return domain.CallbackResult{Text: "Failed to send answer", ShowAlert: true}, err
	}
	if res.Status == domain.T3CommandStatusFailed {
		b.clearPendingCommandRef(cmdID)
		rec.Attempt++
		rec.PendingCommandID = ""
		rec.PendingSummary = ""
		_ = b.saveAnswerRecord(ctx, rec)
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = "Failed to send answer"
		}
		return domain.CallbackResult{Text: clipRunes(msg, 180), ShowAlert: true}, errors.New(msg)
	}

	if res.Status != domain.T3CommandStatusSucceeded {
		// Reconcile once after clearing inFlightTokens in case the command
		// completed in the window between timeout and clearing the flag.
		if latest, ok, cmdErr := b.cp.Command(ctx, res.CommandID); cmdErr == nil && ok {
			if latest.Status == domain.T3CommandStatusSucceeded {
				if latestTh, thErr := b.cp.Thread(ctx, rec.ThreadID); thErr == nil {
					thread = &latestTh
				}
				b.finalizeAnsweredRecord(ctx, q, rec, summary, thread)
				return domain.CallbackResult{Text: "Answered: " + clipRunes(summary, 140)}, nil
			}
			if latest.Status == domain.T3CommandStatusFailed {
				b.clearPendingCommandRef(cmdID)
				rec.Attempt++
				rec.PendingCommandID = ""
				rec.PendingSummary = ""
				_ = b.saveAnswerRecord(ctx, rec)
				msg := strings.TrimSpace(latest.ErrorText)
				if msg == "" {
					msg = "Failed to send answer"
				}
				return domain.CallbackResult{Text: clipRunes(msg, 180), ShowAlert: true}, errors.New(msg)
			}
		}
		return domain.CallbackResult{Text: "Queued answer: " + clipRunes(summary, 130)}, nil
	}

	if latestTh, thErr := b.cp.Thread(ctx, rec.ThreadID); thErr == nil {
		thread = &latestTh
	}
	b.finalizeAnsweredRecord(ctx, q, rec, summary, thread)
	return domain.CallbackResult{Text: "Answered: " + clipRunes(summary, 140)}, nil
}

func (b *T3WorkspaceBridge) finalizePendingCommand(ctx context.Context, commandID string) {
	if commandID == "" {
		return
	}
	token := b.loadPendingCommandRef(ctx, commandID)
	if token == "" {
		return
	}
	b.cbMu.Lock()
	inFlight := b.inFlightTokens[token]
	b.cbMu.Unlock()
	if inFlight {
		// handleAnswerCallback is still awaiting DispatchAndWait and will finalize directly.
		return
	}
	unlock := b.lockToken("a:" + token)
	defer unlock()

	rec, found, err := b.loadAnswerRecord(ctx, token)
	if err != nil || !found {
		return
	}
	if rec.Answered {
		if !rec.FinalEdited {
			if edited, editErr := b.editFinalAnsweredMessage(ctx, domain.CallbackQuery{}, rec); edited && editErr == nil {
				rec.FinalEdited = true
				_ = b.saveAnswerRecord(ctx, rec)
			}
		}
		return
	}
	if rec.PendingCommandID != commandID {
		return
	}
	cmd, ok, cmdErr := b.cp.Command(ctx, commandID)
	if cmdErr != nil || !ok || cmd.Status != domain.T3CommandStatusSucceeded {
		return
	}
	var thread *domain.T3Thread
	if th, err := b.cp.Thread(ctx, rec.ThreadID); err == nil {
		thread = &th
	}
	b.finalizeAnsweredRecord(ctx, domain.CallbackQuery{}, rec, rec.PendingSummary, thread)
}

func (b *T3WorkspaceBridge) savePendingCommandRef(ctx context.Context, commandID, token string) error {
	if commandID == "" || token == "" {
		return nil
	}
	b.cbMu.Lock()
	b.pendingByCommand[commandID] = token
	b.cbMu.Unlock()
	if b.repo != nil {
		raw, _ := json.Marshal(token)
		return b.repo.SaveCheckpoint(ctx, t3CommandCheckpointBase+commandID, raw)
	}
	return nil
}

func (b *T3WorkspaceBridge) loadPendingCommandRef(ctx context.Context, commandID string) string {
	b.cbMu.Lock()
	token := b.pendingByCommand[commandID]
	b.cbMu.Unlock()
	if token != "" {
		return token
	}
	if b.repo == nil {
		return ""
	}
	raw, ok, err := b.repo.LoadCheckpoint(ctx, t3CommandCheckpointBase+commandID)
	if err != nil || !ok {
		return ""
	}
	var loaded string
	if json.Unmarshal(raw, &loaded) != nil {
		return ""
	}
	return strings.TrimSpace(loaded)
}

func (b *T3WorkspaceBridge) clearPendingCommandRef(commandID string) {
	if commandID == "" {
		return
	}
	b.cbMu.Lock()
	delete(b.pendingByCommand, commandID)
	b.cbMu.Unlock()
}

func (b *T3WorkspaceBridge) finalizeAnsweredRecord(ctx context.Context, q domain.CallbackQuery, rec *t3AnswerCallbackRecord, summary string, thread *domain.T3Thread) {
	b.clearPendingCommandRef(rec.PendingCommandID)
	rec.Answered = true
	rec.AnsweredText = summary
	rec.PendingCommandID = ""
	rec.PendingSummary = ""
	rec.NeedsRefresh = false

	edited, editErr := b.editFinalAnsweredMessage(ctx, q, rec)
	if edited && editErr == nil {
		rec.FinalEdited = true
	} else if editErr != nil && b.logger != nil {
		b.logger.Warn("failed to edit answered T3 question message", "token", rec.Token, "error", editErr.Error())
	}
	if err := b.saveAnswerRecord(ctx, rec); err != nil && b.logger != nil {
		b.logger.Warn("failed to persist answered callback state", "token", rec.Token, "error", err.Error())
	}

	convID := ""
	title := rec.Title
	if thread != nil {
		convID = thread.ConversationID
		if thread.Title != "" {
			title = thread.Title
		}
	}
	if convID == "" && rec.ThreadTopicID > 0 {
		convID = fmt.Sprintf("telegram:group:%d:topic:%d", b.workspaceGroupID, rec.ThreadTopicID)
	}
	if topicID, ok := b.workspaceTopicThreadID(convID); ok {
		_ = b.syncTopicTitleIfThreadActive(ctx, rec.ThreadID, convID, topicID, "🔄", title)
	}
	_ = b.refreshStatusBoard(ctx)
}

func (b *T3WorkspaceBridge) editFinalAnsweredMessage(ctx context.Context, q domain.CallbackQuery, rec *t3AnswerCallbackRecord) (bool, error) {
	chatID := rec.ChatID
	if chatID == 0 {
		chatID = q.ChatID
	}
	msgID := q.MessageID
	if msgID <= 0 {
		msgID = rec.MessageID
	}
	if chatID == 0 || msgID <= 0 {
		return false, nil
	}
	updatedText := renderFinalAnsweredMessage(rec, rec.AnsweredText)
	emptyKeyboard := &domain.InlineKeyboardMarkup{InlineKeyboard: [][]domain.InlineButton{}}
	if err := b.transport.EditMessageText(ctx, chatID, msgID, updatedText, emptyKeyboard); err != nil {
		return false, err
	}
	return true, nil
}

// syncTopicTitleIfThreadActive re-reads the persisted thread while holding
// topicMu so a concurrent completion (👀/✅) arriving during message edits can
// never be overwritten by a stale 🔄 prefix.
func (b *T3WorkspaceBridge) syncTopicTitleIfThreadActive(ctx context.Context, threadID, conversationID string, topicID int, prefix, fallbackTitle string) error {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()

	th, err := b.cp.Thread(ctx, threadID)
	if err != nil {
		return nil
	}
	st := strings.ToLower(strings.TrimSpace(th.Status))
	if th.PendingRequest != nil || (st != "" && st != "running" && st != "starting" && st != "queued" && st != "waiting_input" && st != "waiting") {
		return nil
	}
	if th.Title != "" {
		fallbackTitle = th.Title
	}
	return b.syncTopicTitleLocked(ctx, conversationID, topicID, prefix, fallbackTitle)
}

func (b *T3WorkspaceBridge) editQuestionMessage(ctx context.Context, q domain.CallbackQuery, rec *t3AnswerCallbackRecord) error {
	chatID := rec.ChatID
	if chatID == 0 {
		chatID = q.ChatID
	}
	msgID := q.MessageID
	if msgID <= 0 {
		msgID = rec.MessageID
	}
	if chatID == 0 || msgID <= 0 {
		return nil
	}
	text, markup := renderQuestionPrompt(rec)
	return b.transport.EditMessageText(ctx, chatID, msgID, text, markup)
}

func (b *T3WorkspaceBridge) clearQuestionKeyboard(ctx context.Context, q domain.CallbackQuery, rec *t3AnswerCallbackRecord, note string) {
	chatID := rec.ChatID
	if chatID == 0 {
		chatID = q.ChatID
	}
	msgID := q.MessageID
	if msgID <= 0 {
		msgID = rec.MessageID
	}
	if chatID == 0 || msgID <= 0 {
		return
	}
	base, _ := renderQuestionPrompt(rec)
	if base == "" {
		base = note
	} else {
		base = base + "\n\n" + note
	}
	emptyKeyboard := &domain.InlineKeyboardMarkup{InlineKeyboard: [][]domain.InlineButton{}}
	_ = b.transport.EditMessageText(ctx, chatID, msgID, clipUTF16(base, messagefmt.TextLimit), emptyKeyboard)
}

func summarizeAnswers(rec *t3AnswerCallbackRecord) string {
	if len(rec.Questions) == 1 {
		return answerDisplayValue(rec, rec.Questions[0].Key)
	}
	parts := make([]string, 0, len(rec.Questions))
	for _, q := range rec.Questions {
		if lbl := answerDisplayValue(rec, q.Key); lbl != "" {
			parts = append(parts, lbl)
		}
	}
	return strings.Join(parts, " / ")
}

func toggleIntSlice(in []int, val int) []int {
	out := make([]int, 0, len(in)+1)
	found := false
	for _, x := range in {
		if x == val {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		out = append(out, val)
	}
	return out
}

func (b *T3WorkspaceBridge) handleControlCallback(ctx context.Context, q domain.CallbackQuery, token, action string) (domain.CallbackResult, error) {
	if action != "sync" {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}
	unlock := b.lockToken("c:" + token)
	defer unlock()

	rec, found, err := b.loadControlRecord(ctx, token)
	if err != nil {
		return domain.CallbackResult{Text: "Action failed"}, err
	}
	if !found {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}
	if q.ChatID != 0 && rec.ChatID != 0 && q.ChatID != rec.ChatID {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}

	envID := rec.EnvironmentID
	if th, err := b.cp.Thread(ctx, rec.ThreadID); err == nil && th.EnvironmentID != "" {
		envID = th.EnvironmentID
	}
	if strings.TrimSpace(envID) == "" {
		return domain.CallbackResult{Text: "Expired or unknown action"}, nil
	}

	res, err := b.cp.DispatchAndWait(ctx, "", envID, domain.T3ActionThreadControl, map[string]any{
		"thread_id": rec.ThreadID,
		"action":    action,
	}, b.callbackDispatchTimeout())
	if err != nil {
		return domain.CallbackResult{Text: "Sync failed", ShowAlert: true}, err
	}
	if res.Status == domain.T3CommandStatusFailed {
		msg := strings.TrimSpace(res.Error)
		if msg == "" {
			msg = "Sync failed"
		}
		return domain.CallbackResult{Text: clipRunes(msg, 180), ShowAlert: true}, errors.New(msg)
	}
	if res.Status != domain.T3CommandStatusSucceeded {
		return domain.CallbackResult{Text: "Sync queued"}, nil
	}
	_ = b.refreshStatusBoard(ctx)
	return domain.CallbackResult{Text: "Synced"}, nil
}

func (b *T3WorkspaceBridge) lockToken(key string) func() {
	b.cbMu.Lock()
	mu, ok := b.tokenLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		b.tokenLocks[key] = mu
	}
	b.cbMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func cloneAnswerRecord(rec *t3AnswerCallbackRecord) *t3AnswerCallbackRecord {
	if rec == nil {
		return nil
	}
	raw, _ := json.Marshal(rec)
	var out t3AnswerCallbackRecord
	_ = json.Unmarshal(raw, &out)
	return &out
}

func (b *T3WorkspaceBridge) saveAnswerRecord(ctx context.Context, rec *t3AnswerCallbackRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if b.repo != nil {
		if err := b.repo.SaveCheckpoint(ctx, t3AnswerCheckpointBase+rec.Token, raw); err != nil {
			return err
		}
	}
	cp := cloneAnswerRecord(rec)
	b.cbMu.Lock()
	b.answerRecords[rec.Token] = cp
	b.cbMu.Unlock()
	return nil
}

func (b *T3WorkspaceBridge) loadAnswerRecord(ctx context.Context, token string) (*t3AnswerCallbackRecord, bool, error) {
	b.cbMu.Lock()
	if rec, ok := b.answerRecords[token]; ok {
		cp := cloneAnswerRecord(rec)
		b.cbMu.Unlock()
		return cp, true, nil
	}
	b.cbMu.Unlock()

	if b.repo == nil {
		return nil, false, nil
	}
	raw, ok, err := b.repo.LoadCheckpoint(ctx, t3AnswerCheckpointBase+token)
	if err != nil || !ok {
		return nil, false, err
	}
	var rec t3AnswerCallbackRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, false, nil
	}
	b.cbMu.Lock()
	b.answerRecords[token] = cloneAnswerRecord(&rec)
	b.cbMu.Unlock()
	return &rec, true, nil
}

func (b *T3WorkspaceBridge) saveControlRecord(ctx context.Context, rec *t3ControlCallbackRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if b.repo != nil {
		if err := b.repo.SaveCheckpoint(ctx, t3ControlCheckpointBase+rec.Token, raw); err != nil {
			return err
		}
	}
	cp := *rec
	b.cbMu.Lock()
	b.controlRecords[rec.Token] = &cp
	b.cbMu.Unlock()
	return nil
}

func (b *T3WorkspaceBridge) loadControlRecord(ctx context.Context, token string) (*t3ControlCallbackRecord, bool, error) {
	b.cbMu.Lock()
	if rec, ok := b.controlRecords[token]; ok {
		cp := *rec
		b.cbMu.Unlock()
		return &cp, true, nil
	}
	b.cbMu.Unlock()

	if b.repo == nil {
		return nil, false, nil
	}
	raw, ok, err := b.repo.LoadCheckpoint(ctx, t3ControlCheckpointBase+token)
	if err != nil || !ok {
		return nil, false, err
	}
	var rec t3ControlCallbackRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, false, nil
	}
	b.cbMu.Lock()
	cp := rec
	b.controlRecords[token] = &cp
	b.cbMu.Unlock()
	return &rec, true, nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func isValidHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func clipUTF16(s string, maxUnits int) string {
	if maxUnits <= 0 || utf16Len(s) <= maxUnits {
		return s
	}
	var b strings.Builder
	units := 0
	limit := maxUnits - 1
	for _, r := range s {
		n := 1
		if r > 0xFFFF {
			n = 2
		}
		if units+n > limit {
			break
		}
		b.WriteRune(r)
		units += n
	}
	b.WriteRune('…')
	return b.String()
}
