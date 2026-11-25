package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
	"openalbert/apps/backend/internal/t3"
)

func setupT3Activities(t *testing.T) (*Activities, *storage.Memory, *t3.ControlPlane, domain.Conversation) {
	t.Helper()
	a, repo, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	cp := t3.New(repo, repo, t3.Config{
		OwnerUserID:     owner.OwnerID,
		OnlineThreshold: 3 * time.Minute,
		DispatchTimeout: 2 * time.Second,
	})
	a.T3 = cp
	return a, repo, cp, owner
}

func TestT3ToolsAuthorizationAndCodingAccess(t *testing.T) {
	a, _, cp, owner := setupT3Activities(t)
	ctx := context.Background()
	if _, err := cp.RegisterHost(ctx, t3.RegisterHostRequest{
		EnvironmentID: "env-1",
		Label:         "MacBook",
	}); err != nil {
		t.Fatal(err)
	}

	// Owner private chat: allowed & all 7 tools advertised.
	in := ToolInput{RunKey: "t3-auth", Conversation: owner, Call: domain.ToolCall{ID: "hosts", Name: "t3_hosts", Arguments: json.RawMessage(`{}`)}}
	if out, err := a.ToolCall(ctx, in); err != nil || strings.Contains(out, `"error"`) || !strings.Contains(out, `"MacBook"`) {
		t.Fatalf("owner private t3_hosts failed: %s %v", out, err)
	}
	count := 0
	for _, def := range a.toolsForTurn(Turn{Conversation: owner}) {
		if isT3Tool(def.Name) {
			count++
		}
	}
	if count != 7 {
		t.Fatalf("expected 7 t3 tools advertised in owner private chat, got %d", count)
	}

	// Owner background task (TaskID != "", Initiative=false): allowed & advertised.
	taskCount := 0
	for _, def := range a.toolsForTurn(Turn{Conversation: owner, Input: RunInput{TaskID: "task-1"}}) {
		if isT3Tool(def.Name) {
			taskCount++
		}
	}
	if taskCount != 7 {
		t.Fatalf("expected 7 t3 tools advertised in owner background task, got %d", taskCount)
	}
	taskIn := in
	taskIn.RunKey = "t3-task"
	taskIn.TaskID = "task-1"
	if out, err := a.ToolCall(ctx, taskIn); err != nil || strings.Contains(out, `"error"`) {
		t.Fatalf("owner background task t3_hosts failed: %s %v", out, err)
	}

	// ACP coding session: all 7 tools advertised & executable via ExecuteCodingTool.
	codingCtx, err := a.CodingContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	codingCount := 0
	for _, def := range codingCtx.Tools {
		if isT3Tool(def.Name) {
			codingCount++
		}
	}
	if codingCount != 7 {
		t.Fatalf("expected 7 t3 tools in CodingContext, got %d", codingCount)
	}
	codingRes, err := a.ExecuteCodingTool(ctx, CodingToolRequest{
		SessionID: "coding-1",
		TurnID:    "turn-1",
		CallID:    "call-1",
		Name:      "t3_hosts",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil || codingRes.Failed || !strings.Contains(string(codingRes.Output), `"MacBook"`) {
		t.Fatalf("ExecuteCodingTool t3_hosts failed: %+v err=%v", codingRes, err)
	}

	// Unauthorized contexts blocked even on checkpoint replay (same RunKey + Call.ID as owner call above!).
	a.WorkspaceGroupID = -100555
	ownerWorkspaceTopic := domain.Conversation{
		ID:       "telegram:group:-100555:topic:42",
		Title:    "Agent Work",
		ChatID:   -100555,
		ThreadID: 42,
		OwnerID:  owner.OwnerID,
		Private:  false,
	}
	nonOwnerWorkspaceTopic := domain.Conversation{
		ID:       "telegram:group:-100555:topic:42",
		Title:    "Agent Work",
		ChatID:   -100555,
		ThreadID: 42,
		OwnerID:  999,
		Private:  false,
	}

	// Owner Workspace Supergroup topic: all 7 tools advertised and executable!
	wsCount := 0
	for _, def := range a.toolsForTurn(Turn{Conversation: ownerWorkspaceTopic}) {
		if isT3Tool(def.Name) {
			wsCount++
		}
	}
	if wsCount != 7 {
		t.Fatalf("expected 7 t3 tools advertised in owner workspace topic, got %d", wsCount)
	}
	wsIn := in
	wsIn.RunKey = "t3-workspace-owner"
	wsIn.Conversation = ownerWorkspaceTopic
	if out, err := a.ToolCall(ctx, wsIn); err != nil || strings.Contains(out, `"error"`) || !strings.Contains(out, `"MacBook"`) {
		t.Fatalf("owner workspace t3_hosts failed: %s %v", out, err)
	}

	for _, tc := range []struct {
		name       string
		conv       domain.Conversation
		initiative bool
	}{
		{"group", domain.Conversation{ID: "group:-100", ChatID: -100, OwnerID: owner.OwnerID}, false},
		{"non_owner_workspace", nonOwnerWorkspaceTopic, false},
		{"workspace_initiative", ownerWorkspaceTopic, true},
		{"other_private", domain.Conversation{ID: "personal:999", Private: true, ChatID: 999, OwnerID: 999}, false},
		{"spoofed_private", domain.Conversation{ID: "spoof", Private: true, ChatID: 999, OwnerID: owner.OwnerID}, false},
		{"initiative", owner, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unauthIn := in
			unauthIn.Conversation = tc.conv
			unauthIn.Background = tc.initiative
			out, err := a.ToolCall(ctx, unauthIn)
			if err != nil || !strings.Contains(out, `"error"`) {
				t.Fatalf("expected error for %s, got %s %v", tc.name, out, err)
			}
			for _, def := range a.toolsForTurn(Turn{Conversation: tc.conv, Input: RunInput{Initiative: tc.initiative}}) {
				if isT3Tool(def.Name) {
					t.Fatalf("t3 tool %s advertised in %s", def.Name, tc.name)
				}
			}
		})
	}

	// Disabled T3 control plane: no tools advertised.
	a.T3 = nil
	for _, def := range a.toolsForTurn(Turn{Conversation: owner}) {
		if isT3Tool(def.Name) {
			t.Fatalf("disabled t3 tool %s advertised", def.Name)
		}
	}
}

func TestT3SevenToolsLifecycle(t *testing.T) {
	a, _, cp, owner := setupT3Activities(t)
	ctx := context.Background()

	// Register an online host and simulate a LAN bridge consuming SSE commands.
	host, err := cp.RegisterHost(ctx, t3.RegisterHostRequest{
		EnvironmentID: "env-lan",
		Label:         "MacBook Pro",
		Projects:      []domain.T3Project{{ID: "proj-oa", Title: "OpenAlbert", WorkspaceRoot: "/code/oa"}},
		Providers: []domain.T3Provider{{
			ProviderInstanceID: "ag-1",
			DriverKind:         "antigravity",
			DisplayName:        "Antigravity",
			Models:             []domain.T3Model{{ID: "gemini-4-neon", Label: "Gemini 4 Neon"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	wakeup, unsub, err := cp.SubscribeCommands(ctx, "env-lan")
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	// Helper simulating host executing the next command and posting its result.
	respondNext := func(fn func(cmd domain.T3Command) (bool, string, string)) {
		go func() {
			<-wakeup
			cmds, err := cp.ClaimPendingCommands(ctx, "env-lan", 10)
			if err != nil || len(cmds) != 1 {
				t.Errorf("claim: %+v err=%v", cmds, err)
				return
			}
			ok, resJSON, errText := fn(cmds[0])
			if err := cp.RecordCommandResult(ctx, cmds[0].ID, t3.CommandResultRequest{
				EnvironmentID: "env-lan",
				OK:            ok,
				Result:        json.RawMessage(resJSON),
				Error:         errText,
			}); err != nil {
				t.Errorf("record result: %v", err)
			}
		}()
	}

	// 1. t3_hosts
	out := tool(t, a, owner, "run-1", "c1", "t3_hosts", `{}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, host.HostID) || !strings.Contains(out, `"online":true`) {
		t.Fatalf("t3_hosts: %s", out)
	}

	// 2. t3_thread_launch (defaults conversation_id to owner.ID)
	respondNext(func(cmd domain.T3Command) (bool, string, string) {
		if cmd.Action != domain.T3ActionThreadLaunch || !strings.Contains(string(cmd.Payload), owner.ID) {
			t.Errorf("unexpected launch command: %+v", cmd)
		}
		return true, `{"thread_id":"th-100","link":"t3://thread/th-100","status":"running","branch":"feat/x"}`, ""
	})
	out = tool(t, a, owner, "run-1", "c2", "t3_thread_launch", `{
		"host_id":"`+host.HostID+`",
		"project_id":"proj-oa",
		"title":"Implement PR B",
		"message":"Build control plane",
		"model_selection":{"provider_instance_id":"ag-1","model":"gemini-4-neon"}
	}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"th-100"`) {
		t.Fatalf("t3_thread_launch: %s", out)
	}

	// 3. t3_threads_list
	out = tool(t, a, owner, "run-1", "c3", "t3_threads_list", `{"host_id":"`+host.HostID+`","conversation_id":"`+owner.ID+`"}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"th-100"`) {
		t.Fatalf("t3_threads_list: %s", out)
	}

	// 4. t3_thread_send (default mode=auto and explicit mode=steer)
	respondNext(func(cmd domain.T3Command) (bool, string, string) {
		var p map[string]any
		_ = json.Unmarshal(cmd.Payload, &p)
		if cmd.Action != domain.T3ActionThreadSend || p["mode"] != "steer" || p["thread_id"] != "th-100" {
			t.Errorf("unexpected send command: %+v", cmd)
		}
		return true, `{"accepted":true}`, ""
	})
	out = tool(t, a, owner, "run-1", "c4", "t3_thread_send", `{"thread_id":"th-100","message":"Adjust tests","mode":"steer"}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"succeeded"`) {
		t.Fatalf("t3_thread_send: %s", out)
	}

	// 5. t3_thread_read
	respondNext(func(cmd domain.T3Command) (bool, string, string) {
		var p map[string]any
		_ = json.Unmarshal(cmd.Payload, &p)
		if cmd.Action != domain.T3ActionThreadRead || p["view"] != "messages" {
			t.Errorf("unexpected read command: %+v", cmd)
		}
		return true, `{"messages":[{"role":"assistant","text":"Working on it"}]}`, ""
	})
	out = tool(t, a, owner, "run-1", "c5", "t3_thread_read", `{"thread_id":"th-100","limit":10}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"Working on it"`) {
		t.Fatalf("t3_thread_read: %s", out)
	}

	// 6. t3_thread_respond clears pending_request once accepted (including bridge camelCase requestId + singular question).
	var bridgePending domain.T3PendingRequest
	if err := json.Unmarshal([]byte(`{"requestId":"req-7","question":{"id":"q1","question":"Approve?"}}`), &bridgePending); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.IngestEvent(ctx, domain.T3Event{
		EventID:        "ev-q-1",
		EnvironmentID:  "env-lan",
		ThreadID:       "th-100",
		Kind:           domain.T3EventThreadWaitingInput,
		PendingRequest: &bridgePending,
	}, nil); err != nil {
		t.Fatal(err)
	}
	listWithPending := tool(t, a, owner, "run-1", "c5b", "t3_threads_list", `{"host_id":"`+host.HostID+`"}`)
	if !strings.Contains(listWithPending, `"request_id":"req-7"`) || !strings.Contains(listWithPending, `"Approve?"`) {
		t.Fatalf("t3_threads_list missing normalized pending_request: %s", listWithPending)
	}
	respondNext(func(cmd domain.T3Command) (bool, string, string) {
		if cmd.Action != domain.T3ActionThreadRespond {
			t.Errorf("unexpected respond action: %s", cmd.Action)
		}
		return true, `{"accepted":true}`, ""
	})
	out = tool(t, a, owner, "run-1", "c6", "t3_thread_respond", `{"thread_id":"th-100","request_id":"req-7","answers":{"Approve?":"Yes"}}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"succeeded"`) {
		t.Fatalf("t3_thread_respond: %s", out)
	}
	th, err := cp.Thread(ctx, "th-100")
	if err != nil || th.PendingRequest != nil {
		t.Fatalf("pending_request was not cleared after t3_thread_respond: %+v err=%v", th, err)
	}

	// 7. t3_thread_control
	respondNext(func(cmd domain.T3Command) (bool, string, string) {
		var p map[string]any
		_ = json.Unmarshal(cmd.Payload, &p)
		if cmd.Action != domain.T3ActionThreadControl || p["action"] != "settle" {
			t.Errorf("unexpected control command: %+v", cmd)
		}
		return true, `{"status":"settled"}`, ""
	})
	out = tool(t, a, owner, "run-1", "c7", "t3_thread_control", `{"thread_id":"th-100","action":"settle"}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"settled"`) {
		t.Fatalf("t3_thread_control: %s", out)
	}
	th, _ = cp.Thread(ctx, "th-100")
	if th.Status != "settled" {
		t.Fatalf("expected settled status, got %q", th.Status)
	}

	// Offline fallback: register an offline host (via custom clock) and verify t3_thread_launch returns queued.
	offlineCP := t3.New(a.Repo.(*storage.Memory), a.Repo, t3.Config{
		OwnerUserID:     owner.OwnerID,
		OnlineThreshold: time.Minute,
		Now:             func() time.Time { return time.Now().UTC().Add(10 * time.Minute) },
	})
	a.T3 = offlineCP
	out = tool(t, a, owner, "run-2", "c8", "t3_thread_launch", `{
		"host_id":"`+host.HostID+`",
		"project_id":"proj-oa",
		"title":"Queued while offline",
		"message":"Run when online",
		"model_selection":{"provider_instance_id":"ag-1","model":"gemini-4-neon"}
	}`)
	if strings.Contains(out, `"error"`) || !strings.Contains(out, `"status":"queued"`) || !strings.Contains(out, `"command_id"`) {
		t.Fatalf("expected clean queued response for offline host, got: %s", out)
	}
}
