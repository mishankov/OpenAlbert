package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/mcpclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/session"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/tools"
)

func TestBoundHistoryForGatewayStripsOldImagesThenOldTurns(t *testing.T) {
	bigImg := "data:image/png;base64," + strings.Repeat("A", 800)
	msgs := []serverclient.Message{
		{Role: "user", Content: "turn 1 prompt " + strings.Repeat("x", 200), Images: []string{bigImg}},
		{Role: "assistant", ToolCalls: []serverclient.ToolCall{{ID: "tc-1", Name: "codemode", Arguments: json.RawMessage(`{"code":"return 1"}`)}}},
		{Role: "tool", ToolCallID: "tc-1", Content: `{"output":1}`},
		{Role: "assistant", Content: "turn 1 answer " + strings.Repeat("y", 200)},
		{Role: "user", Content: "turn 2 prompt"},
		{Role: "assistant", Content: "turn 2 answer"},
		{Role: "user", Content: "current turn prompt", Images: []string{bigImg}},
	}

	// Budget large enough to keep turn 1 text once its old image is stripped, while keeping current turn's image intact.
	bounded, err := BoundHistoryForGateway("sys", msgs, 1900)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != len(msgs)+1 {
		t.Fatalf("expected all turns preserved after image strip, got %d messages", len(bounded))
	}
	if len(bounded[1].Images) != 0 {
		t.Fatalf("expected turn 1 image stripped, still has %d", len(bounded[1].Images))
	}
	if len(bounded[len(bounded)-1].Images) != 1 {
		t.Fatalf("current turn image must be preserved")
	}

	// Tighter budget drops turn 1 completely as a unit (never leaving orphan tool results) while keeping turn 2 + current turn.
	tight, err := BoundHistoryForGateway("sys", msgs, 1300)
	if err != nil {
		t.Fatal(err)
	}
	if len(tight) < 2 || tight[0].Role != "system" {
		t.Fatalf("unexpected tight history: %+v", tight)
	}
	if tight[1].Role != "user" {
		t.Fatalf("first retained message after system must be a user turn boundary, got %+v", tight[1])
	}
	for _, m := range tight {
		if m.ToolCallID == "tc-1" {
			t.Fatalf("orphan tool message from dropped turn leaked into history: %+v", tight)
		}
	}
	if tight[len(tight)-1].Content != "current turn prompt" {
		t.Fatalf("current turn was dropped: %+v", tight)
	}
}

