package bridge

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

	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/mcpclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

// allowedT3ToolArgs mirrors the real T3 Code MCP tool schemas (no toolSummary).
var allowedT3ToolArgs = map[string]map[string]bool{
	"t3_environment_read":       {"environmentId": true},
	"t3_project_list":           {"cursor": true, "environmentId": true, "limit": true},
	"orchestrator_capabilities": {"environmentId": true, "search": true},
	"t3_thread_launch": {
		"attachments": true, "clientRequestId": true, "delegatedFrom": true, "environmentId": true,
		"generateTitle": true, "interactionMode": true, "message": true, "modelSelection": true,
		"outputSchema": true, "projectId": true, "runtimeMode": true, "scratch": true,
		"title": true, "workflowRun": true, "workspaceStrategy": true,
	},
	"t3_thread_send": {
		"clientRequestId": true, "environmentId": true, "freshContext": true,
		"message": true, "mode": true, "outputSchema": true, "threadId": true,
	},
	"t3_thread_read": {
		"afterPosition": true, "beforePosition": true, "environmentId": true, "itemId": true,
		"limit": true, "maxCharsPerItem": true, "runLimit": true, "textOffset": true,
		"threadId": true, "view": true,
	},
	"t3_thread_wait": {
		"environmentId": true, "runId": true, "threadId": true,
		"timeoutMs": true, "untilDelegatedWorkSettled": true, "untilOutput": true,
	},
	"t3_pending_request": {
		"action": true, "answers": true, "environmentId": true, "requestId": true, "threadId": true,
	},
	"t3_thread_interrupt": {
		"clientRequestId": true, "environmentId": true, "reason": true, "runId": true, "stop": true, "threadId": true,
	},
	"t3_thread_organize": {
		"action": true, "environmentId": true, "snoozedUntil": true, "threadId": true,
	},
}

type fakeT3MCP struct {
	mu                  sync.Mutex
	failOutsideTurn     bool
	failParentNotActive bool
	failPendingReadOnce bool
	envID               string
	threadState         map[string]map[string]any
	threadWaitOutput    map[string]any
	threadWaitTimedOut  map[string]bool
	pendingRequests     map[string]map[string]any
	calls               []string
	launchCount         int
	lastLaunchArgs      map[string]any
	lastSendArgs        map[string]any
	lastRespondArgs     map[string]any
	lastWaitArgs        map[string]any
	lastControlCalls    []string
}

func newFakeT3MCP(envID string) *fakeT3MCP {
	return &fakeT3MCP{
		envID:              envID,
		threadState:        map[string]map[string]any{},
		threadWaitOutput:   map[string]any{},
		threadWaitTimedOut: map[string]bool{},
		pendingRequests:    map[string]map[string]any{},
	}
}

func (f *fakeT3MCP) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			toolNames := []string{
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
			tools := make([]map[string]any, 0, len(toolNames))
			for _, name := range toolNames {
				tools = append(tools, map[string]any{
					"name":        name,
					"description": name,
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": tools}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			// Strict argument validation matching live T3 MCP server (-32602 on unknown arg).
			if allowed, ok := allowedT3ToolArgs[p.Name]; ok {
				for k := range p.Arguments {
					if !allowed[k] {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"jsonrpc": "2.0",
							"id":      req.ID,
							"error":   map[string]any{"code": -32602, "message": fmt.Sprintf("Unknown argument %q", k)},
						})
						return
					}
				}
			}

			f.mu.Lock()
			if f.failOutsideTurn {
				f.mu.Unlock()
				http.Error(w, "mcp unavailable outside turn", http.StatusServiceUnavailable)
				return
			}
			if f.failParentNotActive && (p.Name == "t3_thread_interrupt" || p.Name == "t3_thread_send" || p.Name == "t3_thread_launch") {
				f.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]string{{"type": "text", "text": `{"code":"parent_not_active","message":"Cannot mutate threads while parent session is idle"}`}},
					},
				})
				return
			}
			if p.Name == "t3_pending_request" && p.Arguments["action"] == "read" && f.failPendingReadOnce {
				f.failPendingReadOnce = false
				f.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]string{{"type": "text", "text": "temporary pending read error"}},
					},
				})
				return
			}
			f.calls = append(f.calls, p.Name)
			f.mu.Unlock()

			out := f.handleTool(p.Name, p.Arguments)
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

