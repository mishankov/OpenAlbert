package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
	"openalbert/apps/backend/internal/t3"
)

func setupT3WorkspaceBridge(t *testing.T) (*T3WorkspaceBridge, *t3.ControlPlane, *storage.Memory, *fakeWorkspaceTransport) {
	t.Helper()
	repo := storage.NewMemory()
	transport := &fakeWorkspaceTransport{}
	cp := t3.New(repo, repo, t3.Config{
		OwnerUserID:     7,
		OnlineThreshold: 3 * time.Minute,
		DispatchTimeout: 2 * time.Second,
	})
	bridge := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
		ControlPlane:     cp,
		Repo:             repo,
		Transport:        transport,
		OwnerUserID:      7,
		WorkspaceGroupID: -100555,
	})
	cp.AddObserver(bridge)
	return bridge, cp, repo, transport
}

func TestT3WorkspaceBridgeTopicPrefixLifecycleAndNoAccumulation(t *testing.T) {
	_, cp, repo, transport := setupT3WorkspaceBridge(t)
	ctx := context.Background()

	topicConv := domain.Conversation{
		ID:       "telegram:group:-100555:topic:42",
		Title:    "Refactor Auth",
		ChatID:   -100555,
		ThreadID: 42,
		OwnerID:  7,
	}
	if err := repo.SaveConversation(ctx, topicConv); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:       "th-prefix-1",
		EnvironmentID:  "env-1",
		ConversationID: topicConv.ID,
		Title:          "Refactor Auth",
		Status:         "running",
	}); err != nil {
		t.Fatal(err)
	}

	ingest := func(ev domain.T3Event) {
		t.Helper()
		ev.EnvironmentID = "env-1"
		ev.ThreadID = "th-prefix-1"
		if _, err := cp.IngestEvent(ctx, ev, nil); err != nil {
			t.Fatalf("ingest %s: %v", ev.EventID, err)
		}
	}

	// 1. thread_launched -> 🔄 Refactor Auth
	ingest(domain.T3Event{EventID: "ev-1", Kind: domain.T3EventThreadLaunched, Status: "running"})
	// 2. Redundant running progress -> MUST NOT call EditForumTopic again!
	ingest(domain.T3Event{EventID: "ev-2", Kind: domain.T3EventThreadProgress, Status: "running", Summary: "still running"})
	// 3. thread_waiting_input -> ❓ Refactor Auth (no accumulated 🔄)
	ingest(domain.T3Event{EventID: "ev-3", Kind: domain.T3EventThreadWaitingInput, Status: "waiting_input"})
	// 4. thread_completed with PR URL -> 👀 Refactor Auth
	ingest(domain.T3Event{EventID: "ev-4", Kind: domain.T3EventThreadCompleted, Status: "completed", PRURL: "https://github.com/org/repo/pull/12"})
	// 5. thread_completed without PR URL on another thread -> ✅ Fix Tests
	topic2 := domain.Conversation{
		ID:       "telegram:group:-100555:topic:43",
		Title:    "🔄 ❌ Fix Tests",
		ChatID:   -100555,
		ThreadID: 43,
		OwnerID:  7,
	}
	if err := repo.SaveConversation(ctx, topic2); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:       "th-prefix-2",
		EnvironmentID:  "env-1",
		ConversationID: topic2.ID,
		Title:          "Fix Tests",
		Status:         "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-5",
		EnvironmentID: "env-1",
		ThreadID:      "th-prefix-2",
		Kind:          domain.T3EventThreadCompleted,
		Status:        "completed",
	}, nil); err != nil {
		t.Fatal(err)
	}
	// 6. thread_failed -> ❌ Fix Tests
	if _, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-6",
		EnvironmentID: "env-1",
		ThreadID:      "th-prefix-2",
		Kind:          domain.T3EventThreadFailed,
		Status:        "failed",
		Error:         "boom",
	}, nil); err != nil {
		t.Fatal(err)
	}
	// 7. Non-workspace conversation (private DM) -> MUST NOT call EditForumTopic.
	if _, err := repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:       "th-dm",
		EnvironmentID:  "env-1",
		ConversationID: "personal:7",
		Title:          "DM Thread",
		Status:         "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-7",
		EnvironmentID: "env-1",
		ThreadID:      "th-dm",
		Kind:          domain.T3EventThreadLaunched,
		Status:        "running",
	}, nil); err != nil {
		t.Fatal(err)
	}

	transport.mu.Lock()
	edited := append([]struct {
		chatID   int64
		threadID int
		name     string
	}(nil), transport.editedTopics...)
	transport.mu.Unlock()

	want := []struct {
		threadID int
		name     string
	}{
		{42, "🔄 Refactor Auth"},
		{42, "❓ Refactor Auth"},
		{42, "👀 Refactor Auth"},
		{43, "✅ Fix Tests"},
		{43, "❌ Fix Tests"},
	}
	if len(edited) != len(want) {
		t.Fatalf("expected %d EditForumTopic calls, got %d: %+v", len(want), len(edited), edited)
	}
	for i, w := range want {
		if edited[i].chatID != -100555 || edited[i].threadID != w.threadID || edited[i].name != w.name {
			t.Fatalf("edit[%d] = (%d, %d, %q), want (-100555, %d, %q)", i, edited[i].chatID, edited[i].threadID, edited[i].name, w.threadID, w.name)
		}
	}
}

func TestT3WorkspaceBridgePinnedControlBoardAndConcurrency(t *testing.T) {
	_, cp, repo, transport := setupT3WorkspaceBridge(t)
	ctx := context.Background()

	if _, err := cp.RegisterHost(ctx, t3.RegisterHostRequest{
		EnvironmentID: "env-mac",
		Label:         "MacBook Pro",
		Projects:      []domain.T3Project{{ID: "p1", Title: "OpenAlbert", WorkspaceRoot: "/code/oa"}},
	}); err != nil {
		t.Fatal(err)
	}

	transport.mu.Lock()
	if len(transport.sentMessages) != 1 || len(transport.pinnedMessages) != 1 {
		t.Fatalf("expected initial #Control board sent and pinned once, sent=%d pinned=%d", len(transport.sentMessages), len(transport.pinnedMessages))
	}
	initialBoard := transport.sentMessages[0].text
	transport.mu.Unlock()

	if !strings.Contains(initialBoard, "🟢 MacBook Pro (1 project)") {
		t.Fatalf("expected online host on #Control board, got:\n%s", initialBoard)
	}

	// Concurrent events update the pinned board safely without races or extra pinned messages.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			thID := fmt.Sprintf("th-conc-%d", idx)
			_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
				ThreadID:      thID,
				EnvironmentID: "env-mac",
				Title:         fmt.Sprintf("Task %d", idx),
				Status:        "running",
			})
			_, _ = cp.IngestEvent(ctx, domain.T3Event{
				EventID:       fmt.Sprintf("ev-conc-%d", idx),
				EnvironmentID: "env-mac",
				ThreadID:      thID,
				Kind:          domain.T3EventThreadCompleted,
				PRURL:         fmt.Sprintf("https://github.com/org/repo/pull/%d", 100+idx),
			}, nil)
		}(i)
	}
	wg.Wait()

	transport.mu.Lock()
	pinnedCount := len(transport.pinnedMessages)
	lastEdit := ""
	if len(transport.editedMessages) > 0 {
		lastEdit = transport.editedMessages[len(transport.editedMessages)-1].text
	}
	transport.mu.Unlock()

	if pinnedCount != 1 {
		t.Fatalf("expected exactly 1 pinned #Control message across concurrent updates, got %d", pinnedCount)
	}
	if !strings.Contains(lastEdit, "🟢 MacBook Pro") || !strings.Contains(lastEdit, "👀 Task 0 — https://github.com/org/repo/pull/100") {
		t.Fatalf("expected final #Control board to list host and PR thread, got:\n%s", lastEdit)
	}
}