func TestRunTurnCancelDuringToolPersistsToolCancellationResults(t *testing.T) {
	var (
		mu             sync.Mutex
		secondTurnReqs []serverclient.CompleteRequest
		callNum        int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/llm/complete" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		var req serverclient.CompleteRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		n := callNum
		callNum++
		if n >= 1 {
			secondTurnReqs = append(secondTurnReqs, req)
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		send := func(ev serverclient.StreamEvent) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		if n == 0 {
			args, _ := json.Marshal(map[string]string{"code": `await tools.bash({ command: "sleep 10" }); return 1;`})
			send(serverclient.StreamEvent{
				Type: "done",
				Completion: &serverclient.Completion{
					Model: "proxy/gpt-5.4",
					Message: serverclient.Message{
						Role: "assistant",
						ToolCalls: []serverclient.ToolCall{
							{ID: "call-1", Name: "codemode", Arguments: args},
							{ID: "call-2", Name: "codemode", Arguments: args},
						},
					},
				},
			})
			return
		}
		send(serverclient.StreamEvent{
			Type: "done",
			Completion: &serverclient.Completion{
				Model:   "proxy/gpt-5.4",
				Message: serverclient.Message{Role: "assistant", Content: "Recovered cleanly."},
			},
		})
	}))
	defer srv.Close()

	sess := &Session{
		ID:            "sess-cancel",
		CWD:           t.TempDir(),
		Model:         "proxy/gpt-5.4",
		Mode:          ModeFullAccess,
		Client:        serverclient.New(srv.URL, "tok", 10*time.Second),
		Store:         session.NewStore(t.TempDir()),
		LocalTools:    &tools.Local{CWD: t.TempDir()},
		MCP:           &mcpclient.Manager{},
		SyncInspector: false,
		MaxSteps:      5,
	}

	turnCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	stopReason, err := sess.RunTurn(turnCtx, "Run slow command", nil, nil)
	if err != nil || stopReason != "cancelled" {
		t.Fatalf("expected cancelled stopReason, got %q %v", stopReason, err)
	}

	// Next turn must send valid history where every assistant tool_call_id has a matching role="tool" message.
	stopReason, err = sess.RunTurn(context.Background(), "Continue after cancel", nil, nil)
	if err != nil || stopReason != "end_turn" {
		t.Fatalf("second turn failed: %q %v", stopReason, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(secondTurnReqs) != 1 {
		t.Fatalf("expected 1 request in second turn, got %d", len(secondTurnReqs))
	}
	seenToolIDs := map[string]bool{}
	for _, m := range secondTurnReqs[0].Messages {
		if m.Role == "tool" {
			seenToolIDs[m.ToolCallID] = true
		}
	}
	if !seenToolIDs["call-1"] || !seenToolIDs["call-2"] {
		t.Fatalf("missing persisted cancellation tool results in follow-up history: %+v", secondTurnReqs[0].Messages)
	}
}

func TestFlushSyncRetriesTransientFailureAndDrainsBeforeClose(t *testing.T) {
	var (
		syncAttempts atomic.Int32
		mu           sync.Mutex
		syncedKeys   []string
		events       []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sync"):
			attempt := syncAttempts.Add(1)
			if attempt == 1 {
				http.Error(w, "temporary outage", http.StatusServiceUnavailable)
				return
			}
			var req serverclient.SyncRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			for _, m := range req.Messages {
				syncedKeys = append(syncedKeys, m.Key)
			}
			events = append(events, "sync")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/close"):
			mu.Lock()
			events = append(events, "close")
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"closed": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store := session.NewStore(t.TempDir())
	sess := &Session{
		ID:            "sess-retry",
		CWD:           t.TempDir(),
		Title:         "Retry Test",
		Client:        serverclient.New(srv.URL, "tok", 5*time.Second),
		Store:         store,
		SyncInspector: true,
		PendingSyncMessages: []serverclient.SyncMessage{
			{Key: "t1/user", Message: serverclient.Message{Role: "user", Content: "Remember this preference"}},
		},
	}

	// First flush fails transiently and must retain pending messages.
	if err := sess.FlushSync(context.Background()); err == nil {
		t.Fatal("expected first FlushSync to fail on 503")
	}
	if len(sess.PendingSyncMessages) != 1 {
		t.Fatalf("pending messages should be retained after failure, got %d", len(sess.PendingSyncMessages))
	}

	// Close drains pending messages before calling /close and persists Closed=true.
	sess.Close(context.Background())
	if len(sess.PendingSyncMessages) != 0 || !sess.Closed {
		t.Fatalf("expected pending drained and Closed=true, got pending=%d closed=%v", len(sess.PendingSyncMessages), sess.Closed)
	}
	rec, ok, err := store.Load("sess-retry")
	if err != nil || !ok || !rec.Closed || len(rec.PendingSyncMessages) != 0 {
		t.Fatalf("unexpected persisted record after close: %+v ok=%v err=%v", rec, ok, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(syncedKeys) != 1 || syncedKeys[0] != "t1/user" {
		t.Fatalf("expected retried message synced, got %+v", syncedKeys)
	}
	if strings.Join(events, ",") != "sync,close" {
		t.Fatalf("expected sync before close, got %+v", events)
	}
}

func TestCloseSkipsRemoteCloseWhenFinalSyncFails(t *testing.T) {
	var closed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sync"):
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/close"):
			closed.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"closed": true})
		}
	}))
	defer srv.Close()

	store := session.NewStore(t.TempDir())
	sess := &Session{
		ID:            "sess-fail-close",
		CWD:           t.TempDir(),
		Client:        serverclient.New(srv.URL, "tok", 5*time.Second),
		Store:         store,
		SyncInspector: true,
		PendingSyncMessages: []serverclient.SyncMessage{
			{Key: "t1/user", Message: serverclient.Message{Role: "user", Content: "important"}},
		},
	}
	sess.Close(context.Background())
	if closed.Load() != 0 || sess.Closed || len(sess.PendingSyncMessages) != 1 {
		t.Fatalf("session must stay open when final sync fails: closedCalls=%d sess.Closed=%v pending=%d", closed.Load(), sess.Closed, len(sess.PendingSyncMessages))
	}
}