func (f *fakeT3MCP) handleTool(name string, args map[string]any) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "t3_environment_read":
		return map[string]any{
			"environmentId": f.envID,
			"label":         "MacBook Env Label",
		}
	case "t3_project_list":
		cursor, _ := args["cursor"].(float64)
		if cursor == 0 {
			return map[string]any{
				"projects": []map[string]any{
					{"id": "proj-1", "title": "OpenAlbert", "workspaceRoot": "/dev/openalbert"},
				},
				"nextCursor": 1,
			}
		}
		return map[string]any{
			"projects": []map[string]any{
				{"id": "proj-2", "title": "Website", "workspaceRoot": "/dev/website"},
				{"id": "proj-archived", "title": "Old", "workspaceRoot": "/dev/old", "archivedAt": "2026-01-01T00:00:00Z"},
			},
		}
	case "orchestrator_capabilities":
		return map[string]any{
			"providers": []map[string]any{
				{
					"providerInstanceId": "antigravity",
					"driverKind":         "antigravity",
					"displayName":        "Antigravity",
					"constraints":        []string{},
					"models": []map[string]any{
						{"id": "gemini-4-neon", "label": "Gemini 4 Neon", "options": []map[string]any{{"id": "effort", "values": []string{"medium", "high"}}}},
					},
				},
				{
					"providerInstanceId": "codex",
					"driverKind":         "codex",
					"displayName":        "Codex",
					"constraints":        []string{},
					"models": []map[string]any{
						{"id": "gpt-6.2-astra", "label": "GPT-6.2 Astra"},
					},
				},
				{
					"providerInstanceId": "cursor",
					"driverKind":         "cursor",
					"displayName":        "Cursor",
					"constraints":        []string{"Provider instance is disabled."},
					"models":             []map[string]any{},
				},
			},
		}
	case "t3_thread_launch":
		f.launchCount++
		f.lastLaunchArgs = args
		tid := fmt.Sprintf("thread-%d", 100+f.launchCount)
		title, _ := args["title"].(string)
		pid, _ := args["projectId"].(string)
		f.threadState[tid] = map[string]any{
			"thread": map[string]any{
				"threadId":           tid,
				"projectId":          pid,
				"title":              title,
				"link":               "[" + title + "](t3-thread://v2/" + f.envID + "/" + tid + ")",
				"status":             "running",
				"latestRunId":        "run-1",
				"providerInstanceId": "codex",
				"model":              "gpt-6.2-astra",
				"branch":             "feat/test",
				"itemCount":          1,
			},
			"recentRuns": []map[string]any{{"runId": "run-1", "status": "running"}},
			"items":      []map[string]any{},
		}
		return map[string]any{
			"threadId":           tid,
			"projectId":          pid,
			"title":              title,
			"link":               "[" + title + "](t3-thread://v2/" + f.envID + "/" + tid + ")",
			"status":             "running",
			"branch":             "feat/test",
			"model":              "gpt-6.2-astra",
			"providerInstanceId": "codex",
		}
	case "t3_thread_send":
		f.lastSendArgs = args
		return map[string]any{"ok": true, "threadId": args["threadId"]}
	case "t3_thread_read":
		tid, _ := args["threadId"].(string)
		if st, ok := f.threadState[tid]; ok {
			return st
		}
		return map[string]any{"thread": map[string]any{"threadId": tid, "status": "idle"}}
	case "t3_thread_wait":
		f.lastWaitArgs = args
		tid, _ := args["threadId"].(string)
		if f.threadWaitTimedOut[tid] {
			return map[string]any{"status": "completed", "timedOut": true}
		}
		if out, ok := f.threadWaitOutput[tid]; ok {
			return map[string]any{"status": "completed", "output": out}
		}
		return map[string]any{"status": "completed"}
	case "t3_pending_request":
		action, _ := args["action"].(string)
		tid, _ := args["threadId"].(string)
		switch action {
		case "list":
			if req, ok := f.pendingRequests[tid]; ok {
				return map[string]any{"requestIds": []string{req["requestId"].(string)}}
			}
			return map[string]any{"requestIds": []string{}}
		case "read":
			if req, ok := f.pendingRequests[tid]; ok {
				return req
			}
			return map[string]any{}
		case "respond":
			f.lastRespondArgs = args
			delete(f.pendingRequests, tid)
			return map[string]any{"ok": true}
		}
	case "t3_thread_interrupt":
		f.lastControlCalls = append(f.lastControlCalls, "interrupt:"+args["threadId"].(string))
		return map[string]any{"ok": true}
	case "t3_thread_organize":
		f.lastControlCalls = append(f.lastControlCalls, args["action"].(string)+":"+args["threadId"].(string))
		return map[string]any{"ok": true}
	}
	return map[string]any{}
}

func TestSingletonFlock(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		ServerURL:      "http://127.0.0.1:8080",
		Token:          "tok",
		StateDir:       dir,
		CustomStateDir: true,
		Bridge:         true,
		HostLabel:      "test-host",
	}
	cli := serverclient.New(cfg.ServerURL, cfg.Token, time.Second)

	b1, ok1, err := New(cfg, cli, nil)
	if err != nil || !ok1 || b1 == nil {
		t.Fatalf("first bridge should acquire lock: ok=%v err=%v", ok1, err)
	}

	b2, ok2, err := New(cfg, cli, nil)
	if err != nil || ok2 || b2 != nil {
		t.Fatalf("second bridge should NOT acquire lock: ok=%v err=%v", ok2, err)
	}

	if err := b1.Close(); err != nil {
		t.Fatalf("close first bridge: %v", err)
	}

	b3, ok3, err := New(cfg, cli, nil)
	if err != nil || !ok3 || b3 == nil {
		t.Fatalf("third bridge should acquire lock after release: ok=%v err=%v", ok3, err)
	}
	_ = b3.Close()
}