func TestT3WorkspaceBridgeWaitingInputCallbackAndPRSyncCallback(t *testing.T) {
	bridge, cp, repo, transport := setupT3WorkspaceBridge(t)
	ctx := context.Background()

	if _, err := cp.RegisterHost(ctx, t3.RegisterHostRequest{
		EnvironmentID: "env-lan",
		Label:         "Mac Studio",
	}); err != nil {
		t.Fatal(err)
	}
	topicConv := domain.Conversation{
		ID:       "telegram:group:-100555:topic:77",
		Title:    "Ship Glue",
		ChatID:   -100555,
		ThreadID: 77,
		OwnerID:  7,
	}
	if err := repo.SaveConversation(ctx, topicConv); err != nil {
		t.Fatal(err)
	}
	longThreadID := "thread-very-long-identifier-exceeding-telegram-64-byte-limit-0123456789abcdef"
	longReqID := "pending-request-very-long-identifier-exceeding-telegram-64-byte-limit-fedcba9876543210"
	if _, err := repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
		ThreadID:       longThreadID,
		EnvironmentID:  "env-lan",
		ConversationID: topicConv.ID,
		Title:          "Ship Glue",
		Status:         "running",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Emit thread_waiting_input with structured options and huge IDs -> callback_data must stay <= 64 UTF-8 bytes!
	if _, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-wait-opts",
		EnvironmentID: "env-lan",
		ThreadID:      longThreadID,
		Kind:          domain.T3EventThreadWaitingInput,
		PendingRequest: &domain.T3PendingRequest{
			RequestID: longReqID,
			Questions: []json.RawMessage{
				json.RawMessage(`{"id":"deploy_mode","question":"How should we deploy?","options":[{"label":"Canary","description":"10% traffic"},{"label":"Full","description":"100% traffic"}]}`),
			},
		},
	}, nil); err != nil {
		t.Fatal(err)
	}

	transport.mu.Lock()
	var questionMsgID int64
	var opt0Data string
	for _, m := range transport.sentMessages {
		if m.conv.ThreadID == 77 && m.markup != nil && len(m.markup.InlineKeyboard) > 0 {
			questionMsgID = m.msgID
			for _, row := range m.markup.InlineKeyboard {
				for _, btn := range row {
					if len(btn.CallbackData) == 0 || len(btn.CallbackData) > 64 {
						t.Fatalf("callback_data violates Telegram 1..64 byte limit: %q (%d bytes)", btn.CallbackData, len(btn.CallbackData))
					}
					if btn.Text == "Canary" {
						opt0Data = btn.CallbackData
					}
				}
			}
		}
	}
	transport.mu.Unlock()
	if questionMsgID == 0 || opt0Data == "" {
		t.Fatalf("expected inline option buttons posted to topic 77, got msgID=%d opt0=%q", questionMsgID, opt0Data)
	}

	// 2. Non-owner callback MUST be rejected.
	if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{
		ID:        "cb-non-owner",
		UserID:    99,
		ChatID:    -100555,
		ThreadID:  77,
		MessageID: questionMsgID,
		Data:      opt0Data,
	}); err == nil {
		t.Fatal("expected non-owner callback to be rejected")
	}

	// 3. Unknown/expired token MUST be handled gracefully (no error, clean toast).
	resUnknown, err := bridge.HandleCallback(ctx, domain.CallbackQuery{
		ID:        "cb-unknown",
		UserID:    7,
		ChatID:    -100555,
		ThreadID:  77,
		MessageID: questionMsgID,
		Data:      "t3:a:deadbeefcafe:0",
	})
	if err != nil || !strings.Contains(strings.ToLower(resUnknown.Text), "expired") {
		t.Fatalf("expected graceful expired message for unknown token, got res=%+v err=%v", resUnknown, err)
	}

	// 4. Owner taps "Canary" -> dispatches thread_respond over ControlPlane, clears pending_request, edits message & clears keyboard!
	wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-lan")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	var capturedRespondPayload map[string]any
	go func() {
		<-wakeup
		cmds, err := cp.ClaimPendingCommands(ctx, "env-lan", 10)
		if err != nil || len(cmds) != 1 {
			t.Errorf("claim respond cmd: %+v err=%v", cmds, err)
			return
		}
		if cmds[0].Action != domain.T3ActionThreadRespond {
			t.Errorf("expected thread_respond, got %s", cmds[0].Action)
		}
		_ = json.Unmarshal(cmds[0].Payload, &capturedRespondPayload)
		_ = cp.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
			EnvironmentID: "env-lan",
			OK:            true,
			Result:        json.RawMessage(`{"accepted":true}`),
		})
	}()

	resAns, err := bridge.HandleCallback(ctx, domain.CallbackQuery{
		ID:        "cb-owner-ans",
		UserID:    7,
		ChatID:    -100555,
		ThreadID:  77,
		MessageID: questionMsgID,
		Data:      opt0Data,
	})
	if err != nil || !strings.Contains(resAns.Text, "Canary") {
		t.Fatalf("owner answer callback failed: res=%+v err=%v", resAns, err)
	}
	if capturedRespondPayload["thread_id"] != longThreadID || capturedRespondPayload["request_id"] != longReqID {
		t.Fatalf("unexpected thread_respond payload: %+v", capturedRespondPayload)
	}
	answersMap, _ := capturedRespondPayload["answers"].(map[string]any)
	if answersMap["deploy_mode"] != "Canary" {
		t.Fatalf("expected answers[deploy_mode]=Canary, got %+v", answersMap)
	}
	th, err := cp.Thread(ctx, longThreadID)
	if err != nil || th.PendingRequest != nil {
		t.Fatalf("expected pending_request cleared after callback, got %+v err=%v", th, err)
	}

	transport.mu.Lock()
	foundEditedQuestion := false
	for _, e := range transport.editedMessages {
		if e.msgID == questionMsgID {
			foundEditedQuestion = true
			if !strings.Contains(e.text, "✓ Answered: Canary") {
				t.Fatalf("edited question missing answered marker: %q", e.text)
			}
			if e.markup == nil || len(e.markup.InlineKeyboard) != 0 {
				t.Fatalf("expected inline keyboard cleared (empty non-nil markup), got %+v", e.markup)
			}
		}
	}
	transport.mu.Unlock()
	if !foundEditedQuestion {
		t.Fatal("expected EditMessageText on question message after owner answer")
	}

	// 5. thread_completed with pr_url -> posts Open PR + Sync buttons, tapping Sync dispatches thread_control(sync).
	if _, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:       "ev-done-pr",
		EnvironmentID: "env-lan",
		ThreadID:      longThreadID,
		Kind:          domain.T3EventThreadCompleted,
		PRURL:         "https://github.com/mishankov/OpenAlbert/pull/67",
		Summary:       "All green",
	}, nil); err != nil {
		t.Fatal(err)
	}

	transport.mu.Lock()
	var syncData string
	var prButtonURL string
	for _, m := range transport.sentMessages {
		if m.conv.ThreadID == 77 && m.msgID != questionMsgID && m.markup != nil {
			for _, row := range m.markup.InlineKeyboard {
				for _, btn := range row {
					if btn.URL != "" {
						prButtonURL = btn.URL
					}
					if strings.Contains(btn.Text, "Sync") {
						if len(btn.CallbackData) == 0 || len(btn.CallbackData) > 64 {
							t.Fatalf("sync callback_data violates 64-byte limit: %q", btn.CallbackData)
						}
						syncData = btn.CallbackData
					}
				}
			}
		}
	}
	transport.mu.Unlock()
	if syncData == "" || prButtonURL != "https://github.com/mishankov/OpenAlbert/pull/67" {
		t.Fatalf("expected PR URL and Sync button, got url=%q sync=%q", prButtonURL, syncData)
	}

	var capturedControl map[string]any
	go func() {
		<-wakeup
		cmds, err := cp.ClaimPendingCommands(ctx, "env-lan", 10)
		if err != nil || len(cmds) != 1 {
			t.Errorf("claim control cmd: %+v err=%v", cmds, err)
			return
		}
		_ = json.Unmarshal(cmds[0].Payload, &capturedControl)
		_ = cp.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
			EnvironmentID: "env-lan",
			OK:            true,
			Result:        json.RawMessage(`{"status":"completed"}`),
		})
	}()

	resSync, err := bridge.HandleCallback(ctx, domain.CallbackQuery{
		ID:       "cb-sync",
		UserID:   7,
		ChatID:   -100555,
		ThreadID: 77,
		Data:     syncData,
	})
	if err != nil || resSync.Text != "Synced" {
		t.Fatalf("sync callback failed: res=%+v err=%v", resSync, err)
	}
	if capturedControl["thread_id"] != longThreadID || capturedControl["action"] != "sync" {
		t.Fatalf("unexpected thread_control payload: %+v", capturedControl)
	}
}

