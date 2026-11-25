package desktopapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/storage"
	"openalbert/apps/backend/internal/t3"
)

type recordedTGCall struct {
	Method  string
	Payload map[string]any
	MsgID   int64
}

type fakeTelegramBotServer struct {
	mu      sync.Mutex
	nextID  int64
	calls   []recordedTGCall
	answers []string
}

func (f *fakeTelegramBotServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/botsecret/")
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)

		f.mu.Lock()
		rec := recordedTGCall{Method: method, Payload: payload}
		switch method {
		case "sendMessage":
			f.nextID++
			rec.MsgID = f.nextID
			f.calls = append(f.calls, rec)
			id := rec.MsgID
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
			return
		case "answerCallbackQuery":
			id, _ := payload["callback_query_id"].(string)
			txt, _ := payload["text"].(string)
			f.answers = append(f.answers, id+":"+txt)
			f.calls = append(f.calls, rec)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
			return
		default:
			f.calls = append(f.calls, rec)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
			return
		}
	})
}

func (f *fakeTelegramBotServer) snapshotCalls() []recordedTGCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedTGCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeTelegramBotServer) snapshotAnswers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answers...)
}

type fakeT3MCPState struct {
	mu              sync.Mutex
	threadID        string
	status          string
	latestRunStatus string
	prURL           string
	summary         string
	output          map[string]any
	pendingReqID    string
	pendingQuestion map[string]any

	launchArgs  map[string]any
	sendArgs    []map[string]any
	respondArgs []map[string]any
	readCount   int
}

func (s *fakeT3MCPState) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"protocolVersion": "2024-11-05"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			names := []string{
				"t3_environment_read",
				"t3_project_list",
				"orchestrator_capabilities",
				"t3_thread_launch",
				"t3_thread_send",
				"t3_thread_read",
				"t3_thread_wait",
				"t3_pending_request",
				"t3_thread_interrupt",
				"t3_thread_organize",
			}
			tools := make([]map[string]any, 0, len(names))
			for _, n := range names {
				tools = append(tools, map[string]any{
					"name":        n,
					"description": n,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"tools": tools},
			})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			out := s.handleToolCall(p.Name, p.Arguments)
			raw, _ := json.Marshal(out)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]string{{"type": "text", "text": string(raw)}},
				},
			})
		}
	})
}

func (s *fakeT3MCPState) handleToolCall(name string, args map[string]any) any {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch name {
	case "t3_environment_read":
		return map[string]any{"environmentId": "env-e2e-mac", "label": "MacBook Pro"}
	case "t3_project_list":
		return map[string]any{"projects": []map[string]any{
			{"id": "proj-oa", "title": "OpenAlbert", "workspaceRoot": "/code/openalbert"},
		}}
	case "orchestrator_capabilities":
		return map[string]any{"providers": []map[string]any{
			{
				"providerInstanceId": "antigravity",
				"driverKind":         "antigravity",
				"displayName":        "Antigravity",
				"models":             []map[string]any{{"id": "gemini-4-neon", "label": "Gemini 4 Neon"}},
			},
		}}
	case "t3_thread_launch":
		s.launchArgs = args
		s.threadID = "th-e2e-1"
		s.status = "running"
		s.latestRunStatus = "running"
		return map[string]any{
			"threadId":           s.threadID,
			"link":               "t3://thread/" + s.threadID,
			"title":              args["title"],
			"status":             "running",
			"branch":             "feat/e2e-glue",
			"model":              "gemini-4-neon",
			"providerInstanceId": "antigravity",
			"runId":              "run-1",
		}
	case "t3_thread_send":
		s.sendArgs = append(s.sendArgs, args)
		return map[string]any{"accepted": true, "runId": "run-1"}
	case "t3_thread_read":
		s.readCount++
		th := map[string]any{
			"threadId":           s.threadID,
			"projectId":          "proj-oa",
			"title":              "Build PR E",
			"link":               "t3://thread/" + s.threadID,
			"status":             s.status,
			"latestRunId":        "run-1",
			"branch":             "feat/e2e-glue",
			"model":              "gemini-4-neon",
			"providerInstanceId": "antigravity",
		}
		if s.prURL != "" {
			th["linkedPullRequest"] = map[string]any{"url": s.prURL}
		}
		run := map[string]any{"runId": "run-1", "status": s.latestRunStatus}
		if s.output != nil {
			run["output"] = s.output
		}
		resp := map[string]any{
			"thread":     th,
			"recentRuns": []any{run},
		}
		if s.summary != "" {
			resp["items"] = []map[string]any{{"type": "assistant_message", "role": "assistant", "text": s.summary}}
		}
		return resp
	case "t3_thread_wait":
		return map[string]any{"output": s.output, "timedOut": false}
	case "t3_pending_request":
		action, _ := args["action"].(string)
		switch action {
		case "list":
			if s.pendingReqID != "" {
				return map[string]any{"requestIds": []string{s.pendingReqID}}
			}
			return map[string]any{"requestIds": []string{}}
		case "read":
			return map[string]any{
				"requestId": s.pendingReqID,
				"questions": []any{s.pendingQuestion},
			}
		case "respond":
			s.respondArgs = append(s.respondArgs, args)
			s.pendingReqID = ""
			s.pendingQuestion = nil
			s.status = "running"
			s.latestRunStatus = "running"
			return map[string]any{"accepted": true}
		}
	}
	return map[string]any{"ok": true}
}