func TestBridgeFullLANRoundtripSSEWatcherAndOnceDrain(t *testing.T) {
	fakeMCP := newFakeT3MCP("env-mac-01")
	mcpSrv := httptest.NewServer(fakeMCP.handler())
	defer mcpSrv.Close()

	mgr, warnings := mcpclient.Connect(context.Background(), []mcpclient.ServerConfig{
		{Name: "t3-code", Type: "http", URL: mcpSrv.URL},
	}, nil)
	if len(warnings) > 0 {
		t.Fatalf("unexpected MCP warnings: %v", warnings)
	}
	defer mgr.Close()

	var (
		mu             sync.Mutex
		registers      []serverclient.T3HostRegisterRequest
		heartbeats     []serverclient.T3HeartbeatRequest
		cmdResults     = map[string]serverclient.T3CommandResultRequest{}
		failResultOnce = map[string]bool{}
		events         []serverclient.T3ThreadEvent
		sseCh          = make(chan serverclient.T3Command, 16)
		sseConnected   = make(chan struct{}, 4)
		resultSignal   = make(chan string, 16)
		eventSignal    = make(chan string, 32)
		badAuthCount   atomic.Int32
	)

	piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lan-secret" {
			badAuthCount.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("User-Agent") != "openalbert/0.1" {
			t.Errorf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/hosts/register":
			var req serverclient.T3HostRegisterRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			registers = append(registers, req)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.T3HostRegisterResponse{
				HostID:        "host-1",
				EnvironmentID: req.EnvironmentID,
				RegisteredAt:  "2026-11-25T20:00:00Z",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/heartbeat":
			var req serverclient.T3HeartbeatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			heartbeats = append(heartbeats, req)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.T3HeartbeatResponse{
				OK:         true,
				ServerTime: "2026-11-25T20:00:01Z",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/t3/commands/stream" && r.URL.Query().Get("once") == "true":
			// Control plane does NOT re-return SSE-dispatched commands here.
			_ = json.NewEncoder(w).Encode(serverclient.T3CommandsDrainResponse{Commands: []serverclient.T3Command{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/t3/commands/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			fmt.Fprint(w, ": connected\n\n")
			flusher.Flush()
			select {
			case sseConnected <- struct{}{}:
			default:
			}
			for {
				select {
				case <-r.Context().Done():
					return
				case cmd := <-sseCh:
					raw, _ := json.Marshal(cmd)
					fmt.Fprintf(w, "event: command\nid: %s\ndata: %s\n\n", cmd.CommandID, raw)
					flusher.Flush()
				}
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/t3/commands/") && strings.HasSuffix(r.URL.Path, "/result"):
			cmdID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/t3/commands/"), "/result")
			var req serverclient.T3CommandResultRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			if failResultOnce[cmdID] {
				delete(failResultOnce, cmdID)
				mu.Unlock()
				http.Error(w, "temporary lan error", http.StatusBadGateway)
				return
			}
			cmdResults[cmdID] = req
			mu.Unlock()
			resultSignal <- cmdID
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/events":
			var ev serverclient.T3ThreadEvent
			_ = json.NewDecoder(r.Body).Decode(&ev)
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			eventSignal <- ev.Kind
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer piSrv.Close()

	cfg := config.Config{
		ServerURL:      piSrv.URL,
		Token:          "lan-secret",
		StateDir:       t.TempDir(),
		CustomStateDir: true,
		Bridge:         true,
		HostLabel:      "Denis-MacBook",
	}
	client := serverclient.New(cfg.ServerURL, cfg.Token, 5*time.Second)
	b, acquired, err := New(cfg, client, nil)
	if err != nil || !acquired {
		t.Fatalf("failed to create bridge: acquired=%v err=%v", acquired, err)
	}
	b.WatchInterval = 40 * time.Millisecond
	b.HeartbeatInterval = 150 * time.Millisecond
	b.ReconnectBaseDelay = 20 * time.Millisecond
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	b.AttachMCP(mgr)

	select {
	case <-sseConnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SSE command stream connection")
	}

	mu.Lock()
	if len(registers) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 registration, got %d", len(registers))
	}
	reg := registers[0]
	mu.Unlock()
	if reg.EnvironmentID != "env-mac-01" || reg.Label != "Denis-MacBook" || len(reg.Projects) != 2 || len(reg.Providers) != 2 {
		t.Fatalf("unexpected registration payload: %+v", reg)
	}

	waitResult := func(wantID string) serverclient.T3CommandResultRequest {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case id := <-resultSignal:
				if id == wantID {
					mu.Lock()
					res := cmdResults[id]
					mu.Unlock()
					return res
				}
			case <-deadline:
				t.Fatalf("timed out waiting for result of %s", wantID)
			}
		}
	}
	waitEvent := func(wantKind string) serverclient.T3ThreadEvent {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case k := <-eventSignal:
				if k == wantKind {
					mu.Lock()
					for i := len(events) - 1; i >= 0; i-- {
						if events[i].Kind == wantKind {
							ev := events[i]
							mu.Unlock()
							return ev
						}
					}
					mu.Unlock()
				}
			case <-deadline:
				t.Fatalf("timed out waiting for event %s", wantKind)
			}
		}
	}

	// 1. Live SSE thread_launch with output_schema, snake_case model_selection & workspace_strategy,
	// AND simulate transient result POST failure to verify cached result retry without duplicate launch.
	mu.Lock()
	failResultOnce["cmd-launch"] = true
	mu.Unlock()
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-launch",
		Action:    "thread_launch",
		Payload: json.RawMessage(`{
			"project_id": "proj-1",
			"title": "Build feature X",
			"message": "Implement X",
			"model_selection": {"provider_instance_id": "codex", "model": "gpt-6.2-astra"},
			"workspace_strategy": {"type": "worktree", "base_ref": "origin/master", "start_from_origin": true},
			"output_schema": {"type": "object", "properties": {"done": {"type": "boolean"}}}
		}`),
	}
	launchedEv := waitEvent("thread_launched")
	if launchedEv.ThreadID != "thread-101" || launchedEv.ProviderInstanceID != "codex" || launchedEv.Model != "gpt-6.2-astra" {
		t.Fatalf("unexpected thread_launched event: %+v", launchedEv)
	}
	// Cached result is retried by heartbeat/tick without re-launching!
	launchRes := waitResult("cmd-launch")
	if !launchRes.OK || !strings.Contains(string(launchRes.Result), `"thread-101"`) {
		t.Fatalf("unexpected thread_launch result after retry: %+v", launchRes)
	}
	fakeMCP.mu.Lock()
	launchesSoFar := fakeMCP.launchCount
	ms, _ := fakeMCP.lastLaunchArgs["modelSelection"].(map[string]any)
	ws, _ := fakeMCP.lastLaunchArgs["workspaceStrategy"].(map[string]any)
	clientReqID := fakeMCP.lastLaunchArgs["clientRequestId"]
	fakeMCP.mu.Unlock()
	if launchesSoFar != 1 {
		t.Fatalf("expected exactly 1 t3_thread_launch call despite result POST retry, got %d", launchesSoFar)
	}
	if clientReqID != "cmd-launch" || ms["instanceId"] != "codex" || ws["baseRef"] != "origin/master" || ws["startFromOrigin"] != true {
		t.Fatalf("normalized launch args mismatch: clientReqID=%v ms=%+v ws=%+v", clientReqID, ms, ws)
	}

	// 2. Missing attachment fails cleanly before invoking T3 MCP.
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-attach-err",
		Action:    "thread_launch",
		Payload:   json.RawMessage(`{"title":"With file","attachment_ids":["att-1"]}`),
	}
	attRes := waitResult("cmd-attach-err")
	if attRes.OK || !strings.Contains(attRes.Error, "download attachment") {
		t.Fatalf("expected download error for missing attachment, got %+v", attRes)
	}
	fakeMCP.mu.Lock()
	if fakeMCP.launchCount != 1 {
		fakeMCP.mu.Unlock()
		t.Fatal("attachment download error must fail before calling t3_thread_launch")
	}
	fakeMCP.mu.Unlock()

	// 3. Live SSE thread_send (steer) passes clientRequestId.
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-steer",
		Action:    "thread_send",
		Payload:   json.RawMessage(`{"thread_id":"thread-101","message":"Also add tests","mode":"steer"}`),
	}
	steerRes := waitResult("cmd-steer")
	if !steerRes.OK {
		t.Fatalf("thread_send failed: %+v", steerRes)
	}
	fakeMCP.mu.Lock()
	if fakeMCP.lastSendArgs["mode"] != "steer" || fakeMCP.lastSendArgs["threadId"] != "thread-101" || fakeMCP.lastSendArgs["clientRequestId"] != "cmd-steer" {
		fakeMCP.mu.Unlock()
		t.Fatalf("unexpected thread_send args: %+v", fakeMCP.lastSendArgs)
	}
	fakeMCP.mu.Unlock()

	// 4. Progress update changing summary ONLY (isolating Step 1 -> Step 2) MUST produce distinct event_id.
	fakeMCP.mu.Lock()
	fakeMCP.threadState["thread-101"] = map[string]any{
		"thread": map[string]any{
			"threadId":           "thread-101",
			"projectId":          "proj-1",
			"title":              "Build feature X",
			"status":             "running",
			"latestRunId":        "run-1",
			"providerInstanceId": "codex",
			"model":              "gpt-6.2-astra",
			"branch":             "feat/test-v2",
			"itemCount":          1,
		},
		"recentRuns": []map[string]any{{"runId": "run-1", "status": "running"}},
		"items":      []map[string]any{{"type": "assistant_message", "text": "Step 1 complete"}},
	}
	fakeMCP.mu.Unlock()
	var prog1 serverclient.T3ThreadEvent
	for {
		ev := waitEvent("thread_progress")
		if ev.Summary == "Step 1 complete" {
			prog1 = ev
			break
		}
	}
	fakeMCP.mu.Lock()
	fakeMCP.threadState["thread-101"] = map[string]any{
		"thread": map[string]any{
			"threadId":           "thread-101",
			"projectId":          "proj-1",
			"title":              "Build feature X",
			"status":             "running",
			"latestRunId":        "run-1",
			"providerInstanceId": "codex",
			"model":              "gpt-6.2-astra",
			"branch":             "feat/test-v2",
			"itemCount":          1,
		},
		"recentRuns": []map[string]any{{"runId": "run-1", "status": "running"}},
		"items":      []map[string]any{{"type": "assistant_message", "text": "Step 2 complete"}},
	}
	fakeMCP.mu.Unlock()
	var prog2 serverclient.T3ThreadEvent
	for {
		ev := waitEvent("thread_progress")
		if ev.Summary == "Step 2 complete" {
			prog2 = ev
			break
		}
	}
	if prog1.EventID == prog2.EventID {
		t.Fatalf("expected distinct event_id on summary update: prog1=%+v prog2=%+v", prog1, prog2)
	}

	// 5. Thread enters pending question state; first pending read fails transiently (emits request_id only),
	// then next poll retrieves full questions and MUST emit updated thread_waiting_input with distinct event_id!
	fakeMCP.mu.Lock()
	fakeMCP.failPendingReadOnce = true
	fakeMCP.pendingRequests["thread-101"] = map[string]any{
		"requestId": "req-42",
		"questions": []map[string]any{
			{"id": "q1", "question": "Proceed with migration?", "options": []string{"Yes", "No"}},
		},
	}
	fakeMCP.mu.Unlock()

	var waitEv serverclient.T3ThreadEvent
	var decodedPending struct {
		RequestID string           `json:"request_id"`
		Questions []map[string]any `json:"questions"`
	}
	for {
		ev := waitEvent("thread_waiting_input")
		decodedPending = struct {
			RequestID string           `json:"request_id"`
			Questions []map[string]any `json:"questions"`
		}{}
		if err := json.Unmarshal(ev.PendingRequest, &decodedPending); err != nil {
			t.Fatalf("decode pending_request failed: %v (raw=%s)", err, ev.PendingRequest)
		}
		if decodedPending.RequestID == "req-42" && len(decodedPending.Questions) == 1 {
			waitEv = ev
			break
		}
	}
	if waitEv.EventID == "" {
		t.Fatal("missing event_id on thread_waiting_input")
	}
	// Verify no further duplicates once questions have been delivered.
	mu.Lock()
	waitingCountAfterFull := 0
	for _, ev := range events {
		if ev.Kind == "thread_waiting_input" {
			waitingCountAfterFull++
		}
	}
	mu.Unlock()
	time.Sleep(130 * time.Millisecond)
	mu.Lock()
	waitingCountLater := 0
	for _, ev := range events {
		if ev.Kind == "thread_waiting_input" {
			waitingCountLater++
		}
	}
	mu.Unlock()
	if waitingCountLater != waitingCountAfterFull {
		t.Fatalf("expected no duplicate thread_waiting_input after full delivery, grew from %d to %d", waitingCountAfterFull, waitingCountLater)
	}

	// 6. Live SSE thread_respond answers the question.
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-respond",
		Action:    "thread_respond",
		Payload:   json.RawMessage(`{"thread_id":"thread-101","request_id":"req-42","answers":{"q1":"Yes"}}`),
	}
	respRes := waitResult("cmd-respond")
	if !respRes.OK {
		t.Fatalf("thread_respond failed: %+v", respRes)
	}

	// 7. Thread finishes with PR URL + summary; t3_thread_wait(untilOutput: true) first times out (output pending on later turn),
	// so thread MUST remain tracked without emitting thread_completed until output arrives on the subsequent poll!
	fakeMCP.mu.Lock()
	fakeMCP.threadWaitTimedOut["thread-101"] = true
	fakeMCP.threadState["thread-101"] = map[string]any{
		"thread": map[string]any{
			"threadId":           "thread-101",
			"projectId":          "proj-1",
			"title":              "Build feature X",
			"link":               "[Build feature X](t3-thread://v2/env-mac-01/thread-101)",
			"status":             "completed",
			"latestRunId":        "run-1",
			"providerInstanceId": "codex",
			"model":              "gpt-6.2-astra",
			"branch":             "feat/test",
			"linkedPullRequest":  map[string]any{"url": "https://github.com/mishankov/OpenAlbert/pull/99"},
			"itemCount":          5,
		},
		"recentRuns": []map[string]any{{"runId": "run-1", "status": "completed"}},
		"items":      []map[string]any{{"type": "assistant_message", "text": "Implemented X and opened PR #99."}},
	}
	fakeMCP.mu.Unlock()

	time.Sleep(110 * time.Millisecond)
	if len(b.ActiveThreadIDs()) != 1 {
		t.Fatalf("expected thread-101 to remain tracked while structured output is pending, got %v", b.ActiveThreadIDs())
	}
	fakeMCP.mu.Lock()
	if fakeMCP.lastWaitArgs["untilOutput"] != true {
		fakeMCP.mu.Unlock()
		t.Fatalf("expected t3_thread_wait to pass untilOutput=true, got %+v", fakeMCP.lastWaitArgs)
	}
	fakeMCP.threadWaitTimedOut["thread-101"] = false
	fakeMCP.threadWaitOutput["thread-101"] = map[string]any{"done": true, "pr": 99}
	fakeMCP.mu.Unlock()

	compEv := waitEvent("thread_completed")
	if compEv.PRURL != "https://github.com/mishankov/OpenAlbert/pull/99" || !strings.Contains(compEv.Summary, "PR #99") {
		t.Fatalf("unexpected thread_completed event: %+v", compEv)
	}
	if !strings.Contains(string(compEv.Output), `"pr":99`) {
		t.Fatalf("expected structured output retrieved via t3_thread_wait, got %s", compEv.Output)
	}
	// Idle watcher produces zero MCP chatter.
	time.Sleep(80 * time.Millisecond)
	if len(b.ActiveThreadIDs()) != 0 {
		t.Fatalf("expected 0 active threads after completion, got %v", b.ActiveThreadIDs())
	}
	fakeMCP.mu.Lock()
	callsAtIdle := len(fakeMCP.calls)
	fakeMCP.mu.Unlock()
	time.Sleep(130 * time.Millisecond)
	fakeMCP.mu.Lock()
	callsAfterIdle := len(fakeMCP.calls)
	fakeMCP.mu.Unlock()
	if callsAfterIdle != callsAtIdle {
		t.Fatalf("expected zero MCP chatter when idle, got %d extra calls", callsAfterIdle-callsAtIdle)
	}

	// 8a. Resilient fallback with SINGLE delivery for tool-level parent_not_active error outside turn:
	// bridge retains cmd-parent-idle locally (no early failure), and executes it on next Tick()!
	fakeMCP.mu.Lock()
	fakeMCP.failParentNotActive = true
	fakeMCP.mu.Unlock()

	sseCh <- serverclient.T3Command{
		CommandID: "cmd-parent-idle",
		Action:    "thread_control",
		Payload:   json.RawMessage(`{"thread_id":"thread-101","action":"interrupt"}`),
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	_, failedIdleEarly := cmdResults["cmd-parent-idle"]
	mu.Unlock()
	if failedIdleEarly {
		t.Fatal("parent_not_active outside turn must be deferred locally, not posted as failure")
	}

	// 8b. Also verify a cancelled Tick preserves deferred commands without caching permanent failures.
	fakeMCP.mu.Lock()
	fakeMCP.failParentNotActive = false
	fakeMCP.mu.Unlock()
	cancelledCtx, cancelTick := context.WithCancel(ctx)
	cancelTick()
	_ = b.Tick(cancelledCtx, mgr)
	mu.Lock()
	_, failedOnCancel := cmdResults["cmd-parent-idle"]
	mu.Unlock()
	if failedOnCancel {
		t.Fatal("cancelled tick must not fail retained deferred commands")
	}

	// Now run a healthy Tick -> retained cmd-parent-idle executes and succeeds!
	if err := b.Tick(ctx, mgr); err != nil {
		t.Fatalf("Tick failed: %v", err)
	}
	idleRes := waitResult("cmd-parent-idle")
	if !idleRes.OK {
		t.Fatalf("locally retained parent_not_active command should succeed on Tick: %+v", idleRes)
	}

	// 8c. Resilient fallback with SINGLE delivery for HTTP 503 MCP transport error outside turn.
	fakeMCP.mu.Lock()
	fakeMCP.failOutsideTurn = true
	fakeMCP.mu.Unlock()

	sseCh <- serverclient.T3Command{
		CommandID: "cmd-deferred",
		Action:    "thread_control",
		Payload:   json.RawMessage(`{"thread_id":"thread-101","action":"interrupt"}`),
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	_, failedEarly := cmdResults["cmd-deferred"]
	mu.Unlock()
	if failedEarly {
		t.Fatal("command should have been retained locally on background MCP transport error, not failed early")
	}

	fakeMCP.mu.Lock()
	fakeMCP.failOutsideTurn = false
	fakeMCP.mu.Unlock()
	if err := b.Tick(ctx, mgr); err != nil {
		t.Fatalf("Tick failed: %v", err)
	}
	deferredRes := waitResult("cmd-deferred")
	if !deferredRes.OK {
		t.Fatalf("locally retained deferred command should succeed on Tick: %+v", deferredRes)
	}

	// 9. Verify thread_failed classification via thread_control(sync).
	fakeMCP.mu.Lock()
	fakeMCP.threadState["thread-202"] = map[string]any{
		"thread": map[string]any{
			"threadId": "thread-202",
			"status":   "failed",
			"error":    "build failed",
		},
		"recentRuns": []map[string]any{{"runId": "run-2", "status": "failed", "error": "build failed"}},
	}
	fakeMCP.mu.Unlock()
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-sync-fail",
		Action:    "thread_control",
		Payload:   json.RawMessage(`{"thread_id":"thread-202","action":"sync"}`),
	}
	syncFailRes := waitResult("cmd-sync-fail")
	if !syncFailRes.OK {
		t.Fatalf("thread_control(sync) failed: %+v", syncFailRes)
	}
	failEv := waitEvent("thread_failed")
	if failEv.ThreadID != "thread-202" || failEv.Error != "build failed" {
		t.Fatalf("unexpected thread_failed event: %+v", failEv)
	}

	if badAuthCount.Load() != 0 {
		t.Fatalf("unexpected unauthorized requests: %d", badAuthCount.Load())
	}
	mu.Lock()
	hbCount := len(heartbeats)
	mu.Unlock()
	if hbCount == 0 {
		t.Fatal("expected at least one heartbeat")
	}
}