func TestSyncInspectorFalseOmitsQueueAndServerSessionIDs(t *testing.T) {
	var (
		mu       sync.Mutex
		reqSids  []string
		toolSids []string
		syncHits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/llm/complete":
			var req serverclient.CompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			reqSids = append(reqSids, req.SessionID)
			step := len(reqSids)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			send := func(ev serverclient.StreamEvent) {
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
			if step == 1 {
				args, _ := json.Marshal(map[string]string{"code": `return await tools.memory_search({ query: "x" });`})
				send(serverclient.StreamEvent{
					Type: "done",
					Completion: &serverclient.Completion{
						Model: "proxy/gpt-5.4",
						Message: serverclient.Message{
							Role:      "assistant",
							ToolCalls: []serverclient.ToolCall{{ID: "c1", Name: "codemode", Arguments: args}},
						},
					},
				})
				return
			}
			send(serverclient.StreamEvent{
				Type: "done",
				Completion: &serverclient.Completion{
					Model:   "proxy/gpt-5.4",
					Message: serverclient.Message{Role: "assistant", Content: "Done."},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/agent/tools/"):
			var req serverclient.ToolRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			toolSids = append(toolSids, req.SessionID)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.ToolResult{Output: json.RawMessage(`{"hits":[]}`)})
		case strings.HasSuffix(r.URL.Path, "/sync"):
			mu.Lock()
			syncHits++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer srv.Close()

	store := session.NewStore(t.TempDir())
	sess := &Session{
		ID:            "sess-nosync",
		CWD:           t.TempDir(),
		Model:         "proxy/gpt-5.4",
		Mode:          ModeFullAccess,
		Client:        serverclient.New(srv.URL, "tok", 5*time.Second),
		Store:         store,
		LocalTools:    &tools.Local{CWD: t.TempDir()},
		MCP:           &mcpclient.Manager{},
		ServerContext: serverclient.AgentContext{Tools: []serverclient.Tool{{Name: "memory_search", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		SyncInspector: false,
		MaxSteps:      5,
	}
	stopReason, err := sess.RunTurn(context.Background(), "Private prompt", nil, nil)
	if err != nil || stopReason != "end_turn" {
		t.Fatalf("RunTurn failed: %q %v", stopReason, err)
	}
	if len(sess.PendingSyncMessages) != 0 || len(sess.PendingSyncTraces) != 0 {
		t.Fatalf("SyncInspector=false must not queue pending sync items: msgs=%d traces=%d", len(sess.PendingSyncMessages), len(sess.PendingSyncTraces))
	}
	mu.Lock()
	defer mu.Unlock()
	if syncHits != 0 {
		t.Fatalf("SyncInspector=false hit /sync %d times", syncHits)
	}
	for _, sid := range append(reqSids, toolSids...) {
		if sid != "" {
			t.Fatalf("SyncInspector=false leaked SessionID=%q to backend", sid)
		}
	}
}

func TestOversizedCurrentTurnAndSyncRecordsAreBounded(t *testing.T) {
	hugeImg := "data:image/png;base64," + strings.Repeat("B", 3<<20)
	sess := &Session{
		ID:            "sess-big",
		CWD:           t.TempDir(),
		Store:         session.NewStore(t.TempDir()),
		SyncInspector: true,
	}
	// Two 3 MiB images exceed maxSinglePromptBytes and must fail before persisting.
	if _, err := sess.RunTurn(context.Background(), "inspect", []string{hugeImg, hugeImg}, nil); err == nil {
		t.Fatal("expected oversized two-image prompt to be rejected before persisting")
	}
	if len(sess.Messages) != 0 || len(sess.PendingSyncMessages) != 0 {
		t.Fatalf("rejected oversized prompt must not be persisted: msgs=%d pending=%d", len(sess.Messages), len(sess.PendingSyncMessages))
	}

	// BoundHistoryForGateway guarantees output <= maxBytes even for an oversized single current turn
	// including massive Reasoning and opaque ResponsesItems.
	bounded, err := BoundHistoryForGateway("sys", []serverclient.Message{
		{Role: "user", Content: "inspect", Images: []string{hugeImg, hugeImg}},
		{
			Role:           "assistant",
			Reasoning:      strings.Repeat("R", 3<<20),
			ResponsesItems: []json.RawMessage{json.RawMessage(`{"blob":"` + strings.Repeat("Q", 4<<20) + `"}`)},
			Content:        strings.Repeat("C", 2<<20),
		},
	}, maxGatewayBytes)
	if err != nil {
		t.Fatalf("expected trimmable oversized turn to fit, got %v", err)
	}
	if raw, _ := json.Marshal(bounded); len(raw) > maxGatewayBytes {
		t.Fatalf("BoundHistoryForGateway exceeded maxGatewayBytes: %d > %d", len(raw), maxGatewayBytes)
	}

	// boundSyncMessage caps a huge single sync message even when JSON-escaping multiplies byte size (e.g. control bytes \x00 -> \u0000)
	// and strips NUL bytes (for PostgreSQL jsonb) as well as bounding 1 MiB identifiers.
	hugeID := strings.Repeat("i", 1<<20)
	escapedPayload := strings.Repeat("\x00", 512<<10)
	capped := boundSyncMessage(serverclient.SyncMessage{
		Key: hugeID,
		Message: serverclient.Message{
			Role:             "tool",
			TurnID:           hugeID,
			ToolCallID:       hugeID,
			ParentToolCallID: hugeID,
			Content:          "hello\x00world" + escapedPayload,
			Reasoning:        escapedPayload,
			Images:           []string{hugeImg},
		},
	}, maxSyncItemBytes)
	if raw, _ := json.Marshal(capped); len(raw) > maxSyncItemBytes || strings.Contains(string(raw), `\u0000`) {
		t.Fatalf("boundSyncMessage exceeded limit or kept NUL escape: len=%d limit=%d", len(raw), maxSyncItemBytes)
	}
	cappedTrace := boundSyncTrace(serverclient.Trace{
		ID:        hugeID,
		SessionID: hugeID,
		TurnID:    hugeID,
		CallID:    hugeID,
		Name:      hugeID,
		Input:     json.RawMessage(`{"nul":"a\u0000b","literal":"keep\\u0000text"}`),
		Output:    json.RawMessage(`{"y":"` + strings.Repeat("z", 1<<20) + `"}`),
	}, maxSyncItemBytes)
	rawTrace, _ := json.Marshal(cappedTrace)
	if len(rawTrace) > maxSyncItemBytes || strings.Contains(string(rawTrace), `"a\u0000b"`) || !strings.Contains(string(cappedTrace.Input), `keep\\u0000text`) {
		t.Fatalf("boundSyncTrace size/escape handling failed: len=%d input=%s", len(rawTrace), string(cappedTrace.Input))
	}
}

func TestFlushSyncRollsOverGenerationOnSessionClosed409(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/v1/coding/sessions/sess-roll/sync" {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "session_closed", "message": "Coding session is already closed"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	store := session.NewStore(t.TempDir())
	sess := &Session{
		ID:              "sess-roll",
		ServerSessionID: "sess-roll",
		Generation:      1,
		CWD:             t.TempDir(),
		Client:          serverclient.New(srv.URL, "tok", 5*time.Second),
		Store:           store,
		SyncInspector:   true,
		PendingSyncMessages: []serverclient.SyncMessage{
			{Key: "t1/user", Message: serverclient.Message{Role: "user", Content: "recover me"}},
		},
	}
	if err := sess.FlushSync(context.Background()); err != nil {
		t.Fatalf("FlushSync should roll over on 409 session_closed: %v", err)
	}
	if sess.ServerSessionID != "sess-roll-g2" || sess.Generation != 2 || len(sess.PendingSyncMessages) != 0 {
		t.Fatalf("expected rolled over generation -g2 with drained queue, got id=%q gen=%d pending=%d", sess.ServerSessionID, sess.Generation, len(sess.PendingSyncMessages))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[1] != "/api/v1/coding/sessions/sess-roll-g2/sync" {
		t.Fatalf("unexpected sync retry paths: %+v", paths)
	}
}

func TestRunTurnRecoversFromSessionClosed409OnCompleteStream(t *testing.T) {
	var (
		mu              sync.Mutex
		created         []string
		completeSessIDs []string
		syncPaths       []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/coding/sessions":
			var req struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			created = append(created, req.SessionID)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.URL.Path == "/api/v1/llm/complete":
			var req serverclient.CompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			completeSessIDs = append(completeSessIDs, req.SessionID)
			mu.Unlock()
			// Simulate server-side stale closure of g1 between user turns.
			if req.SessionID == "sess-stale" {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "session_closed", "message": "Coding session is already closed"}})
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			raw, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: req.Model, Message: serverclient.Message{Role: "assistant", Content: "Recovered on g2"}},
			})
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		case strings.HasSuffix(r.URL.Path, "/sync"):
			mu.Lock()
			syncPaths = append(syncPaths, r.URL.Path)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer srv.Close()

	sess := &Session{
		ID:               "sess-stale",
		ServerSessionID:  "sess-stale",
		Generation:       1,
		RemoteRegistered: true,
		CWD:              t.TempDir(),
		Model:            "proxy/gpt-5.4",
		Mode:             ModeDefault,
		Client:           serverclient.New(srv.URL, "tok", 5*time.Second),
		Store:            session.NewStore(t.TempDir()),
		SyncInspector:    true,
	}
	stopReason, err := sess.RunTurn(context.Background(), "Continue work", nil, nil)
	if err != nil || stopReason != "end_turn" {
		t.Fatalf("RunTurn failed to recover from 409 session_closed: stop=%q err=%v", stopReason, err)
	}
	if sess.ServerSessionID != "sess-stale-g2" || sess.Generation != 2 || !sess.IsRemoteRegistered() {
		t.Fatalf("expected rolled over generation -g2, got id=%q gen=%d reg=%v", sess.ServerSessionID, sess.Generation, sess.IsRemoteRegistered())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(completeSessIDs) != 2 || completeSessIDs[0] != "sess-stale" || completeSessIDs[1] != "sess-stale-g2" {
		t.Fatalf("unexpected complete session sequence: %+v", completeSessIDs)
	}
	if len(created) != 1 || created[0] != "sess-stale-g2" {
		t.Fatalf("expected lazy creation of g2 after rollover, got %+v", created)
	}
	if len(syncPaths) == 0 || syncPaths[len(syncPaths)-1] != "/api/v1/coding/sessions/sess-stale-g2/sync" {
		t.Fatalf("expected sync to target g2, got %+v", syncPaths)
	}
}