func TestT3WorkspaceBridgeReviewFindingsCoverage(t *testing.T) {
	ctx := context.Background()

	t.Run("S1_waiting_input_buttons_suppress_wake_but_fallback_and_completion_wake", func(t *testing.T) {
		_, cp, repo, _ := setupT3WorkspaceBridge(t)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:51", Title: "Wake Test", ChatID: -100555, ThreadID: 51, OwnerID: 7}
		if err := repo.SaveConversation(ctx, topicConv); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s1", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "Wake Test", Status: "running",
		}); err != nil {
			t.Fatal(err)
		}

		// Structured waiting_input -> buttons delivered -> NO LLM wake queued.
		if _, err := cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s1-structured", EnvironmentID: "env-1", ThreadID: "th-s1", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-structured",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q1","question":"Pick one?","options":["A","B"]}`)},
			},
		}, nil); err != nil {
			t.Fatal(err)
		}
		if pending, _ := repo.PendingIncoming(ctx, 50); len(pending) != 0 {
			t.Fatalf("structured waiting_input handled by buttons must not queue LLM wake: %+v", pending)
		}

		// Free-text waiting_input (no options) -> fallback LLM wake IS queued.
		if _, err := cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s1-freetext", EnvironmentID: "env-1", ThreadID: "th-s1", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-freetext",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q2","question":"Describe the custom endpoint URL"}`)},
			},
		}, nil); err != nil {
			t.Fatal(err)
		}
		pending, _ := repo.PendingIncoming(ctx, 50)
		if len(pending) != 1 || pending[0].ID != "t3-event:ev-s1-freetext" {
			t.Fatalf("expected fallback LLM wake for free-text question, got %+v", pending)
		}

		// thread_completed -> completion wake IS always queued.
		if _, err := cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s1-done", EnvironmentID: "env-1", ThreadID: "th-s1", Kind: domain.T3EventThreadCompleted,
			PRURL: "https://github.com/org/repo/pull/51",
		}, nil); err != nil {
			t.Fatal(err)
		}
		pending, _ = repo.PendingIncoming(ctx, 50)
		if len(pending) != 2 || pending[1].ID != "t3-event:ev-s1-done" {
			t.Fatalf("expected completion wake preserved, got %+v", pending)
		}
	})

	t.Run("S2_idle_completed_and_failed_render_effective_status_on_control_board", func(t *testing.T) {
		_, cp, repo, transport := setupT3WorkspaceBridge(t)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-1", Label: "Mac"})
		for _, p := range []domain.T3ThreadPatch{
			{ThreadID: "th-idle-pr", EnvironmentID: "env-1", Title: "PR Idle", Status: "running"},
			{ThreadID: "th-idle-done", EnvironmentID: "env-1", Title: "Done Idle", Status: "running"},
			{ThreadID: "th-idle-fail", EnvironmentID: "env-1", Title: "Fail Idle", Status: "running"},
		} {
			_, _ = repo.UpsertT3Thread(ctx, p)
		}
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s2-1", EnvironmentID: "env-1", ThreadID: "th-idle-pr",
			Kind: domain.T3EventThreadCompleted, Status: "idle", PRURL: "https://github.com/org/repo/pull/22",
		}, nil)
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s2-2", EnvironmentID: "env-1", ThreadID: "th-idle-done",
			Kind: domain.T3EventThreadCompleted, Status: "idle",
		}, nil)
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s2-3", EnvironmentID: "env-1", ThreadID: "th-idle-fail",
			Kind: domain.T3EventThreadFailed, Status: "idle", Error: "tests failed",
		}, nil)

		transport.mu.Lock()
		lastBoard := transport.editedMessages[len(transport.editedMessages)-1].text
		transport.mu.Unlock()

		for _, want := range []string{
			"👀 PR Idle — https://github.com/org/repo/pull/22",
			"✅ Done Idle",
			"❌ Fail Idle",
		} {
			if !strings.Contains(lastBoard, want) {
				t.Fatalf("#Control board missing %q:\n%s", want, lastBoard)
			}
		}
	})

	t.Run("S3_offline_queue_preserves_buttons_and_failed_attempt_allows_retry", func(t *testing.T) {
		repo := storage.NewMemory()
		transport := &fakeWorkspaceTransport{}
		clockMu := sync.Mutex{}
		now := time.Now().UTC()
		cp := t3.New(repo, repo, t3.Config{
			OwnerUserID:     7,
			OnlineThreshold: time.Minute,
			DispatchTimeout: 2 * time.Second,
			Now: func() time.Time {
				clockMu.Lock()
				defer clockMu.Unlock()
				return now
			},
		})
		bridge := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp, Repo: repo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp.AddObserver(bridge)

		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-s3", Label: "Laptop"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:53", Title: "Retry Test", ChatID: -100555, ThreadID: 53, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s3", EnvironmentID: "env-s3", ConversationID: topicConv.ID, Title: "Retry Test", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s3-wait", EnvironmentID: "env-s3", ThreadID: "th-s3", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-s3",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"confirm","question":"Ship?","options":["Yes","No"]}`)},
			},
		}, nil)

		transport.mu.Lock()
		var qMsgID int64
		var yesData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 53 && m.markup != nil {
				qMsgID = m.msgID
				yesData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.mu.Unlock()

		// Advance clock so host is offline.
		clockMu.Lock()
		now = now.Add(5 * time.Minute)
		clockMu.Unlock()

		// 1st tap while offline: queued, buttons NOT removed!
		res1, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-1", UserID: 7, ChatID: -100555, ThreadID: 53, MessageID: qMsgID, Data: yesData})
		if err != nil || !strings.Contains(res1.Text, "Queued") {
			t.Fatalf("expected queued response while offline, got res=%+v err=%v", res1, err)
		}
		transport.mu.Lock()
		for _, e := range transport.editedMessages {
			if e.msgID == qMsgID {
				t.Fatalf("offline queued tap must NOT remove buttons prematurely: %+v", e)
			}
		}
		transport.mu.Unlock()

		// 2nd tap while queued: reports already queued, does not create a duplicate command.
		res2, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-2", UserID: 7, ChatID: -100555, ThreadID: 53, MessageID: qMsgID, Data: yesData})
		if err != nil || !strings.Contains(res2.Text, "already queued") {
			t.Fatalf("expected already queued toast, got res=%+v err=%v", res2, err)
		}

		// Queued command later FAILS on host -> owner taps again and gets a FRESH command ID that succeeds!
		cmds, err := cp.ClaimPendingCommands(ctx, "env-s3", 10)
		if err != nil || len(cmds) != 1 {
			t.Fatalf("expected 1 queued command, got %+v err=%v", cmds, err)
		}
		firstCmdID := cmds[0].ID
		if err := cp.RecordCommandResult(ctx, firstCmdID, t3.CommandResultRequest{
			EnvironmentID: "env-s3", OK: false, Error: "transient MCP error",
		}); err != nil {
			t.Fatal(err)
		}

		// Host comes back online via SSE subscription.
		wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-s3")
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()

		var secondCmdID string
		go func() {
			<-wakeup
			claimed, _ := cp.ClaimPendingCommands(ctx, "env-s3", 10)
			if len(claimed) == 1 {
				secondCmdID = claimed[0].ID
				_ = cp.RecordCommandResult(ctx, claimed[0].ID, t3.CommandResultRequest{
					EnvironmentID: "env-s3", OK: true, Result: json.RawMessage(`{"accepted":true}`),
				})
			}
		}()

		res3, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-3", UserID: 7, ChatID: -100555, ThreadID: 53, MessageID: qMsgID, Data: yesData})
		if err != nil || !strings.Contains(res3.Text, "Answered: Yes") {
			t.Fatalf("retry after failure should succeed: res=%+v err=%v", res3, err)
		}
		if secondCmdID == "" || secondCmdID == firstCmdID {
			t.Fatalf("retry must use a fresh command ID; first=%q second=%q", firstCmdID, secondCmdID)
		}
	})

	t.Run("S4_answered_in_t3_running_thread_rejects_stale_telegram_button", func(t *testing.T) {
		bridge, cp, repo, transport := setupT3WorkspaceBridge(t)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-s4", Label: "Mac"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:54", Title: "Stale Btn", ChatID: -100555, ThreadID: 54, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s4", EnvironmentID: "env-s4", ConversationID: topicConv.ID, Title: "Stale Btn", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s4-wait", EnvironmentID: "env-s4", ThreadID: "th-s4", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-s4",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Continue?","options":["Yes","No"]}`)},
			},
		}, nil)
		transport.mu.Lock()
		var qMsgID int64
		var cbData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 54 && m.markup != nil {
				qMsgID = m.msgID
				cbData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.mu.Unlock()

		// Owner answers directly in T3 UI -> bridge reports thread_progress(running) clearing pending_request.
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s4-resumed", EnvironmentID: "env-s4", ThreadID: "th-s4",
			Kind: domain.T3EventThreadProgress, Status: "running",
		}, nil)

		res, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-stale", UserID: 7, ChatID: -100555, ThreadID: 54, MessageID: qMsgID, Data: cbData})
		if err != nil || !strings.Contains(strings.ToLower(res.Text), "no longer pending") {
			t.Fatalf("expected stale button rejection toast, got res=%+v err=%v", res, err)
		}
		if cmds, _ := cp.ClaimPendingCommands(ctx, "env-s4", 10); len(cmds) != 0 {
			t.Fatalf("stale button must not dispatch thread_respond: %+v", cmds)
		}
	})

	t.Run("S5_two_question_and_multiselect_dispatches_single_complete_answers_map", func(t *testing.T) {
		bridge, cp, repo, transport := setupT3WorkspaceBridge(t)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-s5", Label: "Mac"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:55", Title: "Multi Q", ChatID: -100555, ThreadID: 55, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s5", EnvironmentID: "env-s5", ConversationID: topicConv.ID, Title: "Multi Q", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s5-wait", EnvironmentID: "env-s5", ThreadID: "th-s5", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-s5-multi",
				Questions: []json.RawMessage{
					json.RawMessage(`{"id":"env","question":"Target environment?","options":["Staging","Prod"]}`),
					json.RawMessage(`{"id":"checks","question":"Run checks?","is_multi_select":true,"options":["Unit","E2E","Lint"]}`),
				},
			},
		}, nil)
		// Zero LLM wake for multi-question structured request.
		if pending, _ := repo.PendingIncoming(ctx, 10); len(pending) != 0 {
			t.Fatalf("expected 0 wakes for structured multi-question request, got %+v", pending)
		}

		transport.mu.Lock()
		var qMsgID int64
		var q0Opt0 string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 55 && m.markup != nil {
				qMsgID = m.msgID
				q0Opt0 = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.mu.Unlock()
		if qMsgID == 0 || q0Opt0 == "" {
			t.Fatal("expected first question buttons posted")
		}

		// Tap Q1 option 0 ("Staging") -> advances in place to Q2 without dispatching yet.
		resQ1, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-q1", UserID: 7, ChatID: -100555, ThreadID: 55, MessageID: qMsgID, Data: q0Opt0})
		if err != nil || !strings.Contains(resQ1.Text, "1/2") {
			t.Fatalf("Q1 tap failed: res=%+v err=%v", resQ1, err)
		}
		if cmds, _ := cp.ClaimPendingCommands(ctx, "env-s5", 10); len(cmds) != 0 {
			t.Fatalf("must not dispatch before all questions answered: %+v", cmds)
		}

		// Read Q2 buttons from edited message: toggle "Unit" + "Lint", then tap Confirm.
		transport.mu.Lock()
		var q2Markup *domain.InlineKeyboardMarkup
		for _, e := range transport.editedMessages {
			if e.msgID == qMsgID {
				q2Markup = e.markup
			}
		}
		transport.mu.Unlock()
		var unitData, lintData, confirmData string
		for _, row := range q2Markup.InlineKeyboard {
			for _, btn := range row {
				if len(btn.CallbackData) > 64 {
					t.Fatalf("callback_data too long: %q", btn.CallbackData)
				}
				switch {
				case strings.Contains(btn.Text, "Unit"):
					unitData = btn.CallbackData
				case strings.Contains(btn.Text, "Lint"):
					lintData = btn.CallbackData
				case strings.Contains(btn.Text, "Confirm"):
					confirmData = btn.CallbackData
				}
			}
		}
		if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-q2-u", UserID: 7, ChatID: -100555, ThreadID: 55, MessageID: qMsgID, Data: unitData}); err != nil {
			t.Fatal(err)
		}
		if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-q2-l", UserID: 7, ChatID: -100555, ThreadID: 55, MessageID: qMsgID, Data: lintData}); err != nil {
			t.Fatal(err)
		}

		wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-s5")
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()

		var captured map[string]any
		go func() {
			<-wakeup
			cmds, _ := cp.ClaimPendingCommands(ctx, "env-s5", 10)
			if len(cmds) == 1 {
				_ = json.Unmarshal(cmds[0].Payload, &captured)
				_ = cp.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
					EnvironmentID: "env-s5", OK: true, Result: json.RawMessage(`{"accepted":true}`),
				})
			}
		}()

		resConfirm, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-q2-confirm", UserID: 7, ChatID: -100555, ThreadID: 55, MessageID: qMsgID, Data: confirmData})
		if err != nil || !strings.Contains(resConfirm.Text, "Answered:") {
			t.Fatalf("confirm failed: res=%+v err=%v", resConfirm, err)
		}
		ansMap, _ := captured["answers"].(map[string]any)
		checksRaw, _ := ansMap["checks"].([]any)
		if ansMap["env"] != "Staging" || len(checksRaw) != 2 || checksRaw[0] != "Unit" || checksRaw[1] != "Lint" {
			t.Fatalf("expected complete answers map {env:Staging, checks:[Unit Lint]}, got %+v", ansMap)
		}
	})

	t.Run("S6_late_waiting_input_does_not_overwrite_completed_topic_or_post_obsolete_buttons", func(t *testing.T) {
		_, cp, repo, transport := setupT3WorkspaceBridge(t)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:56", Title: "Order Test", ChatID: -100555, ThreadID: 56, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s6", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "Order Test", Status: "running",
		})
		tDone := time.Date(2026, 11, 25, 12, 0, 10, 0, time.UTC)
		tOlder := tDone.Add(-5 * time.Second)

		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s6-done", EnvironmentID: "env-1", ThreadID: "th-s6",
			Kind: domain.T3EventThreadCompleted, PRURL: "https://github.com/org/repo/pull/56", OccurredAt: tDone,
		}, nil)
		transport.mu.Lock()
		editsAfterDone := len(transport.editedTopics)
		sentAfterDone := len(transport.sentMessages)
		transport.mu.Unlock()

		// Older waiting_input arrives late -> must NOT rename topic to ❓ or post obsolete buttons!
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s6-late-wait", EnvironmentID: "env-1", ThreadID: "th-s6",
			Kind: domain.T3EventThreadWaitingInput, OccurredAt: tOlder,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-stale",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Old?","options":["A"]}`)},
			},
		}, nil)

		transport.mu.Lock()
		defer transport.mu.Unlock()
		if len(transport.editedTopics) != editsAfterDone {
			t.Fatalf("late waiting_input must not rename completed topic: %+v", transport.editedTopics)
		}
		for _, m := range transport.sentMessages[sentAfterDone:] {
			if m.conv.ThreadID == 56 {
				t.Fatalf("late waiting_input must not post obsolete buttons to topic 56: %+v", m)
			}
		}
	})

	t.Run("S7_manual_topic_from_incoming_message_never_renames_to_supergroup_title", func(t *testing.T) {
		_, cp, repo, transport := setupT3WorkspaceBridge(t)
		// Topic first seen via incoming Telegram message carrying only chat.title ("OpenAlbert Workspace").
		manualTopic := domain.Conversation{
			ID:            "telegram:group:-100555:topic:57",
			Title:         "OpenAlbert Workspace",
			TitleFromChat: true,
			ChatID:        -100555,
			ThreadID:      57,
			OwnerID:       7,
		}
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
			ID:           "tg-manual-1",
			Conversation: manualTopic,
			Text:         "Launch work here",
			CreatedAt:    time.Now().UTC(),
		}, 1); err != nil {
			t.Fatal(err)
		}
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s7", EnvironmentID: "env-1", ConversationID: manualTopic.ID, Title: "Fix Cache Bug", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s7-launch", EnvironmentID: "env-1", ThreadID: "th-s7",
			Kind: domain.T3EventThreadLaunched, Title: "Fix Cache Bug",
		}, nil)

		transport.mu.Lock()
		defer transport.mu.Unlock()
		if len(transport.editedTopics) != 1 || transport.editedTopics[0].name != "🔄 Fix Cache Bug" {
			t.Fatalf("expected thread title fallback '🔄 Fix Cache Bug' instead of supergroup title, got %+v", transport.editedTopics)
		}
	})

	t.Run("R2_S1_board_error_and_duplicate_replay_still_suppress_wake", func(t *testing.T) {
		_, cp, repo, transport := setupT3WorkspaceBridge(t)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:61", Title: "Err Board", ChatID: -100555, ThreadID: 61, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r2-s1", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "Err Board", Status: "running",
		})
		// Inject PinChatMessage failure so OnThreadEventHandled returns handled=true + non-nil error.
		transport.mu.Lock()
		transport.pinErr = context.DeadlineExceeded
		transport.mu.Unlock()

		ev := domain.T3Event{
			EventID: "ev-r2-s1", EnvironmentID: "env-1", ThreadID: "th-r2-s1", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r2-s1",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Proceed?","options":["Yes","No"]}`)},
			},
		}
		if _, err := cp.IngestEvent(ctx, ev, nil); err != nil {
			t.Fatal(err)
		}
		if pending, _ := repo.PendingIncoming(ctx, 10); len(pending) != 0 {
			t.Fatalf("successful button delivery must suppress wake even when board pin fails: %+v", pending)
		}
		// Replay same event_id -> still deduplicated and 0 wakes!
		if dup, err := cp.IngestEvent(ctx, ev, nil); err != nil || !dup {
			t.Fatalf("expected deduplicated replay, got dup=%v err=%v", dup, err)
		}
		if pending, _ := repo.PendingIncoming(ctx, 10); len(pending) != 0 {
			t.Fatalf("replay must not queue wake: %+v", pending)
		}
	})

	t.Run("R2_S3_restart_recovers_pending_command_and_finalizes_message", func(t *testing.T) {
		repo := storage.NewMemory()
		transport := &fakeWorkspaceTransport{}
		now := time.Now().UTC()
		cp := t3.New(repo, repo, t3.Config{
			OwnerUserID:     7,
			OnlineThreshold: time.Minute,
			DispatchTimeout: 2 * time.Second,
			Now:             func() time.Time { return now },
		})
		bridge1 := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp, Repo: repo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp.AddObserver(bridge1)

		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-restart", Label: "Mac"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:62", Title: "Restart", ChatID: -100555, ThreadID: 62, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-restart", EnvironmentID: "env-restart", ConversationID: topicConv.ID, Title: "Restart", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-restart-wait", EnvironmentID: "env-restart", ThreadID: "th-restart", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-restart",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Deploy?","options":["Yes","No"]}`)},
			},
		}, nil)
		transport.mu.Lock()
		var qMsgID int64
		var yesData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 62 && m.markup != nil {
				qMsgID = m.msgID
				yesData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.mu.Unlock()

		// Host goes offline -> tap queues command.
		now = now.Add(5 * time.Minute)
		resQ, err := bridge1.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-q", UserID: 7, ChatID: -100555, ThreadID: 62, MessageID: qMsgID, Data: yesData})
		if err != nil || !strings.Contains(resQ.Text, "Queued") {
			t.Fatalf("expected queued answer: res=%+v err=%v", resQ, err)
		}

		// Simulate backend restart: fresh ControlPlane + fresh T3WorkspaceBridge with empty in-memory maps over same repo!
		cp2 := t3.New(repo, repo, t3.Config{OwnerUserID: 7, OnlineThreshold: time.Minute, DispatchTimeout: 2 * time.Second})
		bridge2 := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp2, Repo: repo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp2.AddObserver(bridge2)

		cmds, err := cp2.ClaimPendingCommands(ctx, "env-restart", 10)
		if err != nil || len(cmds) != 1 {
			t.Fatalf("expected 1 persisted queued command after restart, got %+v err=%v", cmds, err)
		}
		if err := cp2.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
			EnvironmentID: "env-restart", OK: true, Result: json.RawMessage(`{"accepted":true}`),
		}); err != nil {
			t.Fatal(err)
		}

		transport.mu.Lock()
		finalized := false
		for _, e := range transport.editedMessages {
			if e.msgID == qMsgID && strings.Contains(e.text, "✓ Answered: Yes") && e.markup != nil && len(e.markup.InlineKeyboard) == 0 {
				finalized = true
			}
		}
		transport.mu.Unlock()
		if !finalized {
			t.Fatal("expected restarted bridge to finalize queued question message and clear keyboard on command completion")
		}
	})

	t.Run("R2_S4_missing_thread_expires_button_without_dispatch", func(t *testing.T) {
		bridge, cp, _, transport := setupT3WorkspaceBridge(t)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-missing", Label: "Mac"})
		// Post buttons directly without persisting thread in store.
		if _, err := bridge.OnThreadEventHandled(ctx, domain.T3Event{
			EventID: "ev-orphan", EnvironmentID: "env-missing", ThreadID: "th-does-not-exist",
			ConversationID: "telegram:group:-100555:topic:63", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-orphan",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Orphan?","options":["Yes"]}`)},
			},
		}); err != nil {
			t.Fatal(err)
		}
		transport.mu.Lock()
		var qMsgID int64
		var cbData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 63 && m.markup != nil {
				qMsgID = m.msgID
				cbData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.mu.Unlock()

		res, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-orphan", UserID: 7, ChatID: -100555, ThreadID: 63, MessageID: qMsgID, Data: cbData})
		if err != nil || !strings.Contains(strings.ToLower(res.Text), "no longer pending") {
			t.Fatalf("missing thread must expire button gracefully, got res=%+v err=%v", res, err)
		}
		if cmds, _ := cp.ClaimPendingCommands(ctx, "env-missing", 10); len(cmds) != 0 {
			t.Fatalf("missing thread must not dispatch thread_respond: %+v", cmds)
		}
	})

	t.Run("R2_S6_delayed_respond_ack_after_completion_keeps_pr_prefix", func(t *testing.T) {
		_, cp, repo, transport := setupT3WorkspaceBridge(t)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:64", Title: "Delayed Ack", ChatID: -100555, ThreadID: 64, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-delayed-ack", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "Delayed Ack", Status: "running",
		})
		cmd, err := cp.EnqueueCommand(ctx, "cmd-delayed-resp", "env-1", domain.T3ActionThreadRespond, map[string]any{
			"thread_id": "th-delayed-ack", "request_id": "req-1", "answers": map[string]any{"q": "Yes"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Thread completes with PR -> topic becomes 👀 Delayed Ack.
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-delayed-done", EnvironmentID: "env-1", ThreadID: "th-delayed-ack",
			Kind: domain.T3EventThreadCompleted, PRURL: "https://github.com/org/repo/pull/64",
		}, nil)
		// Delayed thread_respond ack arrives afterwards -> MUST NOT flip 👀 back to 🔄!
		if err := cp.RecordCommandResult(ctx, cmd.ID, t3.CommandResultRequest{
			EnvironmentID: "env-1", OK: true, Result: json.RawMessage(`{"accepted":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		transport.mu.Lock()
		lastTopic := transport.editedTopics[len(transport.editedTopics)-1].name
		transport.mu.Unlock()
		if lastTopic != "👀 Delayed Ack" {
			t.Fatalf("delayed thread_respond ack overwrote completed PR prefix: got %q", lastTopic)
		}
	})

	t.Run("R2_S7_pre_change_legacy_row_without_general_row_upgrades_title_from_chat", func(t *testing.T) {
		_, cp, repo, transport := setupT3WorkspaceBridge(t)
		// Pre-change DB row: Title="OpenAlbert Workspace" WITHOUT TitleFromChat flag, and NO topic:0 row!
		if err := repo.SaveConversation(ctx, domain.Conversation{
			ID: "telegram:group:-100555:topic:65", Title: "OpenAlbert Workspace", ChatID: -100555, ThreadID: 65, OwnerID: 7,
		}); err != nil {
			t.Fatal(err)
		}
		// New incoming Telegram message identifies "OpenAlbert Workspace" as a chat-title fallback.
		if _, err := repo.AcceptIncoming(ctx, domain.Incoming{
			ID: "tg-legacy-upgrade",
			Conversation: domain.Conversation{
				ID: "telegram:group:-100555:topic:65", Title: "OpenAlbert Workspace", TitleFromChat: true, ChatID: -100555, ThreadID: 65, OwnerID: 7,
			},
			Text:      "Launch task",
			CreatedAt: time.Now().UTC(),
		}, 2); err != nil {
			t.Fatal(err)
		}
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-legacy", EnvironmentID: "env-1", ConversationID: "telegram:group:-100555:topic:65", Title: "Legacy Fix", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-legacy-launch", EnvironmentID: "env-1", ThreadID: "th-legacy", Kind: domain.T3EventThreadLaunched, Title: "Legacy Fix",
		}, nil)
		transport.mu.Lock()
		defer transport.mu.Unlock()
		if len(transport.editedTopics) != 1 || transport.editedTopics[0].name != "🔄 Legacy Fix" {
			t.Fatalf("expected legacy topic to use thread title '🔄 Legacy Fix', got %+v", transport.editedTopics)
		}
	})

	t.Run("R2_S8_edit_failure_on_multi_question_recovers_on_retry_tap", func(t *testing.T) {
		bridge, cp, repo, transport := setupT3WorkspaceBridge(t)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:66", Title: "Edit Retry", ChatID: -100555, ThreadID: 66, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-s8", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "Edit Retry", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-s8-wait", EnvironmentID: "env-1", ThreadID: "th-s8", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-s8",
				Questions: []json.RawMessage{
					json.RawMessage(`{"id":"q1","question":"First?","options":["One","Two"]}`),
					json.RawMessage(`{"id":"q2","question":"Second?","options":["Alpha","Beta"]}`),
				},
			},
		}, nil)
		transport.mu.Lock()
		var qMsgID int64
		var q0Opt0 string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 66 && m.markup != nil {
				qMsgID = m.msgID
				q0Opt0 = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		// Fail the EditMessageText when advancing Q1 -> Q2.
		transport.editErr = context.DeadlineExceeded
		transport.mu.Unlock()

		// 1st tap returns error so owner knows the message wasn't updated.
		if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-s8-1", UserID: 7, ChatID: -100555, ThreadID: 66, MessageID: qMsgID, Data: q0Opt0}); err == nil {
			t.Fatal("expected edit error to be returned on failed Q1->Q2 transition")
		}
		// Owner taps old Q1 button again -> bridge re-renders persisted Q2 instead of stranding!
		resRecover, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-s8-2", UserID: 7, ChatID: -100555, ThreadID: 66, MessageID: qMsgID, Data: q0Opt0})
		if err != nil || !strings.Contains(resRecover.Text, "2/2") {
			t.Fatalf("expected recovery re-render of question 2/2, got res=%+v err=%v", resRecover, err)
		}
		transport.mu.Lock()
		foundQ2 := false
		for _, e := range transport.editedMessages {
			if e.msgID == qMsgID && strings.Contains(e.text, "(2/2) Second?") {
				foundQ2 = true
			}
		}
		transport.mu.Unlock()
		if !foundQ2 {
			t.Fatal("expected Q2 re-rendered into Telegram message on retry tap")
		}
	})

	t.Run("R3_S1_checkpoint_failure_after_button_send_still_suppresses_wake", func(t *testing.T) {
		mem := storage.NewMemory()
		flakyRepo := &failNthCheckpointRepo{Memory: mem, failOnPrefix: "t3/cb/a/", failNth: 2}
		transport := &fakeWorkspaceTransport{}
		cp := t3.New(mem, flakyRepo, t3.Config{OwnerUserID: 7, DispatchTimeout: 2 * time.Second})
		bridge := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp, Repo: flakyRepo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp.AddObserver(bridge)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:71", Title: "PostSend Err", ChatID: -100555, ThreadID: 71, OwnerID: 7}
		_ = mem.SaveConversation(ctx, topicConv)
		_, _ = mem.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r3-s1", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "PostSend Err", Status: "running",
		})
		if _, err := cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-r3-s1", EnvironmentID: "env-1", ThreadID: "th-r3-s1", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r3-s1",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Go?","options":["Yes","No"]}`)},
			},
		}, nil); err != nil {
			t.Fatal(err)
		}
		if pending, _ := mem.PendingIncoming(ctx, 10); len(pending) != 0 {
			t.Fatalf("checkpoint error after Telegram send must NOT queue LLM wake: %+v", pending)
		}
	})

	t.Run("R3_S3_replay_of_committed_respond_result_finalizes_buttons_after_restart", func(t *testing.T) {
		repo := storage.NewMemory()
		transport := &fakeWorkspaceTransport{}
		now := time.Now().UTC()
		cp := t3.New(repo, repo, t3.Config{OwnerUserID: 7, OnlineThreshold: time.Minute, DispatchTimeout: 2 * time.Second, Now: func() time.Time { return now }})
		bridge1 := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp, Repo: repo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp.AddObserver(bridge1)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-r3-s3", Label: "Mac"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:72", Title: "CrashReplay", ChatID: -100555, ThreadID: 72, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r3-s3", EnvironmentID: "env-r3-s3", ConversationID: topicConv.ID, Title: "CrashReplay", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-r3-s3-wait", EnvironmentID: "env-r3-s3", ThreadID: "th-r3-s3", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r3-s3",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Merge?","options":["Yes","No"]}`)},
			},
		}, nil)
		transport.mu.Lock()
		var qMsgID int64
		var yesData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 72 && m.markup != nil {
				qMsgID = m.msgID
				yesData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.mu.Unlock()

		// Queue while offline.
		now = now.Add(5 * time.Minute)
		if _, err := bridge1.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-r3-s3", UserID: 7, ChatID: -100555, ThreadID: 72, MessageID: qMsgID, Data: yesData}); err != nil {
			t.Fatal(err)
		}
		cmds, _ := cp.ClaimPendingCommands(ctx, "env-r3-s3", 10)
		// Commit command result WITHOUT any observer attached (simulates crash between DB commit and observer run!).
		cpNoObs := t3.New(repo, repo, t3.Config{OwnerUserID: 7, OnlineThreshold: time.Minute, DispatchTimeout: 2 * time.Second})
		if err := cpNoObs.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
			EnvironmentID: "env-r3-s3", OK: true, Result: json.RawMessage(`{"accepted":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		// Restart with fresh ControlPlane + Bridge; host retries the same result POST -> transitioned=false, MUST still finalize!
		cp2 := t3.New(repo, repo, t3.Config{OwnerUserID: 7, OnlineThreshold: time.Minute, DispatchTimeout: 2 * time.Second})
		bridge2 := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp2, Repo: repo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp2.AddObserver(bridge2)
		if err := cp2.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
			EnvironmentID: "env-r3-s3", OK: true, Result: json.RawMessage(`{"accepted":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		transport.mu.Lock()
		finalized := false
		for _, e := range transport.editedMessages {
			if e.msgID == qMsgID && strings.Contains(e.text, "✓ Answered: Yes") && e.markup != nil && len(e.markup.InlineKeyboard) == 0 {
				finalized = true
			}
		}
		transport.mu.Unlock()
		if !finalized {
			t.Fatal("replayed thread_respond result after crash must finalize Telegram buttons")
		}
	})

	t.Run("R3_S6_completion_during_final_edit_is_not_overwritten_by_running_prefix", func(t *testing.T) {
		repo := storage.NewMemory()
		baseTransport := &fakeWorkspaceTransport{}
		cp := t3.New(repo, repo, t3.Config{OwnerUserID: 7, DispatchTimeout: 2 * time.Second})
		var bridge *T3WorkspaceBridge
		hookTransport := &hookEditTransport{
			fakeWorkspaceTransport: baseTransport,
			onEdit: func(msgID int64) {
				// Simulate thread_completed arriving mid-EditMessageText!
				_, _ = cp.IngestEvent(ctx, domain.T3Event{
					EventID: "ev-r3-s6-mid-done", EnvironmentID: "env-r3-s6", ThreadID: "th-r3-s6",
					Kind: domain.T3EventThreadCompleted, PRURL: "https://github.com/org/repo/pull/73",
				}, nil)
			},
		}
		bridge = NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp, Repo: repo, Transport: hookTransport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp.AddObserver(bridge)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-r3-s6", Label: "Mac"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:73", Title: "MidEdit", ChatID: -100555, ThreadID: 73, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r3-s6", EnvironmentID: "env-r3-s6", ConversationID: topicConv.ID, Title: "MidEdit", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-r3-s6-wait", EnvironmentID: "env-r3-s6", ThreadID: "th-r3-s6", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r3-s6",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Ship?","options":["Yes"]}`)},
			},
		}, nil)
		baseTransport.mu.Lock()
		var qMsgID int64
		var yesData string
		for _, m := range baseTransport.sentMessages {
			if m.conv.ThreadID == 73 && m.markup != nil {
				qMsgID = m.msgID
				yesData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		baseTransport.mu.Unlock()
		hookTransport.targetMsgID = qMsgID

		wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-r3-s6")
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()
		go func() {
			<-wakeup
			cmds, _ := cp.ClaimPendingCommands(ctx, "env-r3-s6", 10)
			if len(cmds) == 1 {
				_ = cp.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
					EnvironmentID: "env-r3-s6", OK: true, Result: json.RawMessage(`{"accepted":true}`),
				})
			}
		}()

		if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-r3-s6", UserID: 7, ChatID: -100555, ThreadID: 73, MessageID: qMsgID, Data: yesData}); err != nil {
			t.Fatal(err)
		}
		baseTransport.mu.Lock()
		lastTopic := baseTransport.editedTopics[len(baseTransport.editedTopics)-1].name
		baseTransport.mu.Unlock()
		if lastTopic != "👀 MidEdit" {
			t.Fatalf("completion during finalization edit was overwritten by stale 🔄: got %q", lastTopic)
		}
	})

	t.Run("R3_S8_failed_multiselect_edit_retry_refreshes_without_double_toggle", func(t *testing.T) {
		bridge, cp, repo, transport := setupT3WorkspaceBridge(t)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:74", Title: "MultiErr", ChatID: -100555, ThreadID: 74, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r3-s8", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "MultiErr", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-r3-s8-wait", EnvironmentID: "env-1", ThreadID: "th-r3-s8", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r3-s8",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"checks","question":"Checks?","is_multi_select":true,"options":["Unit","Lint"]}`)},
			},
		}, nil)
		transport.mu.Lock()
		var qMsgID int64
		var unitData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 74 && m.markup != nil {
				qMsgID = m.msgID
				unitData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.editErr = context.DeadlineExceeded
		transport.mu.Unlock()

		// 1st tap persists selection [0] ("Unit"), edit fails -> sets NeedsRefresh=true.
		if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-m1", UserID: 7, ChatID: -100555, ThreadID: 74, MessageID: qMsgID, Data: unitData}); err == nil {
			t.Fatal("expected edit error on multi-select toggle")
		}
		// 2nd tap retries edit -> MUST render "✓ Unit" (NOT undo selection back to "○ Unit")!
		resRefresh, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-m2", UserID: 7, ChatID: -100555, ThreadID: 74, MessageID: qMsgID, Data: unitData})
		if err != nil || !strings.Contains(resRefresh.Text, "Refreshed") {
			t.Fatalf("expected refresh on retry tap, got res=%+v err=%v", resRefresh, err)
		}
		transport.mu.Lock()
		lastEdit := transport.editedMessages[len(transport.editedMessages)-1]
		transport.mu.Unlock()
		if lastEdit.markup == nil || lastEdit.markup.InlineKeyboard[0][0].Text != "✓ Unit" {
			t.Fatalf("retry tap undid persisted selection instead of refreshing: %+v", lastEdit.markup)
		}
	})

	t.Run("R3_S9_failed_final_edit_repairs_on_later_tap_without_redispatch", func(t *testing.T) {
		bridge, cp, repo, transport := setupT3WorkspaceBridge(t)
		_, _ = cp.RegisterHost(ctx, t3.RegisterHostRequest{EnvironmentID: "env-r3-s9", Label: "Mac"})
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:75", Title: "FinalEditErr", ChatID: -100555, ThreadID: 75, OwnerID: 7}
		_ = repo.SaveConversation(ctx, topicConv)
		_, _ = repo.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r3-s9", EnvironmentID: "env-r3-s9", ConversationID: topicConv.ID, Title: "FinalEditErr", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-r3-s9-wait", EnvironmentID: "env-r3-s9", ThreadID: "th-r3-s9", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r3-s9",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Deploy?","options":["Yes","No"]}`)},
			},
		}, nil)
		transport.mu.Lock()
		var qMsgID int64
		var yesData string
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 75 && m.markup != nil {
				qMsgID = m.msgID
				yesData = m.markup.InlineKeyboard[0][0].CallbackData
			}
		}
		transport.editErr = context.DeadlineExceeded
		transport.mu.Unlock()

		wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-r3-s9")
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()
		dispatchCount := 0
		go func() {
			<-wakeup
			cmds, _ := cp.ClaimPendingCommands(ctx, "env-r3-s9", 10)
			if len(cmds) == 1 {
				dispatchCount++
				_ = cp.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
					EnvironmentID: "env-r3-s9", OK: true, Result: json.RawMessage(`{"accepted":true}`),
				})
			}
		}()

		// 1st tap dispatches command (succeeds), but final Telegram edit fails -> FinalEdited stays false.
		if _, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-f1", UserID: 7, ChatID: -100555, ThreadID: 75, MessageID: qMsgID, Data: yesData}); err != nil {
			t.Fatal(err)
		}
		// 2nd tap retries final edit from saved summary WITHOUT dispatching another command!
		res2, err := bridge.HandleCallback(ctx, domain.CallbackQuery{ID: "cb-f2", UserID: 7, ChatID: -100555, ThreadID: 75, MessageID: qMsgID, Data: yesData})
		if err != nil || !strings.Contains(res2.Text, "Already answered: Yes") {
			t.Fatalf("expected Already answered on repair tap, got res=%+v err=%v", res2, err)
		}
		if dispatchCount != 1 {
			t.Fatalf("repair tap must not dispatch another command; dispatchCount=%d", dispatchCount)
		}
		transport.mu.Lock()
		repaired := false
		for _, e := range transport.editedMessages {
			if e.msgID == qMsgID && strings.Contains(e.text, "✓ Answered: Yes") && e.markup != nil && len(e.markup.InlineKeyboard) == 0 {
				repaired = true
			}
		}
		transport.mu.Unlock()
		if !repaired {
			t.Fatal("expected second tap to repair failed final edit and clear keyboard")
		}
	})

	t.Run("R6_S6_completion_during_presend_checkpoint_skips_obsolete_buttons", func(t *testing.T) {
		mem := storage.NewMemory()
		transport := &fakeWorkspaceTransport{}
		cp := t3.New(mem, mem, t3.Config{OwnerUserID: 7, DispatchTimeout: 2 * time.Second})
		hookRepo := &hookCheckpointRepo{
			Memory: mem,
			prefix: "t3/cb/a/",
			onSave: func() {
				// Simulate thread_completed arriving during the pre-send checkpoint write!
				_, _ = cp.IngestEvent(ctx, domain.T3Event{
					EventID: "ev-r6-s6-done", EnvironmentID: "env-1", ThreadID: "th-r6-s6",
					Kind: domain.T3EventThreadCompleted, PRURL: "https://github.com/org/repo/pull/76",
				}, nil)
			},
		}
		bridge := NewT3WorkspaceBridge(T3WorkspaceBridgeConfig{
			ControlPlane: cp, Repo: hookRepo, Transport: transport, OwnerUserID: 7, WorkspaceGroupID: -100555,
		})
		cp.AddObserver(bridge)
		topicConv := domain.Conversation{ID: "telegram:group:-100555:topic:76", Title: "PreSendRace", ChatID: -100555, ThreadID: 76, OwnerID: 7}
		_ = mem.SaveConversation(ctx, topicConv)
		_, _ = mem.UpsertT3Thread(ctx, domain.T3ThreadPatch{
			ThreadID: "th-r6-s6", EnvironmentID: "env-1", ConversationID: topicConv.ID, Title: "PreSendRace", Status: "running",
		})
		_, _ = cp.IngestEvent(ctx, domain.T3Event{
			EventID: "ev-r6-s6-wait", EnvironmentID: "env-1", ThreadID: "th-r6-s6", Kind: domain.T3EventThreadWaitingInput,
			PendingRequest: &domain.T3PendingRequest{
				RequestID: "req-r6-s6",
				Questions: []json.RawMessage{json.RawMessage(`{"id":"q","question":"Obsolete?","options":["Yes","No"]}`)},
			},
		}, nil)
		transport.mu.Lock()
		defer transport.mu.Unlock()
		for _, m := range transport.sentMessages {
			if m.conv.ThreadID == 76 && strings.Contains(m.text, "Obsolete?") {
				t.Fatalf("obsolete question buttons must not be sent after completion during checkpoint write: %+v", m)
			}
		}
	})
}