func TestBridgeStateEdgeCases(t *testing.T) {
	// S14: Cancelled duplicate execution must not clear an existing in-flight claim.
	t.Run("cancelled duplicate preserves in-flight claim", func(t *testing.T) {
		cfg := config.Config{
			ServerURL:      "http://127.0.0.1:8080",
			Token:          "tok",
			StateDir:       t.TempDir(),
			CustomStateDir: true,
		}
		b, ok, err := New(cfg, serverclient.New(cfg.ServerURL, cfg.Token, time.Second), nil)
		if err != nil || !ok {
			t.Fatalf("New failed: ok=%v err=%v", ok, err)
		}
		defer b.Close()

		if !b.beginCommand("cmd-inflight") {
			t.Fatal("expected initial beginCommand to acquire claim")
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		b.executeCommand(cancelled, nil, serverclient.T3Command{CommandID: "cmd-inflight", Action: "thread_read"}, false)

		b.mu.Lock()
		stillInFlight := b.inFlightCmds["cmd-inflight"]
		deferredCount := len(b.deferredCmds)
		b.mu.Unlock()
		if !stillInFlight || deferredCount != 0 {
			t.Fatalf("cancelled duplicate must preserve in-flight claim and not queue duplicate: inFlight=%v deferred=%d", stillInFlight, deferredCount)
		}
	})

	// S5 (without t3_thread_wait), S15 (follow-up run does not inherit output_schema),
	// and S16 (subsequent pending-request read failure does not erase delivered questions).
	t.Run("schema retention without wait tool, follow-up run reset, and pending-read preservation", func(t *testing.T) {
		fakeMCP := newFakeT3MCP("env-edge")
		mcpNoWait := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			if req.Method == "tools/list" {
				// Intentionally omit t3_thread_wait to verify S5 when wait tool is unavailable.
				names := []string{"t3_environment_read", "t3_thread_launch", "t3_thread_send", "t3_thread_read", "t3_pending_request"}
				tools := make([]map[string]any, 0, len(names))
				for _, n := range names {
					tools = append(tools, map[string]any{"name": n, "description": n, "inputSchema": map[string]any{"type": "object"}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": tools}})
				return
			}
			// Delegate remaining methods to fakeMCP by re-encoding body.
			raw, _ := json.Marshal(req)
			r2 := r.Clone(r.Context())
			r2.Body = http.NoBody
			r2.ContentLength = int64(len(raw))
			// Handle inline via fakeMCP.handleTool / standard handshake.
			switch req.Method {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/call":
				var p struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				}
				_ = json.Unmarshal(req.Params, &p)
				fakeMCP.mu.Lock()
				if p.Name == "t3_pending_request" && p.Arguments["action"] == "read" && fakeMCP.failPendingReadOnce {
					fakeMCP.failPendingReadOnce = false
					fakeMCP.mu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0", "id": req.ID,
						"result": map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "transient read error"}}},
					})
					return
				}
				fakeMCP.mu.Unlock()
				out := fakeMCP.handleTool(p.Name, p.Arguments)
				b, _ := json.Marshal(out)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(b)}}},
				})
			}
		}))
		defer mcpNoWait.Close()

		mgr, warnings := mcpclient.Connect(context.Background(), []mcpclient.ServerConfig{
			{Name: "t3-code", Type: "http", URL: mcpNoWait.URL},
		}, nil)
		if len(warnings) > 0 {
			t.Fatalf("MCP connect warnings: %v", warnings)
		}
		defer mgr.Close()

		var (
			mu     sync.Mutex
			events []serverclient.T3ThreadEvent
		)
		piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/t3/hosts/register":
				_ = json.NewEncoder(w).Encode(serverclient.T3HostRegisterResponse{HostID: "h1", EnvironmentID: "env-edge"})
			case "/api/v1/t3/events":
				var ev serverclient.T3ThreadEvent
				_ = json.NewDecoder(r.Body).Decode(&ev)
				mu.Lock()
				events = append(events, ev)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}
		}))
		defer piSrv.Close()

		cfg := config.Config{ServerURL: piSrv.URL, Token: "tok", StateDir: t.TempDir(), CustomStateDir: true}
		b, ok, err := New(cfg, serverclient.New(cfg.ServerURL, cfg.Token, 5*time.Second), nil)
		if err != nil || !ok {
			t.Fatalf("New failed: ok=%v err=%v", ok, err)
		}
		defer b.Close()

		ctx := context.Background()
		if _, err := b.syncCapabilities(ctx, mgr); err != nil {
			t.Fatalf("syncCapabilities: %v", err)
		}

		// Launch with output_schema.
		if _, err := b.handleThreadLaunch(ctx, mgr, "c1", json.RawMessage(`{"title":"Schema Task","output_schema":{"type":"object"}}`)); err != nil {
			t.Fatalf("launch: %v", err)
		}
		// S16: Deliver full pending question, then fail a subsequent pending read -> must NOT downgrade or emit partial event!
		fakeMCP.mu.Lock()
		fakeMCP.pendingRequests["thread-101"] = map[string]any{
			"requestId": "req-7",
			"questions": []map[string]any{{"id": "q1", "question": "Confirm?"}},
		}
		fakeMCP.mu.Unlock()
		if _, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false); err != nil {
			t.Fatalf("poll waiting 1: %v", err)
		}
		mu.Lock()
		countAfterFull := len(events)
		mu.Unlock()
		fakeMCP.mu.Lock()
		fakeMCP.failPendingReadOnce = true
		fakeMCP.mu.Unlock()
		if _, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false); err != nil {
			t.Fatalf("poll waiting 2: %v", err)
		}
		mu.Lock()
		countAfterTransientFail := len(events)
		mu.Unlock()
		if countAfterTransientFail != countAfterFull {
			t.Fatalf("subsequent pending read failure must preserve delivered questions without emitting partial event (events %d -> %d)", countAfterFull, countAfterTransientFail)
		}

		// S17: Request changes from req-7 to req-8 and req-8 read fails twice -> MUST NOT resurrect req-7's payload!
		fakeMCP.mu.Lock()
		fakeMCP.pendingRequests["thread-101"] = map[string]any{
			"requestId": "req-8",
			"questions": []map[string]any{{"id": "q2", "question": "Second?"}},
		}
		fakeMCP.failPendingReadOnce = true
		fakeMCP.mu.Unlock()
		evB1, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		if err != nil {
			t.Fatalf("poll req-8 fail 1: %v", err)
		}
		fakeMCP.mu.Lock()
		fakeMCP.failPendingReadOnce = true
		fakeMCP.mu.Unlock()
		evB2, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		if err != nil {
			t.Fatalf("poll req-8 fail 2: %v", err)
		}
		if strings.Contains(string(evB1.PendingRequest), "req-7") || strings.Contains(string(evB2.PendingRequest), "req-7") ||
			!strings.Contains(string(evB2.PendingRequest), `"request_id":"req-8"`) {
			t.Fatalf("failed read of req-8 must never reuse req-7 payload: b1=%s b2=%s", evB1.PendingRequest, evB2.PendingRequest)
		}

		// S16 concurrent polls: two goroutines polling the same thread serialize so a failing poll cannot overwrite a full payload.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		}()
		go func() {
			defer wg.Done()
			_, _ = b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		}()
		wg.Wait()
		mu.Lock()
		lastEv := events[len(events)-1]
		mu.Unlock()
		if !strings.Contains(string(lastEv.PendingRequest), `"Second?"`) {
			t.Fatalf("expected final serialized event to contain req-8 questions, got %s", lastEv.PendingRequest)
		}

		// Clear pending request.
		fakeMCP.mu.Lock()
		delete(fakeMCP.pendingRequests, "thread-101")
		// S5 without t3_thread_wait: run-1 completes with no output yet -> MUST stay tracked, NOT emit thread_completed!
		fakeMCP.threadState["thread-101"] = map[string]any{
			"thread":     map[string]any{"threadId": "thread-101", "status": "completed", "latestRunId": "run-1"},
			"recentRuns": []map[string]any{{"runId": "run-1", "status": "completed"}},
		}
		fakeMCP.mu.Unlock()
		ev, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		if err != nil {
			t.Fatalf("poll schema completed without wait tool: %v", err)
		}
		if ev.Kind == "thread_completed" || len(b.ActiveThreadIDs()) != 1 {
			t.Fatalf("schema-bearing run without wait tool must stay tracked until output arrives: kind=%s active=%v", ev.Kind, b.ActiveThreadIDs())
		}

		// S15a: Plain follow-up send (non-steer, no output_schema) starts run-2 -> completing run-2 MUST emit thread_completed immediately without waiting for run-1's schema!
		if _, err := b.handleThreadSend(ctx, mgr, "c2", json.RawMessage(`{"thread_id":"thread-101","message":"Plain follow-up","mode":"queue"}`)); err != nil {
			t.Fatalf("plain follow-up send: %v", err)
		}
		fakeMCP.mu.Lock()
		fakeMCP.threadState["thread-101"] = map[string]any{
			"thread":     map[string]any{"threadId": "thread-101", "status": "completed", "latestRunId": "run-2"},
			"recentRuns": []map[string]any{{"runId": "run-2", "status": "completed"}},
		}
		fakeMCP.mu.Unlock()
		ev2, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		if err != nil {
			t.Fatalf("poll plain follow-up: %v", err)
		}
		if ev2.Kind != "thread_completed" || len(b.ActiveThreadIDs()) != 0 {
			t.Fatalf("plain follow-up run must complete normally without inheriting earlier output_schema: kind=%s active=%v", ev2.Kind, b.ActiveThreadIDs())
		}

		// S15b: Thread is now UNTRACKED after run-2 completed. A queued schema-bearing send (omitting runId)
		// while preceding run-2 (even if failed) is still visible MUST NOT bind to run-2 or untrack;
		// when run-3 later completes without output, the bridge MUST keep tracking until run-3's output arrives!
		if _, err := b.handleThreadSend(ctx, mgr, "c3", json.RawMessage(`{"thread_id":"thread-101","message":"Queued schema run","mode":"queue","output_schema":{"type":"object"}}`)); err != nil {
			t.Fatalf("queued schema send: %v", err)
		}
		// Poll 1 still sees preceding run-2 (simulate failed predecessor too) -> must NOT bind schema to run-2 or untrack!
		fakeMCP.mu.Lock()
		fakeMCP.threadState["thread-101"] = map[string]any{
			"thread":     map[string]any{"threadId": "thread-101", "status": "failed", "latestRunId": "run-2"},
			"recentRuns": []map[string]any{{"runId": "run-2", "status": "failed"}},
		}
		fakeMCP.mu.Unlock()
		evPrev, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		if err != nil {
			t.Fatalf("poll preceding run-2: %v", err)
		}
		if evPrev.Kind == "thread_completed" || evPrev.Kind == "thread_failed" || len(b.ActiveThreadIDs()) != 1 {
			t.Fatalf("queued schema run must not terminate on preceding run-2: kind=%s active=%v", evPrev.Kind, b.ActiveThreadIDs())
		}
		// Poll 2 sees queued run-3 completed without output -> MUST bind schema to run-3 and remain tracked!
		fakeMCP.mu.Lock()
		fakeMCP.threadState["thread-101"] = map[string]any{
			"thread":     map[string]any{"threadId": "thread-101", "status": "completed", "latestRunId": "run-3"},
			"recentRuns": []map[string]any{{"runId": "run-3", "status": "completed"}},
		}
		fakeMCP.mu.Unlock()
		evRun3, err := b.pollSingleThread(ctx, mgr, "thread-101", false, false)
		if err != nil {
			t.Fatalf("poll queued run-3: %v", err)
		}
		if evRun3.Kind == "thread_completed" || len(b.ActiveThreadIDs()) != 1 {
			t.Fatalf("queued schema run-3 without output must remain tracked: kind=%s active=%v", evRun3.Kind, b.ActiveThreadIDs())
		}
	})

	// S18: A terminal poll finishing its event POST must not delete a concurrent thread_send's newer tracking generation.
	t.Run("terminal poll does not discard concurrent send", func(t *testing.T) {
		fakeMCP := newFakeT3MCP("env-s18")
		mcpSrv := httptest.NewServer(fakeMCP.handler())
		defer mcpSrv.Close()
		mgr, warnings := mcpclient.Connect(context.Background(), []mcpclient.ServerConfig{
			{Name: "t3-code", Type: "http", URL: mcpSrv.URL},
		}, nil)
		if len(warnings) > 0 {
			t.Fatalf("MCP warnings: %v", warnings)
		}
		defer mgr.Close()

		inEventPost := make(chan struct{})
		releaseEventPost := make(chan struct{})
		var eventPostHit atomic.Bool
		piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/t3/hosts/register":
				_ = json.NewEncoder(w).Encode(serverclient.T3HostRegisterResponse{HostID: "h1", EnvironmentID: "env-s18"})
			case "/api/v1/t3/events":
				if !eventPostHit.Swap(true) {
					close(inEventPost)
					<-releaseEventPost
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}
		}))
		defer piSrv.Close()

		cfg := config.Config{ServerURL: piSrv.URL, Token: "tok", StateDir: t.TempDir(), CustomStateDir: true}
		b, ok, err := New(cfg, serverclient.New(cfg.ServerURL, cfg.Token, 5*time.Second), nil)
		if err != nil || !ok {
			t.Fatalf("New failed: ok=%v err=%v", ok, err)
		}
		defer b.Close()

		ctx := context.Background()
		if _, err := b.syncCapabilities(ctx, mgr); err != nil {
			t.Fatalf("syncCapabilities: %v", err)
		}
		b.trackThread(&trackedThread{threadID: "thread-s18", wasActive: true, lastObservedRunID: "run-2"})
		fakeMCP.mu.Lock()
		fakeMCP.threadState["thread-s18"] = map[string]any{
			"thread":     map[string]any{"threadId": "thread-s18", "status": "completed", "latestRunId": "run-2"},
			"recentRuns": []map[string]any{{"runId": "run-2", "status": "completed"}},
		}
		fakeMCP.mu.Unlock()

		pollDone := make(chan error, 1)
		go func() {
			_, err := b.pollSingleThread(ctx, mgr, "thread-s18", false, true)
			pollDone <- err
		}()

		<-inEventPost
		// While terminal event POST is in flight, a foreground thread_send registers run-3!
		if _, err := b.handleThreadSend(ctx, mgr, "cmd-s18", json.RawMessage(`{"thread_id":"thread-s18","message":"Follow-up run 3","mode":"queue"}`)); err != nil {
			t.Fatalf("concurrent thread_send failed: %v", err)
		}
		close(releaseEventPost)
		if err := <-pollDone; err != nil {
			t.Fatalf("poll failed: %v", err)
		}
		if ids := b.ActiveThreadIDs(); len(ids) != 1 || ids[0] != "thread-s18" {
			t.Fatalf("terminal poll must NOT delete concurrent send's tracking record, got active=%v", ids)
		}
	})
}