func buildOpenAlbertCLI(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to locate test file path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	bin := filepath.Join(t.TempDir(), "openalbert")
	cmd := exec.Command("go", "build", "-o", bin, "./apps/cli/cmd/openalbert")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build openalbert CLI failed: %v\n%s", err, string(out))
	}
	return bin
}

func waitForCondition(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestPRABCEndToEndWorkspaceT3Integration(t *testing.T) {
	cliBin := buildOpenAlbertCLI(t)

	const (
		ownerID    int64 = 7
		workspace  int64 = -100555
		topicID    int   = 42
		topicScope       = "telegram:group:-100555:topic:42"
	)

	// 1. Fake Telegram Bot API + fake T3 Code MCP server.
	fakeTG := &fakeTelegramBotServer{}
	tgSrv := httptest.NewServer(fakeTG.handler())
	defer tgSrv.Close()

	mcpState := &fakeT3MCPState{}
	mcpSrv := httptest.NewServer(mcpState.handler())
	defer mcpSrv.Close()

	// 2. Real backend stack: Storage + Telegram + CallbackRouter + T3 ControlPlane + WorkspaceBridge + Activities + DesktopAPI.
	ctx := context.Background()
	repo := storage.NewMemory()
	topicConv := domain.Conversation{
		ID:       topicScope,
		Title:    "Build PR E",
		ChatID:   workspace,
		ThreadID: topicID,
		OwnerID:  ownerID,
		Private:  false,
	}
	if err := repo.SaveConversation(ctx, topicConv); err != nil {
		t.Fatal(err)
	}

	tg, err := integrations.NewTelegram(integrations.TelegramConfig{
		Token:            "secret",
		BaseURL:          tgSrv.URL,
		OwnerUserID:      ownerID,
		AllowedUserIDs:   []int64{ownerID, 99},
		WorkspaceGroupID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}

	cp := t3.New(repo, repo, t3.Config{
		OwnerUserID:       ownerID,
		OnlineThreshold:   3 * time.Minute,
		DispatchTimeout:   5 * time.Second,
		KeepaliveInterval: 100 * time.Millisecond,
	})
	wsBridge := agent.NewT3WorkspaceBridge(agent.T3WorkspaceBridgeConfig{
		ControlPlane:     cp,
		Repo:             repo,
		Transport:        tg,
		OwnerUserID:      ownerID,
		WorkspaceGroupID: workspace,
	})
	cp.AddObserver(wsBridge)

	callbacks := integrations.NewCallbackRouter(ownerID, workspace, tg.AnswerCallbackQuery)
	callbacks.RegisterPrefix(agent.T3CallbackPrefix, wsBridge)

	activities := &agent.Activities{
		OwnerUserID:      ownerID,
		WorkspaceGroupID: workspace,
		Workspace:        tg,
		Messenger:        tg,
		Repo:             repo,
		T3:               cp,
	}

	api := &API{
		Repo:       repo,
		OwnerID:    ownerID,
		Token:      testToken,
		Activities: activities,
		T3:         cp,
	}
	var llmCalls atomic.Int32
	baseHandler := api.Handler()
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			llmCalls.Add(1)
			http.Error(w, "unexpected LLM call in zero-LLM E2E flow", http.StatusInternalServerError)
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_model": "proxy/gpt-5.4",
				"models": []map[string]any{
					{"id": "proxy/gpt-5.4", "provider": "proxy", "model": "gpt-5.4", "display_name": "GPT 5.4", "context_window_tokens": 128000},
				},
			})
			return
		default:
			baseHandler.ServeHTTP(w, r)
		}
	}))
	defer oaSrv.Close()

	// 3. Start the REAL compiled `openalbert acp --bridge` CLI subprocess!
	stateDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cfgBody := fmt.Sprintf(`server_url = %q
token = %q
state_dir = %q
sync_inspector = false
bridge = true
host_label = "MacBook Pro"
request_timeout = "10s"
`, oaSrv.URL, testToken, stateDir)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	cliCtx, cancelCLI := context.WithCancel(ctx)
	defer cancelCLI()
	proc := exec.CommandContext(cliCtx, cliBin, "acp", "--config", cfgPath, "--bridge")
	stdin, err := proc.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderrBuf strings.Builder
	var stderrMu sync.Mutex
	proc.Stderr = &lockedWriter{mu: &stderrMu, w: &stderrBuf}
	if err := proc.Start(); err != nil {
		t.Fatalf("start openalbert acp --bridge: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		cancelCLI()
		_ = proc.Wait()
	}()

	var (
		rpcWriteMu sync.Mutex
		rpcRespMu  sync.Mutex
		rpcWaiters = map[string]chan json.RawMessage{}
	)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 4096), 4<<20)
		for sc.Scan() {
			var env struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &env) != nil || len(env.ID) == 0 {
				continue
			}
			var key string
			if json.Unmarshal(env.ID, &key) != nil {
				key = string(env.ID)
			}
			rpcRespMu.Lock()
			ch := rpcWaiters[key]
			delete(rpcWaiters, key)
			rpcRespMu.Unlock()
			if ch != nil {
				if len(env.Error) > 0 && string(env.Error) != "null" {
					ch <- env.Error
				} else {
					ch <- env.Result
				}
			}
		}
	}()

	callACP := func(id, method string, params any) json.RawMessage {
		t.Helper()
		ch := make(chan json.RawMessage, 1)
		rpcRespMu.Lock()
		rpcWaiters[id] = ch
		rpcRespMu.Unlock()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		rpcWriteMu.Lock()
		_, wErr := stdin.Write(append(b, '\n'))
		rpcWriteMu.Unlock()
		if wErr != nil {
			t.Fatalf("write ACP %s: %v", method, wErr)
		}
		select {
		case res := <-ch:
			return res
		case <-time.After(10 * time.Second):
			stderrMu.Lock()
			logs := stderrBuf.String()
			stderrMu.Unlock()
			t.Fatalf("timeout waiting for ACP %s; stderr:\n%s", method, logs)
			return nil
		}
	}

	callACP("init", "initialize", map[string]any{"protocolVersion": 1})
	newRes := callACP("new", "session/new", map[string]any{
		"cwd": t.TempDir(),
		"mcpServers": []map[string]any{
			{"name": "t3-code", "type": "http", "url": mcpSrv.URL},
		},
	})
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(newRes, &sess); err != nil || sess.SessionID == "" {
		t.Fatalf("session/new failed: %s (err=%v)", string(newRes), err)
	}

	// Step 1: Host registers via real bridge -> #Control pinned status board updates with online host!
	waitForCondition(t, 8*time.Second, "host registration and #Control board pin", func() bool {
		hosts, err := cp.ListHosts(ctx)
		if err != nil || len(hosts) != 1 || !hosts[0].Online {
			return false
		}
		var sawBoard, sawPin bool
		for _, c := range fakeTG.snapshotCalls() {
			if c.Method == "sendMessage" {
				if txt, _ := c.Payload["text"].(string); strings.Contains(txt, "🟢 MacBook Pro") {
					sawBoard = true
				}
			}
			if c.Method == "pinChatMessage" {
				sawPin = true
			}
		}
		return sawBoard && sawPin
	})
	hosts, _ := cp.ListHosts(ctx)
	hostID := hosts[0].HostID

	// Step 2: t3_thread_launch from Owner Workspace topic -> delivered over SSE to bridge -> thread launches -> topic title becomes 🔄 Build PR E & #Control updates!
	launchOut, err := activities.ToolCall(ctx, agent.ToolInput{
		RunKey:       "e2e-run",
		Step:         1,
		Conversation: topicConv,
		Call: domain.ToolCall{
			ID:   "call-launch",
			Name: "t3_thread_launch",
			Arguments: json.RawMessage(fmt.Sprintf(`{
				"host_id": %q,
				"project_id": "proj-oa",
				"title": "Build PR E",
				"message": "Wire everything end-to-end",
				"model_selection": {"provider_instance_id": "antigravity", "model": "gemini-4-neon"}
			}`, hostID)),
		},
	})
	if err != nil || strings.Contains(launchOut, `"error"`) || !strings.Contains(launchOut, `"th-e2e-1"`) {
		t.Fatalf("t3_thread_launch from workspace topic failed: out=%s err=%v", launchOut, err)
	}

	waitForCondition(t, 5*time.Second, "topic title 🔄 Build PR E and #Control running thread", func() bool {
		var sawTopic, sawBoard bool
		for _, c := range fakeTG.snapshotCalls() {
			if c.Method == "editForumTopic" && c.Payload["name"] == "🔄 Build PR E" {
				sawTopic = true
			}
			if c.Method == "editMessageText" {
				if txt, _ := c.Payload["text"].(string); strings.Contains(txt, "🔄 Build PR E") {
					sawBoard = true
				}
			}
		}
		return sawTopic && sawBoard
	})

	// Step 3: t3_thread_send(mode: "steer") from workspace topic -> delivered over SSE to bridge!
	steerOut, err := activities.ToolCall(ctx, agent.ToolInput{
		RunKey:       "e2e-run",
		Step:         2,
		Conversation: topicConv,
		Call: domain.ToolCall{
			ID:        "call-steer",
			Name:      "t3_thread_send",
			Arguments: json.RawMessage(`{"thread_id":"th-e2e-1","message":"Also cover callback validation","mode":"steer"}`),
		},
	})
	if err != nil || strings.Contains(steerOut, `"error"`) || !strings.Contains(steerOut, `"succeeded"`) {
		t.Fatalf("t3_thread_send(steer) failed: out=%s err=%v", steerOut, err)
	}
	mcpState.mu.Lock()
	if len(mcpState.sendArgs) != 1 || mcpState.sendArgs[0]["mode"] != "steer" || mcpState.sendArgs[0]["threadId"] != "th-e2e-1" {
		mcpState.mu.Unlock()
		t.Fatalf("MCP did not receive steer send: %+v", mcpState.sendArgs)
	}
	mcpState.mu.Unlock()

	// Step 4: thread_waiting_input with structured options -> topic title becomes ❓ Build PR E, inline buttons posted -> owner callback dispatches thread_respond with ZERO LLM calls!
	mcpState.mu.Lock()
	mcpState.status = "waiting"
	mcpState.pendingReqID = "req-e2e-very-long-pending-request-id-1234567890-abcdefghijklmnopqrstuvwxyz"
	mcpState.pendingQuestion = map[string]any{
		"id":       "rollout",
		"question": "Choose rollout strategy?",
		"options":  []string{"Canary", "Full rollout"},
	}
	mcpState.mu.Unlock()

	// Trigger immediate deterministic zero-LLM bridge tick to poll active threads.
	callACP("tick-wait", "session/prompt", map[string]any{
		"sessionId": sess.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "__openalbert_bridge_tick__"}},
	})

	var (
		questionMsgID int64
		canaryCBData  string
	)
	waitForCondition(t, 5*time.Second, "❓ topic title and inline option buttons", func() bool {
		var sawTopic bool
		for _, c := range fakeTG.snapshotCalls() {
			if c.Method == "editForumTopic" && c.Payload["name"] == "❓ Build PR E" {
				sawTopic = true
			}
			if c.Method == "sendMessage" {
				if tid, ok := c.Payload["message_thread_id"].(float64); ok && int(tid) == topicID {
					if rm, ok := c.Payload["reply_markup"].(map[string]any); ok {
						if kb, ok := rm["inline_keyboard"].([]any); ok {
							for _, rawRow := range kb {
								row, _ := rawRow.([]any)
								for _, rawBtn := range row {
									btn, _ := rawBtn.(map[string]any)
									if btn["text"] == "Canary" {
										canaryCBData, _ = btn["callback_data"].(string)
										questionMsgID = c.MsgID
									}
								}
							}
						}
					}
				}
			}
		}
		return sawTopic && questionMsgID > 0 && canaryCBData != ""
	})
	if len(canaryCBData) > 64 {
		t.Fatalf("callback_data exceeds 64 bytes: %q (%d bytes)", canaryCBData, len(canaryCBData))
	}
	// Structured waiting_input handled by inline buttons MUST NOT queue an LLM wake turn!
	if pendingAfterWait, err := repo.PendingIncoming(ctx, 100); err != nil || len(pendingAfterWait) != 0 {
		t.Fatalf("structured waiting_input button flow must not queue an agent wake turn: %+v err=%v", pendingAfterWait, err)
	}

	// Non-owner button tap is rejected.
	if err := callbacks.Dispatch(ctx, domain.CallbackQuery{
		ID:        "cb-intruder",
		UserID:    99,
		ChatID:    workspace,
		ThreadID:  topicID,
		MessageID: questionMsgID,
		Data:      canaryCBData,
	}); err == nil {
		t.Fatal("expected non-owner callback to fail authorization")
	}

	// Owner taps "Canary" -> zero-LLM thread_respond delivered over SSE to bridge!
	if err := callbacks.Dispatch(ctx, domain.CallbackQuery{
		ID:        "cb-canary",
		UserID:    ownerID,
		ChatID:    workspace,
		ThreadID:  topicID,
		MessageID: questionMsgID,
		Data:      canaryCBData,
	}); err != nil {
		t.Fatalf("owner callback dispatch failed: %v", err)
	}

	mcpState.mu.Lock()
	if len(mcpState.respondArgs) != 1 || mcpState.respondArgs[0]["threadId"] != "th-e2e-1" {
		mcpState.mu.Unlock()
		t.Fatalf("MCP did not receive t3_pending_request(respond): %+v", mcpState.respondArgs)
	}
	ansMap, _ := mcpState.respondArgs[0]["answers"].(map[string]any)
	mcpState.mu.Unlock()
	if ansMap["rollout"] != "Canary" {
		t.Fatalf("expected MCP respond answers[rollout]=Canary, got %+v", ansMap)
	}
	if th, err := cp.Thread(ctx, "th-e2e-1"); err != nil || th.PendingRequest != nil {
		t.Fatalf("expected pending_request cleared, got %+v err=%v", th, err)
	}
	foundEditedAns := false
	for _, c := range fakeTG.snapshotCalls() {
		if c.Method == "editMessageText" {
			if mid, _ := c.Payload["message_id"].(float64); int64(mid) == questionMsgID {
				txt, _ := c.Payload["text"].(string)
				if strings.Contains(txt, "✓ Answered: Canary") {
					if rm, ok := c.Payload["reply_markup"].(map[string]any); ok {
						if kb, ok := rm["inline_keyboard"].([]any); ok && len(kb) == 0 {
							foundEditedAns = true
						}
					}
				}
			}
		}
	}
	if !foundEditedAns {
		t.Fatal("expected question message edited with '✓ Answered: Canary' and cleared inline keyboard")
	}

	// Step 5: thread_completed (real T3 shape: thread.status="idle" + latestRunStatus="completed") with structured output and pr_url -> topic title becomes 👀 Build PR E, #Control shows 👀 (not 🔄), owning topic is woken, and Sync button works!
	mcpState.mu.Lock()
	mcpState.status = "idle"
	mcpState.latestRunStatus = "completed"
	mcpState.prURL = "https://github.com/mishankov/OpenAlbert/pull/999"
	mcpState.summary = "PR #999 staged and all tests green"
	mcpState.output = map[string]any{"tests_passed": true, "pr": 999}
	mcpState.mu.Unlock()

	callACP("tick-done", "session/prompt", map[string]any{
		"sessionId": sess.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "__openalbert_bridge_tick__"}},
	})

	var syncCBData string
	waitForCondition(t, 5*time.Second, "👀 topic title, #Control PR link, Sync button, and owning topic wake", func() bool {
		var sawTopic, sawBoard, sawWake bool
		for _, c := range fakeTG.snapshotCalls() {
			if c.Method == "editForumTopic" && c.Payload["name"] == "👀 Build PR E" {
				sawTopic = true
			}
			if c.Method == "editMessageText" {
				if txt, _ := c.Payload["text"].(string); strings.Contains(txt, "👀 Build PR E — https://github.com/mishankov/OpenAlbert/pull/999") {
					sawBoard = true
				}
			}
			if c.Method == "sendMessage" && c.MsgID != questionMsgID {
				if tid, ok := c.Payload["message_thread_id"].(float64); ok && int(tid) == topicID {
					if rm, ok := c.Payload["reply_markup"].(map[string]any); ok {
						if kb, ok := rm["inline_keyboard"].([]any); ok {
							for _, rawRow := range kb {
								row, _ := rawRow.([]any)
								for _, rawBtn := range row {
									btn, _ := rawBtn.(map[string]any)
									if txt, _ := btn["text"].(string); strings.Contains(txt, "Sync") {
										syncCBData, _ = btn["callback_data"].(string)
									}
								}
							}
						}
					}
				}
			}
		}
		pending, _ := repo.PendingIncoming(ctx, 100)
		for _, in := range pending {
			if in.Source == "t3" && in.Conversation.ID == topicScope &&
				strings.Contains(in.Text, "thread_completed") &&
				strings.Contains(in.Text, "https://github.com/mishankov/OpenAlbert/pull/999") &&
				strings.Contains(in.Text, `"tests_passed":true`) {
				sawWake = true
			}
		}
		return sawTopic && sawBoard && sawWake && syncCBData != ""
	})

	// Tapping 🔄 Sync button dispatches thread_control(sync) directly in Go!
	mcpState.mu.Lock()
	readsBefore := mcpState.readCount
	mcpState.mu.Unlock()
	if err := callbacks.Dispatch(ctx, domain.CallbackQuery{
		ID:       "cb-sync",
		UserID:   ownerID,
		ChatID:   workspace,
		ThreadID: topicID,
		Data:     syncCBData,
	}); err != nil {
		t.Fatalf("sync callback failed: %v", err)
	}
	mcpState.mu.Lock()
	readsAfter := mcpState.readCount
	mcpState.mu.Unlock()
	if readsAfter <= readsBefore {
		t.Fatalf("expected Sync button to invoke MCP t3_thread_read; before=%d after=%d", readsBefore, readsAfter)
	}

	// Prove ZERO LLM calls occurred across the entire E2E lifecycle!
	if llmCalls.Load() != 0 {
		t.Fatalf("expected 0 LLM calls across zero-LLM bridge + callback flow, got %d", llmCalls.Load())
	}
	answers := strings.Join(fakeTG.snapshotAnswers(), ",")
	if !strings.Contains(answers, "cb-intruder:Unauthorized") || !strings.Contains(answers, "cb-canary:Answered: Canary") || !strings.Contains(answers, "cb-sync:Synced") {
		t.Fatalf("unexpected callback answers: %s", answers)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}