type hookCheckpointRepo struct {
	*storage.Memory
	mu     sync.Mutex
	prefix string
	fired  bool
	onSave func()
}

func (r *hookCheckpointRepo) SaveCheckpoint(ctx context.Context, key string, data json.RawMessage) error {
	if err := r.Memory.SaveCheckpoint(ctx, key, data); err != nil {
		return err
	}
	r.mu.Lock()
	shouldFire := !r.fired && strings.HasPrefix(key, r.prefix) && r.onSave != nil
	if shouldFire {
		r.fired = true
	}
	r.mu.Unlock()
	if shouldFire {
		r.onSave()
	}
	return nil
}

type failNthCheckpointRepo struct {
	*storage.Memory
	mu           sync.Mutex
	failOnPrefix string
	failNth      int
	seen         int
}

func (r *failNthCheckpointRepo) SaveCheckpoint(ctx context.Context, key string, data json.RawMessage) error {
	r.mu.Lock()
	if strings.HasPrefix(key, r.failOnPrefix) {
		r.seen++
		if r.seen == r.failNth {
			r.mu.Unlock()
			return context.DeadlineExceeded
		}
	}
	r.mu.Unlock()
	return r.Memory.SaveCheckpoint(ctx, key, data)
}

type hookEditTransport struct {
	*fakeWorkspaceTransport
	targetMsgID int64
	fired       bool
	onEdit      func(msgID int64)
}

func (h *hookEditTransport) EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, replyMarkup *domain.InlineKeyboardMarkup) error {
	if messageID == h.targetMsgID && !h.fired && h.onEdit != nil {
		h.fired = true
		h.onEdit(messageID)
	}
	return h.fakeWorkspaceTransport.EditMessageText(ctx, chatID, messageID, text, replyMarkup)
}