func TestNormalizeModelSelectionMapsModelIDAndProviderInstanceID(t *testing.T) {
	got := normalizeModelSelection(map[string]any{
		"provider_instance_id": "antigravity",
		"model_id":             "gemini-4-neon",
	})
	if got["instanceId"] != "antigravity" || got["model"] != "gemini-4-neon" {
		t.Fatalf("unexpected normalized modelSelection: %+v", got)
	}
	if _, hasOldModelID := got["model_id"]; hasOldModelID {
		t.Fatalf("normalized modelSelection must not retain model_id key: %+v", got)
	}
	if _, hasOldProvID := got["provider_instance_id"]; hasOldProvID {
		t.Fatalf("normalized modelSelection must not retain provider_instance_id key: %+v", got)
	}

	parsed := parseLaunchResponse(
		json.RawMessage(`{"threadId":"th-1","modelSelection":{"instanceId":"antigravity","model":"gemini-4-neon"}}`),
		threadLaunchPayload{ModelSelection: map[string]any{"provider_instance_id": "antigravity", "model_id": "gemini-4-neon"}},
	)
	if parsed.Model != "gemini-4-neon" || parsed.ProviderInstanceID != "antigravity" {
		t.Fatalf("unexpected parsed launch response: %+v", parsed)
	}
}
