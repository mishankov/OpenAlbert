package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/acp/internal/config"
	"openalbert/apps/acp/internal/serverclient"
	"openalbert/apps/acp/internal/session"
)

func TestEndToEndACPCodemodeSkillsMCPPermissionsAndInspectorSync(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, ".agents", "skills", "greeter")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: greeter\ndescription: Say hello properly\n---\nAlways greet with Hello ACP!"), 0644); err != nil {
		t.Fatal(err)
	}

	// Fake HTTP MCP server simulating T3 Code's injected MCP server.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"tools": []map[string]any{{"name": "echo", "description": "Echo tool", "inputSchema": map[string]any{"type": "object"}}},
			}})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []map[string]string{{"type": "text", "text": `{"mcp_echo":"ok"}`}},
			}})
		}
	}))
	defer mcpSrv.Close()

	// Fake OpenAlbert server implementing the gateway, tools, and Inspector sync APIs.
	var (
		mu              sync.Mutex
		syncedMsgs      int
		syncedTraces    int
		createdSessions []string
		closedSessions  []string
		serverToolHit   bool
		llmStep         int
	)
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(serverclient.ModelsResponse{
				DefaultModel: "proxy/gpt-5.4",
				Models: []serverclient.CatalogModel{
					{ID: "proxy/gpt-5.4", Provider: "proxy", Model: "gpt-5.4", DisplayName: "GPT 5.4", ContextWindowTokens: 128000, MaxOutputTokens: 8192, SupportedReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, Default: true},
					{ID: "chatgpt/codex-5", Provider: "chatgpt", Model: "codex-5", DisplayName: "Codex 5", ContextWindowTokens: 128000, MaxOutputTokens: 8192},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(serverclient.AgentContext{
				OwnerID: 42,
				Soul:    "Be concise and thorough.",
				Tools: []serverclient.Tool{
					{Name: "memory_update", Description: "Save memory", Parameters: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}}}`)},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/tools/memory_update":
			mu.Lock()
			serverToolHit = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.ToolResult{Output: json.RawMessage(`{"saved":true}`)})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/coding/sessions":
			var req struct {
				SessionID      string `json:"session_id"`
				ConversationID string `json:"conversation_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			createdSessions = append(createdSessions, req.SessionID+"|"+req.ConversationID)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sync"):
			var req serverclient.SyncRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			syncedMsgs += len(req.Messages)
			syncedTraces += len(req.Traces)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			mu.Lock()
			closedSessions = append(closedSessions, r.URL.Path)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"closed": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			var req serverclient.CompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.Tools) != 1 || req.Tools[0].Name != "codemode" {
				t.Errorf("strict codemode violated, got tools: %+v", req.Tools)
			}
			mu.Lock()
			step := llmStep
			llmStep++
			mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			send := func(ev serverclient.StreamEvent) {
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
			if step == 0 {
				if req.Model != "proxy/gpt-5.4" || req.Options.ReasoningEffort != "xhigh" {
					t.Errorf("model/effort override not applied: model=%q effort=%q", req.Model, req.Options.ReasoningEffort)
				}
				code := `
const sk = await tools.skill_invoke({ name: "greeter" });
await tools.fs_write({ path: "hello.txt", content: "Hello ACP!" });
const mcp = await tools.mcp_t3_code_echo({});
const mem = await tools.memory_update({ content: "Uses ACP" });
const sh = await tools.bash({ command: "cat hello.txt" });
return { skill: sk.name, mcp, mem, out: sh.stdout };
`
				argsRaw, _ := json.Marshal(map[string]string{"code": code})
				send(serverclient.StreamEvent{Type: "reasoning_delta", Delta: "planning..."})
				send(serverclient.StreamEvent{
					Type: "done",
					Completion: &serverclient.Completion{
						Model: req.Model,
						Message: serverclient.Message{
							Role:      "assistant",
							Reasoning: "planning...",
							ToolCalls: []serverclient.ToolCall{{ID: "cm-1", Name: "codemode", Arguments: argsRaw}},
						},
					},
				})
				return
			}
			send(serverclient.StreamEvent{Type: "text_delta", Delta: "Done! "})
			send(serverclient.StreamEvent{Type: "text_delta", Delta: "Hello ACP!"})
			send(serverclient.StreamEvent{
				Type: "done",
				Completion: &serverclient.Completion{
					Model:   req.Model,
					Message: serverclient.Message{Role: "assistant", Content: "Done! Hello ACP!"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer oaSrv.Close()

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	cfg := config.Config{
		ServerURL:      oaSrv.URL,
		Token:          "test-token",
		SyncInspector:  true,
		StateDir:       t.TempDir(),
		MaxSteps:       10,
		RequestTimeout: 10 * time.Second,
	}
	srv := NewServer(cfg, clientToServerR, serverToClientW, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()

	// Test harness reading ACP JSON-RPC frames from serverToClientR and auto-approving permission prompts.
	var (
		writeMu        sync.Mutex
		updates        []map[string]any
		permissionAsks int
		responses      = map[string]chan rpcEnvelope{}
		responsesMu    sync.Mutex
	)
	sendRPC := func(v any) {
		b, _ := json.Marshal(v)
		writeMu.Lock()
		_, _ = clientToServerW.Write(append(b, '\n'))
		writeMu.Unlock()
	}
	go func() {
		scanner := bufio.NewScanner(serverToClientR)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			var env rpcEnvelope
			if json.Unmarshal(scanner.Bytes(), &env) != nil {
				continue
			}
			if env.Method == "session/request_permission" {
				mu.Lock()
				permissionAsks++
				mu.Unlock()
				sendRPC(map[string]any{
					"jsonrpc": "2.0",
					"id":      env.ID,
					"result":  map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}},
				})
				continue
			}
			if env.Method == "session/update" {
				var p map[string]any
				_ = json.Unmarshal(env.Params, &p)
				mu.Lock()
				updates = append(updates, p)
				mu.Unlock()
				continue
			}
			if len(env.ID) > 0 {
				var key string
				if json.Unmarshal(env.ID, &key) != nil {
					key = string(env.ID)
				}
				responsesMu.Lock()
				ch := responses[key]
				delete(responses, key)
				responsesMu.Unlock()
				if ch != nil {
					ch <- env
				}
			}
		}
	}()

	call := func(id, method string, params any) rpcEnvelope {
		t.Helper()
		ch := make(chan rpcEnvelope, 1)
		responsesMu.Lock()
		responses[id] = ch
		responsesMu.Unlock()
		sendRPC(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		select {
		case resp := <-ch:
			if resp.Error != nil {
				t.Fatalf("%s error: %+v", method, resp.Error)
			}
			return resp
		case <-time.After(15 * time.Second):
			t.Fatalf("timeout waiting for %s", method)
			return rpcEnvelope{}
		}
	}

	// 1. initialize (must not advertise unsupported mcpCapabilities.sse)
	initResp := call("1", "initialize", map[string]any{"protocolVersion": 1})
	if !strings.Contains(string(initResp.Result), `"openalbert-acp"`) || strings.Contains(string(initResp.Result), `"sse"`) {
		t.Fatalf("unexpected initialize result: %s", initResp.Result)
	}

	// 2. session/new with MCP server
	newResp := call("2", "session/new", map[string]any{
		"cwd": workspace,
		"mcpServers": []map[string]any{
			{"name": "t3-code", "type": "http", "url": mcpSrv.URL},
		},
	})
	var newOut struct {
		SessionID string `json:"sessionId"`
		Models    struct {
			CurrentModelID  string `json:"currentModelId"`
			AvailableModels []any  `json:"availableModels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(newResp.Result, &newOut); err != nil || newOut.SessionID == "" || len(newOut.Models.AvailableModels) != 2 {
		t.Fatalf("unexpected session/new result: %s", newResp.Result)
	}
	if !strings.Contains(string(newResp.Result), `"xhigh"`) {
		t.Fatalf("expected xhigh reasoning_effort option for proxy/gpt-5.4: %s", newResp.Result)
	}

	// 3. Switch to chatgpt/codex-5 (no reasoning_effort option), verify omission, then switch back to proxy/gpt-5.4 with xhigh.
	switchSub := call("3a", "session/set_config_option", map[string]any{"sessionId": newOut.SessionID, "configId": "model", "value": "chatgpt/codex-5"})
	if strings.Contains(string(switchSub.Result), `"reasoning_effort"`) {
		t.Fatalf("chatgpt/codex-5 must not expose reasoning_effort option: %s", switchSub.Result)
	}
	// Setting reasoning_effort on a model that does not support it must return an RPC error.
	badCh := make(chan rpcEnvelope, 1)
	responsesMu.Lock()
	responses["3b"] = badCh
	responsesMu.Unlock()
	sendRPC(map[string]any{"jsonrpc": "2.0", "id": "3b", "method": "session/set_config_option", "params": map[string]any{"sessionId": newOut.SessionID, "configId": "reasoning_effort", "value": "high"}})
	if badResp := <-badCh; badResp.Error == nil {
		t.Fatalf("expected error setting reasoning_effort on chatgpt/codex-5, got %s", badResp.Result)
	}
	call("3", "session/set_config_option", map[string]any{"sessionId": newOut.SessionID, "configId": "model", "value": "proxy/gpt-5.4"})
	call("4", "session/set_config_option", map[string]any{"sessionId": newOut.SessionID, "configId": "reasoning_effort", "value": "xhigh"})

	// 4. Prompt running strict Codemode across skill_invoke + fs_write + MCP + memory_update + bash
	promptResp := call("5", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "Greet and verify"}},
	})
	if !strings.Contains(string(promptResp.Result), `"end_turn"`) {
		t.Fatalf("unexpected prompt result: %s", promptResp.Result)
	}

	// Close client pipe to trigger graceful shutdown & Inspector session close.
	_ = clientToServerW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exited with error: %v", err)
	}

	// Verify real file created in workspace.
	gotFile, err := os.ReadFile(filepath.Join(workspace, "hello.txt"))
	if err != nil || string(gotFile) != "Hello ACP!" {
		t.Fatalf("workspace file not written: %q %v", string(gotFile), err)
	}

	mu.Lock()
	if !serverToolHit {
		mu.Unlock()
		t.Fatal("OpenAlbert server tool memory_update was not called")
	}
	if permissionAsks < 3 {
		got := permissionAsks
		mu.Unlock()
		t.Fatalf("expected permission prompts for mutating/exec tools in default mode, got %d", got)
	}
	if syncedMsgs < 10 || syncedTraces < 4 {
		m, tr := syncedMsgs, syncedTraces
		mu.Unlock()
		t.Fatalf("expected full messages & traces synced to Inspector, got msgs=%d traces=%d", m, tr)
	}
	if len(closedSessions) != 1 {
		cs := append([]string(nil), closedSessions...)
		mu.Unlock()
		t.Fatalf("expected remote coding session to close for analysis on exit, got %v", cs)
	}
	hasCmds, hasDiff, hasMsgChunk := false, false, false
	for _, u := range updates {
		raw, _ := json.Marshal(u)
		s := string(raw)
		if strings.Contains(s, "available_commands_update") && strings.Contains(s, "greeter") {
			hasCmds = true
		}
		if strings.Contains(s, `"type":"diff"`) && strings.Contains(s, "Hello ACP!") {
			hasDiff = true
		}
		if strings.Contains(s, "agent_message_chunk") && strings.Contains(s, "Hello ACP!") {
			hasMsgChunk = true
		}
	}
	mu.Unlock()
	if !hasCmds || !hasDiff || !hasMsgChunk {
		t.Fatalf("missing expected ACP updates: cmds=%v diff=%v msgChunk=%v", hasCmds, hasDiff, hasMsgChunk)
	}

	// 5. Reopen the closed session via session/load on a fresh server instance and verify generation -g2 is created under the same coding:<id> conversation.
	c2r, c2w := io.Pipe()
	s2r, s2w := io.Pipe()
	srv2 := NewServer(cfg, c2r, s2w, nil)
	srv2Done := make(chan error, 1)
	go func() { srv2Done <- srv2.Run(context.Background()) }()

	loadDone := make(chan rpcEnvelope, 1)
	var replayUpdates []string
	var replayMu sync.Mutex
	go func() {
		sc := bufio.NewScanner(s2r)
		sc.Buffer(make([]byte, 4096), 8<<20)
		for sc.Scan() {
			var env rpcEnvelope
			if json.Unmarshal(sc.Bytes(), &env) == nil {
				if env.Method == "session/update" {
					replayMu.Lock()
					replayUpdates = append(replayUpdates, string(env.Params))
					replayMu.Unlock()
				}
				if string(env.ID) == `"load-1"` {
					loadDone <- env
				}
			}
		}
	}()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "load-1", "method": "session/load", "params": map[string]any{"sessionId": newOut.SessionID, "cwd": workspace}})
	_, _ = c2w.Write(append(b, '\n'))
	select {
	case resp := <-loadDone:
		if resp.Error != nil {
			t.Fatalf("session/load error: %+v", resp.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for session/load")
	}
	_ = c2w.Close()
	if err := <-srv2Done; err != nil {
		t.Fatalf("srv2 exited with error: %v", err)
	}

	replayMu.Lock()
	joinedReplay := strings.Join(replayUpdates, "\n")
	replayMu.Unlock()
	if !strings.Contains(joinedReplay, `"sessionUpdate":"tool_call"`) || !strings.Contains(joinedReplay, `"sessionUpdate":"tool_call_update"`) || !strings.Contains(joinedReplay, `"type":"diff"`) {
		t.Fatalf("session/load replay missed tool calls/results/diffs: %s", joinedReplay)
	}

	mu.Lock()
	createdCopy := append([]string(nil), createdSessions...)
	mu.Unlock()
	wantGen2 := newOut.SessionID + "-g2|coding:" + newOut.SessionID
	foundGen2 := false
	for _, s := range createdCopy {
		if s == wantGen2 {
			foundGen2 = true
		}
	}
	if !foundGen2 {
		t.Fatalf("expected reopened session to create generation %q, got %+v", wantGen2, createdCopy)
	}
}

func TestTransportServeCancelsActiveHandlersOnEOFAndContextCancel(t *testing.T) {
	t.Run("EOF cancels active handler", func(t *testing.T) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		defer outR.Close()
		defer outW.Close()

		tr := NewTransport(inR, outW)
		started := make(chan struct{})
		handlerCanceled := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- tr.Serve(context.Background(), func(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) {
				close(started)
				<-ctx.Done()
				close(handlerCanceled)
			})
		}()

		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":"1","method":"session/prompt","params":{}}` + "\n"))
		<-started
		_ = inW.Close()

		select {
		case <-handlerCanceled:
		case <-time.After(2 * time.Second):
			t.Fatal("active handler was not cancelled on EOF")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Serve returned unexpected error on EOF: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not return promptly on EOF")
		}
	})

	t.Run("ctx cancel returns promptly even with open stdin", func(t *testing.T) {
		inR, inW := io.Pipe()
		defer inW.Close()
		outR, outW := io.Pipe()
		defer outR.Close()
		defer outW.Close()

		tr := NewTransport(inR, outW)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- tr.Serve(ctx, func(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) {})
		}()

		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected context.Canceled error")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve hung on context cancellation while stdin was open")
		}
	})
}

func TestOverlappingPromptsConcurrentConfigAndImageOnlyReplay(t *testing.T) {
	var (
		promptStarts = make(chan int, 4)
		release      = make(chan struct{})
		callCount    int
		callMu       sync.Mutex
	)
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(serverclient.ModelsResponse{
				DefaultModel: "proxy/gpt-5.4",
				Models: []serverclient.CatalogModel{
					{ID: "proxy/gpt-5.4", Provider: "proxy", Model: "gpt-5.4", SupportedReasoningEfforts: []string{"low", "high"}, ContextWindowTokens: 128000, MaxOutputTokens: 8192, Default: true},
					{ID: "chatgpt/codex-5", Provider: "chatgpt", Model: "codex-5", ContextWindowTokens: 128000, MaxOutputTokens: 8192},
				},
			})
		case "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(serverclient.AgentContext{OwnerID: 42, Soul: "ok"})
		case "/api/v1/llm/complete":
			callMu.Lock()
			callCount++
			n := callCount
			callMu.Unlock()
			promptStarts <- n
			if n <= 2 {
				// Hold prompt 1 and prompt 2 open until cancelled or test cleanup.
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			b, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "Saw image."}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer oaSrv.Close()
	defer close(release)

	stateDir := t.TempDir()
	workspace := t.TempDir()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	cfg := config.Config{ServerURL: oaSrv.URL, Token: "tok", SyncInspector: false, StateDir: stateDir, MaxSteps: 5, RequestTimeout: 10 * time.Second}
	srv := NewServer(cfg, c2sR, s2cW, nil)
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(context.Background()) }()

	var (
		writeMu     sync.Mutex
		responsesMu sync.Mutex
		responses   = map[string]chan rpcEnvelope{}
		updatesMu   sync.Mutex
		updates     []string
	)
	sendRPC := func(v any) {
		b, _ := json.Marshal(v)
		writeMu.Lock()
		_, _ = c2sW.Write(append(b, '\n'))
		writeMu.Unlock()
	}
	go func() {
		sc := bufio.NewScanner(s2cR)
		sc.Buffer(make([]byte, 4096), 8<<20)
		for sc.Scan() {
			var env rpcEnvelope
			if json.Unmarshal(sc.Bytes(), &env) != nil {
				continue
			}
			if env.Method == "session/update" {
				updatesMu.Lock()
				updates = append(updates, string(env.Params))
				updatesMu.Unlock()
				continue
			}
			if len(env.ID) > 0 {
				var key string
				if json.Unmarshal(env.ID, &key) != nil {
					key = string(env.ID)
				}
				responsesMu.Lock()
				ch := responses[key]
				delete(responses, key)
				responsesMu.Unlock()
				if ch != nil {
					ch <- env
				}
			}
		}
	}()
	startCall := func(id, method string, params any) chan rpcEnvelope {
		ch := make(chan rpcEnvelope, 1)
		responsesMu.Lock()
		responses[id] = ch
		responsesMu.Unlock()
		sendRPC(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		return ch
	}

	newResp := <-startCall("new", "session/new", map[string]any{"cwd": workspace})
	var newOut struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newResp.Result, &newOut)

	// Finding 6: concurrent model/mode/config updates must be race-free.
	var cfgWG sync.WaitGroup
	for i := 0; i < 6; i++ {
		i := i
		cfgWG.Add(1)
		go func() {
			defer cfgWG.Done()
			id := fmt.Sprintf("cfg-%d", i)
			switch i % 3 {
			case 0:
				<-startCall(id, "session/set_model", map[string]any{"sessionId": newOut.SessionID, "modelId": "chatgpt/codex-5"})
			case 1:
				<-startCall(id, "session/set_mode", map[string]any{"sessionId": newOut.SessionID, "modeId": "auto-accept-edits"})
			case 2:
				<-startCall(id, "session/set_config_option", map[string]any{"sessionId": newOut.SessionID, "configId": "model", "value": "proxy/gpt-5.4"})
			}
		}()
	}
	cfgWG.Wait()

	// Finding 5: overlapping prompts serialize and cancel still reaches the replacement prompt.
	p1Ch := startCall("p1", "session/prompt", map[string]any{"sessionId": newOut.SessionID, "prompt": []map[string]any{{"type": "text", "text": "first"}}})
	<-promptStarts // prompt 1 running
	p2Ch := startCall("p2", "session/prompt", map[string]any{"sessionId": newOut.SessionID, "prompt": []map[string]any{{"type": "text", "text": "second"}}})
	if r1 := <-p1Ch; !strings.Contains(string(r1.Result), `"cancelled"`) {
		t.Fatalf("expected prompt 1 cancelled by replacement, got %s %+v", r1.Result, r1.Error)
	}
	<-promptStarts // prompt 2 now running
	sendRPC(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": newOut.SessionID}})
	select {
	case r2 := <-p2Ch:
		if !strings.Contains(string(r2.Result), `"cancelled"`) {
			t.Fatalf("expected prompt 2 cancelled by session/cancel, got %s %+v", r2.Result, r2.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement prompt lost its cancellation handler")
	}

	// Finding 14: image-only prompt is persisted and replayed on session/load.
	p3 := <-startCall("p3", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgo="}},
	})
	if p3.Error != nil || !strings.Contains(string(p3.Result), `"end_turn"`) {
		t.Fatalf("image-only prompt failed: %s %+v", p3.Result, p3.Error)
	}

	updatesMu.Lock()
	updates = nil
	updatesMu.Unlock()
	loadResp := <-startCall("load", "session/load", map[string]any{"sessionId": newOut.SessionID, "cwd": workspace})
	if loadResp.Error != nil {
		t.Fatalf("session/load failed: %+v", loadResp.Error)
	}
	updatesMu.Lock()
	replayed := strings.Join(updates, "\n")
	updatesMu.Unlock()
	if !strings.Contains(replayed, `"type":"image"`) || !strings.Contains(replayed, `iVBORw0KGgo=`) || !strings.Contains(replayed, `Saw image.`) {
		t.Fatalf("image-only prompt or done-only assistant answer disappeared on session/load replay: %s", replayed)
	}

	// P2-4: loading a legacy record (Messages without ReplayUpdates), adding a new turn, and reloading preserves BOTH legacy and new history.
	store := session.NewStore(stateDir)
	if err := store.Save(session.Record{
		ID:    "legacy-1",
		CWD:   workspace,
		Model: "proxy/gpt-5.4",
		Mode:  "default",
		Messages: []serverclient.Message{
			{Role: "user", Content: "Original legacy question"},
			{Role: "assistant", Content: "Original legacy answer"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if r := <-startCall("load-leg-1", "session/load", map[string]any{"sessionId": "legacy-1", "cwd": workspace}); r.Error != nil {
		t.Fatalf("legacy load 1 failed: %+v", r.Error)
	}
	if r := <-startCall("prompt-leg", "session/prompt", map[string]any{"sessionId": "legacy-1", "prompt": []map[string]any{{"type": "text", "text": "Follow-up after legacy"}}}); r.Error != nil {
		t.Fatalf("legacy follow-up prompt failed: %+v", r.Error)
	}
	updatesMu.Lock()
	updates = nil
	updatesMu.Unlock()
	if r := <-startCall("load-leg-2", "session/load", map[string]any{"sessionId": "legacy-1", "cwd": workspace}); r.Error != nil {
		t.Fatalf("legacy load 2 failed: %+v", r.Error)
	}
	updatesMu.Lock()
	legReplay := strings.Join(updates, "\n")
	updatesMu.Unlock()
	if !strings.Contains(legReplay, "Original legacy question") || !strings.Contains(legReplay, "Follow-up after legacy") {
		t.Fatalf("legacy history lost after adding a new turn: %s", legReplay)
	}

	_ = c2sW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exit error: %v", err)
	}
}

func TestCancelDuringReplacementWaitAndReconcileLostCloseAck(t *testing.T) {
	var (
		mu            sync.Mutex
		createdIDs    []string
		completeCalls int
	)
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(serverclient.ModelsResponse{
				DefaultModel: "proxy/gpt-5.4",
				Models:       []serverclient.CatalogModel{{ID: "proxy/gpt-5.4", Provider: "proxy", Model: "gpt-5.4", ContextWindowTokens: 128000, MaxOutputTokens: 8192, Default: true}},
			})
		case "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(serverclient.AgentContext{OwnerID: 42, Soul: "ok"})
		case "/api/v1/coding/sessions":
			var req struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			createdIDs = append(createdIDs, req.SessionID)
			mu.Unlock()
			// Simulate lost close ack: g1 is already closed on backend.
			if req.SessionID == "lost-ack-1" {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "session_closed", "message": "already closed"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/api/v1/llm/complete":
			mu.Lock()
			completeCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			b, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "unexpected"}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer oaSrv.Close()

	stateDir := t.TempDir()
	workspace := t.TempDir()
	store := session.NewStore(stateDir)
	if err := store.Save(session.Record{
		ID:              "lost-ack-1",
		ServerSessionID: "lost-ack-1",
		Generation:      1,
		Closed:          false, // Local file missed close ack
		CWD:             workspace,
		Model:           "proxy/gpt-5.4",
		Mode:            "default",
	}); err != nil {
		t.Fatal(err)
	}

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	defer c2sW.Close()
	defer s2cR.Close()
	go func() { _, _ = io.Copy(io.Discard, s2cR) }()

	cfg := config.Config{ServerURL: oaSrv.URL, Token: "tok", SyncInspector: true, StateDir: stateDir, MaxSteps: 5, RequestTimeout: 10 * time.Second}
	srv := NewServer(cfg, c2sR, s2cW, nil)

	// P1-6: load reconciles 409 session_closed to generation -g2 and persists it.
	_, sess, err := srv.handleSessionLoad(context.Background(), json.RawMessage(`{"sessionId":"lost-ack-1","cwd":`+fmt.Sprintf("%q", workspace)+`}`))
	if err != nil {
		t.Fatalf("handleSessionLoad failed: %v", err)
	}
	if sess.ServerSessionID != "lost-ack-1-g2" || sess.Generation != 2 {
		t.Fatalf("expected reconciled generation -g2, got id=%q gen=%d", sess.ServerSessionID, sess.Generation)
	}

	// P1-3: cancel arriving while replacement prompt waits for previous prompt's cleanup cancels the replacement before RunTurn.
	prevDone := make(chan struct{})
	prevCanceled := make(chan struct{})
	prevHandle := &promptHandle{
		kind: opPrompt,
		cancel: func() {
			select {
			case <-prevCanceled:
			default:
				close(prevCanceled)
			}
		},
		done: prevDone,
	}
	srv.mu.Lock()
	srv.sessionOps[sess.ID] = prevHandle
	srv.activePrompts[sess.ID] = prevHandle
	srv.mu.Unlock()

	replRes := make(chan any, 1)
	go func() {
		res, _ := srv.handlePrompt(context.Background(), json.RawMessage(`{"sessionId":"lost-ack-1","prompt":[{"type":"text","text":"should not run"}]}`))
		replRes <- res
	}()

	<-prevCanceled
	// Replacement is now registered and blocked on <-prevDone; send session/cancel before releasing prevDone.
	srv.handleCancel(json.RawMessage(`{"sessionId":"lost-ack-1"}`))
	close(prevDone)

	select {
	case res := <-replRes:
		raw, _ := json.Marshal(res)
		if !strings.Contains(string(raw), `"cancelled"`) {
			t.Fatalf("expected cancelled replacement prompt, got %s", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cancelled replacement prompt")
	}
	mu.Lock()
	defer mu.Unlock()
	if completeCalls != 0 {
		t.Fatalf("cancelled replacement prompt still called LLM %d times", completeCalls)
	}
}

func TestImmediateCancelAndPromptDuringReloadOnStdio(t *testing.T) {
	var (
		mu              sync.Mutex
		modelFetches    int
		llmCalls        int
		llmModels       []string
		createdSessions int
		reloadPaused    = make(chan struct{})
		releaseReload   = make(chan struct{})
		skillSessionID  string
	)
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/llm/models":
			mu.Lock()
			modelFetches++
			n := modelFetches
			mu.Unlock()
			if n == 2 {
				close(reloadPaused)
				<-releaseReload
			}
			_ = json.NewEncoder(w).Encode(serverclient.ModelsResponse{
				DefaultModel: "proxy/gpt-5.4",
				Models:       []serverclient.CatalogModel{{ID: "proxy/gpt-5.4", Provider: "proxy", Model: "gpt-5.4", ContextWindowTokens: 128000, MaxOutputTokens: 8192, Default: true}},
			})
		case "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(serverclient.AgentContext{
				OwnerID: 42,
				Soul:    "ok",
				Skills:  []serverclient.ServerSkill{{Name: "srv-skill", Description: "Server skill"}},
			})
		case "/api/v1/agent/tools/skill_read":
			var req serverclient.ToolRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			skillSessionID = req.SessionID
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.ToolResult{Output: json.RawMessage(`{"content":"follow me"}`)})
		case "/api/v1/coding/sessions":
			mu.Lock()
			createdSessions++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/api/v1/llm/complete":
			var req serverclient.CompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			llmCalls++
			llmModels = append(llmModels, req.Model)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			b, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: req.Model, Message: serverclient.Message{Role: "assistant", Content: "Answer after reload."}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer oaSrv.Close()

	stateDir := t.TempDir()
	workspace := t.TempDir()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	cfg := config.Config{ServerURL: oaSrv.URL, Token: "tok", SyncInspector: true, StateDir: stateDir, MaxSteps: 5, RequestTimeout: 10 * time.Second}
	srv := NewServer(cfg, c2sR, s2cW, nil)
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(context.Background()) }()

	var (
		writeMu     sync.Mutex
		responsesMu sync.Mutex
		responses   = map[string]chan rpcEnvelope{}
	)
	sendRaw := func(lines ...any) {
		var buf strings.Builder
		for _, v := range lines {
			b, _ := json.Marshal(v)
			buf.Write(b)
			buf.WriteByte('\n')
		}
		writeMu.Lock()
		_, _ = c2sW.Write([]byte(buf.String()))
		writeMu.Unlock()
	}
	go func() {
		sc := bufio.NewScanner(s2cR)
		sc.Buffer(make([]byte, 4096), 8<<20)
		for sc.Scan() {
			var env rpcEnvelope
			if json.Unmarshal(sc.Bytes(), &env) == nil && len(env.ID) > 0 && env.Method == "" {
				var key string
				if json.Unmarshal(env.ID, &key) != nil {
					key = string(env.ID)
				}
				responsesMu.Lock()
				ch := responses[key]
				delete(responses, key)
				responsesMu.Unlock()
				if ch != nil {
					ch <- env
				}
			}
		}
	}()
	expect := func(id string) chan rpcEnvelope {
		ch := make(chan rpcEnvelope, 1)
		responsesMu.Lock()
		responses[id] = ch
		responsesMu.Unlock()
		return ch
	}

	newCh := expect("new")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "new", "method": "session/new", "params": map[string]any{"cwd": workspace}})
	newResp := <-newCh
	var newOut struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newResp.Result, &newOut)

	// P1-B: send session/prompt followed IMMEDIATELY by session/cancel in the same write burst on stdio.
	cancelCh := expect("p-cancel")
	sendRaw(
		map[string]any{"jsonrpc": "2.0", "id": "p-cancel", "method": "session/prompt", "params": map[string]any{"sessionId": newOut.SessionID, "prompt": []map[string]any{{"type": "text", "text": "must cancel"}}}},
		map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": newOut.SessionID}},
	)
	if r := <-cancelCh; !strings.Contains(string(r.Result), `"cancelled"`) {
		t.Fatalf("immediate session/cancel on stdio was lost: %s %+v", r.Result, r.Error)
	}
	mu.Lock()
	if llmCalls != 0 {
		mu.Unlock()
		t.Fatalf("immediately cancelled prompt still invoked LLM %d times", llmCalls)
	}
	mu.Unlock()

	// P1 (Round 5): malformed session/prompt (valid sessionId, invalid prompt type) returns an error and must NOT stall subsequent ops.
	badPromptCh := expect("bad-prompt")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "bad-prompt", "method": "session/prompt", "params": map[string]any{"sessionId": newOut.SessionID, "prompt": "not-an-array"}})
	if r := <-badPromptCh; r.Error == nil {
		t.Fatalf("expected error on malformed prompt, got %s", r.Result)
	}

	// P2-3: session/new already persisted the initial record (no manual Save needed).
	// Pause session/load during model discovery, send a MALFORMED reload (must NOT clear activeReloads
	// or break ordering), then send session/set_mode AND session/prompt; both must serialize behind
	// the valid reload and apply to the replacement session.
	loadCh := expect("reload")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "reload", "method": "session/load", "params": map[string]any{"sessionId": newOut.SessionID, "cwd": workspace}})
	<-reloadPaused

	badReloadCh := expect("bad-reload-during-load")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "bad-reload-during-load", "method": "session/load", "params": map[string]any{"sessionId": newOut.SessionID, "cwd": workspace, "mcpServers": "not-an-array"}})
	if r := <-badReloadCh; r.Error == nil {
		t.Fatalf("expected malformed reload during active reload to fail")
	}

	modeCh := expect("mode-during-reload")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "mode-during-reload", "method": "session/set_mode", "params": map[string]any{"sessionId": newOut.SessionID, "modeId": "plan"}})
	promptCh := expect("after-reload")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "after-reload", "method": "session/prompt", "params": map[string]any{"sessionId": newOut.SessionID, "prompt": []map[string]any{{"type": "text", "text": "survive reload"}}}})
	time.Sleep(40 * time.Millisecond)
	close(releaseReload)

	if r := <-loadCh; r.Error != nil {
		t.Fatalf("session/load failed: %+v", r.Error)
	}
	if r := <-modeCh; r.Error != nil {
		t.Fatalf("session/set_mode during reload failed: %+v", r.Error)
	}
	if r := <-promptCh; r.Error != nil || !strings.Contains(string(r.Result), `"end_turn"`) {
		t.Fatalf("prompt serialized behind reload failed: %s %+v", r.Result, r.Error)
	}
	sessAfter, err := srv.getSession(newOut.SessionID)
	if err != nil || len(sessAfter.Messages) == 0 {
		t.Fatalf("acknowledged prompt during reload was discarded: err=%v msgs=%d", err, len(sessAfter.Messages))
	}
	if _, gotMode, _ := sessAfter.SettingsSnapshot(); gotMode != "plan" {
		t.Fatalf("acknowledged session/set_mode during reload was lost after malformed reload: got mode=%q, want plan", gotMode)
	}

	// P1 (Round 7 + Round 8): missing, null, or malformed prompt blocks (including [null]) must fail
	// validation without cancelling an active prompt.
	activeCancelled := false
	fakeActive := &promptHandle{
		sessionID: newOut.SessionID,
		kind:      opPrompt,
		cancel:    func() { activeCancelled = true },
		done:      make(chan struct{}),
	}
	srv.mu.Lock()
	srv.sessionOps[newOut.SessionID] = fakeActive
	srv.activePrompts[newOut.SessionID] = fakeActive
	srv.mu.Unlock()
	for _, badBody := range []string{
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":null}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[null]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"invalid"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"audio","data":"AAAA","mimeType":"audio/wav"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"text"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"image","mimeType":"image/png"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"image","data":"iVBORw0KGgo="}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"image","mimeType":"image/png","data":"!!!not-base64!!!"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"resource_link","name":"doc"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"resource_link","uri":"file:///doc"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"resource"}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"resource","resource":{}}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"resource","resource":{"uri":"file:///bad.png","mimeType":"image/png","blob":"not valid base64"}}]}`,
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"resource","resource":{"uri":"file:///doc.pdf","mimeType":"application/pdf","blob":"JVBERi0="}}]}`,
		// Finding 4 (Round 11): oversized valid base64 PNG (>4 MiB) must fail before registering/cancelling active turn.
		`{"sessionId":` + fmt.Sprintf("%q", newOut.SessionID) + `,"prompt":[{"type":"image","mimeType":"image/png","data":"` + strings.Repeat("AAAA", (4<<20)/4+1024) + `"}]}`,
	} {
		_ = srv.prepareFrame(context.Background(), "session/prompt", json.RawMessage(badBody))
		if _, err := srv.handlePrompt(context.Background(), json.RawMessage(badBody)); err == nil {
			t.Fatalf("expected error for malformed prompt: %s", badBody)
		}
	}
	if activeCancelled {
		t.Fatal("missing, null, or malformed prompt cancelled an active turn")
	}
	srv.mu.Lock()
	delete(srv.sessionOps, newOut.SessionID)
	delete(srv.activePrompts, newOut.SessionID)
	srv.mu.Unlock()
	close(fakeActive.done)

	// P2-1 (Round 7): missing value in session/set_config_option must fail and preserve plan mode.
	badCfgCh := expect("bad-cfg")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "bad-cfg", "method": "session/set_config_option", "params": map[string]any{"sessionId": newOut.SessionID, "configId": "mode"}})
	if r := <-badCfgCh; r.Error == nil {
		t.Fatalf("expected error on set_config_option with missing value, got %s", r.Result)
	}
	if _, gotMode, _ := sessAfter.SettingsSnapshot(); gotMode != "plan" {
		t.Fatalf("malformed set_config_option cleared plan mode: got %q", gotMode)
	}

	// P2-1 (Round 6) + P2-3 (Round 7): prompt registered after set_model on an idle session must wait for
	// set_model and actually send the updated model to /api/v1/llm/complete.
	idleCtx := srv.prepareFrame(context.Background(), "session/set_model", json.RawMessage(`{"sessionId":`+fmt.Sprintf("%q", newOut.SessionID)+`,"modelId":"proxy/gpt-5.4-alt"}`))
	promptCtx := srv.prepareFrame(context.Background(), "session/prompt", json.RawMessage(`{"sessionId":`+fmt.Sprintf("%q", newOut.SessionID)+`,"prompt":[{"type":"text","text":"uses updated model"}]}`))
	promptFinished := make(chan error, 1)
	go func() {
		_, err := srv.handlePrompt(promptCtx, json.RawMessage(`{"sessionId":`+fmt.Sprintf("%q", newOut.SessionID)+`,"prompt":[{"type":"text","text":"uses updated model"}]}`))
		promptFinished <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if _, err := srv.handleSetModel(idleCtx, json.RawMessage(`{"sessionId":`+fmt.Sprintf("%q", newOut.SessionID)+`,"modelId":"proxy/gpt-5.4-alt"}`)); err != nil {
		t.Fatalf("idle handleSetModel failed: %v", err)
	}
	if err := <-promptFinished; err != nil {
		t.Fatalf("prompt after idle set_model failed: %v", err)
	}
	mu.Lock()
	lastLLMModel := ""
	if len(llmModels) > 0 {
		lastLLMModel = llmModels[len(llmModels)-1]
	}
	createdBeforeBlocked := createdSessions
	mu.Unlock()
	if lastLLMModel != "proxy/gpt-5.4-alt" {
		t.Fatalf("prompt overtook idle set_model: gateway received %q, want proxy/gpt-5.4-alt", lastLLMModel)
	}

	// P2-2 (Round 6) + P2-2 (Round 7): session/new with SyncInspector=true propagates initial save failure
	// BEFORE creating any remote Inspector session.
	blockedState := t.TempDir()
	if err := os.WriteFile(filepath.Join(blockedState, "sessions"), []byte("not-a-dir"), 0644); err != nil {
		t.Fatal(err)
	}
	badSrv := NewServer(config.Config{ServerURL: oaSrv.URL, Token: "tok", SyncInspector: true, StateDir: blockedState, RequestTimeout: 5 * time.Second}, strings.NewReader(""), io.Discard, nil)
	if _, _, err := badSrv.handleSessionNew(context.Background(), json.RawMessage(`{"cwd":`+fmt.Sprintf("%q", workspace)+`}`)); err == nil {
		t.Fatal("expected session/new to fail when initial session persistence fails")
	}
	mu.Lock()
	createdAfterBlocked := createdSessions
	mu.Unlock()
	if createdAfterBlocked != createdBeforeBlocked {
		t.Fatalf("failed session/new created remote Inspector session before local save: before=%d after=%d", createdBeforeBlocked, createdAfterBlocked)
	}

	// P2-1 (Round 5): failed reload must not clear live session's MCP tools.
	failLoadCh := expect("fail-reload")
	sendRaw(map[string]any{"jsonrpc": "2.0", "id": "fail-reload", "method": "session/load", "params": map[string]any{"sessionId": newOut.SessionID, "cwd": workspace, "mcpServers": "bad-type"}})
	if r := <-failLoadCh; r.Error == nil {
		t.Fatalf("expected malformed reload to fail")
	}
	if live, _ := srv.getSession(newOut.SessionID); live == nil || live.MCP == nil {
		t.Fatalf("failed reload cleared live session MCP manager")
	}

	// P2-E: after rollover advances ServerSessionID to -g2, remote skill_read sends the updated generation ID.
	sessAfter.ServerSessionID = newOut.SessionID + "-g2"
	if _, err := sessAfter.Skills.Invoke(context.Background(), "srv-skill", ""); err != nil {
		t.Fatalf("remote skill_invoke failed: %v", err)
	}
	mu.Lock()
	gotSkillSid := skillSessionID
	mu.Unlock()
	if gotSkillSid != newOut.SessionID+"-g2" {
		t.Fatalf("remote skill_read used stale session ID %q, want %q", gotSkillSid, newOut.SessionID+"-g2")
	}

	_ = c2sW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exit error: %v", err)
	}
}

func TestServerReplayPersistsUserPromptAfterGatewayError(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	var mu sync.Mutex
	llmCalls := 0
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_model": "proxy/gpt-5.4",
				"models":        []map[string]any{{"id": "proxy/gpt-5.4", "provider": "proxy", "model": "gpt-5.4"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "skills": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			mu.Lock()
			llmCalls++
			callNum := llmCalls
			mu.Unlock()
			if callNum == 2 {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			td, _ := json.Marshal(serverclient.StreamEvent{Type: "text_delta", Delta: "first reply"})
			doneEv, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "first reply"}},
			})
			fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", td, doneEv)
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer oaSrv.Close()

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	defer s2cR.Close()

	srv := NewServer(config.Config{
		ServerURL:      oaSrv.URL,
		Token:          "tok",
		SyncInspector:  false,
		StateDir:       stateDir,
		RequestTimeout: 5 * time.Second,
	}, c2sR, s2cW, nil)

	srvDone := make(chan error, 1)
	go func() {
		defer s2cW.Close()
		srvDone <- srv.Run(context.Background())
	}()

	type rpcResp struct {
		Result json.RawMessage
		Error  *rpcError
	}
	var respMu sync.Mutex
	waiters := map[string]chan rpcResp{}
	var notifications []map[string]any

	go func() {
		dec := json.NewDecoder(s2cR)
		for {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg.Method == "session/update" {
				var p map[string]any
				_ = json.Unmarshal(msg.Params, &p)
				respMu.Lock()
				notifications = append(notifications, p)
				respMu.Unlock()
				continue
			}
			if len(msg.ID) > 0 {
				var idStr string
				_ = json.Unmarshal(msg.ID, &idStr)
				respMu.Lock()
				ch := waiters[idStr]
				delete(waiters, idStr)
				respMu.Unlock()
				if ch != nil {
					ch <- rpcResp{Result: msg.Result, Error: msg.Error}
				}
			}
		}
	}()

	call := func(id, method string, params any) rpcResp {
		ch := make(chan rpcResp, 1)
		respMu.Lock()
		waiters[id] = ch
		respMu.Unlock()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		_, _ = c2sW.Write(append(b, '\n'))
		return <-ch
	}

	if r := call("init", "initialize", map[string]any{"protocolVersion": 1}); r.Error != nil {
		t.Fatalf("initialize failed: %+v", r.Error)
	}
	newR := call("new", "session/new", map[string]any{"cwd": workspace})
	if newR.Error != nil {
		t.Fatalf("session/new failed: %+v", newR.Error)
	}
	var newOut struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newR.Result, &newOut)

	// Turn 1 succeeds.
	if r := call("p1", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "first prompt"}},
	}); r.Error != nil {
		t.Fatalf("turn 1 failed: %+v", r.Error)
	}

	// Turn 2 hits HTTP 503 after accepting the user prompt.
	if r := call("p2", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "second prompt after 503"}},
	}); r.Error == nil {
		t.Fatalf("expected turn 2 to fail with HTTP 503, got %s", r.Result)
	}

	// Clear captured notifications before session/load and verify BOTH user prompts replay.
	respMu.Lock()
	notifications = nil
	respMu.Unlock()

	if r := call("load", "session/load", map[string]any{"sessionId": newOut.SessionID, "cwd": workspace}); r.Error != nil {
		t.Fatalf("session/load failed: %+v", r.Error)
	}

	respMu.Lock()
	replayed := append([]map[string]any(nil), notifications...)
	respMu.Unlock()

	sawFirst, sawSecond := false, false
	for _, n := range replayed {
		upd, _ := n["update"].(map[string]any)
		if upd["sessionUpdate"] != "user_message_chunk" {
			continue
		}
		content, _ := upd["content"].(map[string]any)
		switch content["text"] {
		case "first prompt":
			sawFirst = true
		case "second prompt after 503":
			sawSecond = true
		}
	}
	if !sawFirst || !sawSecond {
		t.Fatalf("expected both user prompts in replay after HTTP 503, sawFirst=%v sawSecond=%v replayed=%+v", sawFirst, sawSecond, replayed)
	}

	_ = c2sW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exit error: %v", err)
	}
}

func TestServerRound9ProtocolEmbeddedBlobAndInterruptedToolCall(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	var mu sync.Mutex
	var capturedReqs []serverclient.CompleteRequest
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_model": "proxy/gpt-5.4",
				"models":        []map[string]any{{"id": "proxy/gpt-5.4", "provider": "proxy", "model": "gpt-5.4"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "skills": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			var req serverclient.CompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			capturedReqs = append(capturedReqs, req)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			doneEv, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "ok"}},
			})
			fmt.Fprintf(w, "data: %s\n\n", doneEv)
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer oaSrv.Close()

	// Seed a saved session record with an unanswered assistant tool call (simulating a crash mid-tool).
	st := session.NewStore(stateDir)
	if err := st.Save(session.Record{
		ID:    "crashed-sess",
		CWD:   workspace,
		Model: "proxy/gpt-5.4",
		Mode:  "default",
		Messages: []serverclient.Message{
			{TurnID: "crashed-sess-t1", Role: "user", Content: "run something"},
			{
				TurnID: "crashed-sess-t1",
				Role:   "assistant",
				ToolCalls: []serverclient.ToolCall{{
					ID:        "call-interrupted-1",
					Name:      "codemode",
					Arguments: json.RawMessage(`{"code":"return 1"}`),
				}},
			},
		},
		TurnCounter: 1,
	}); err != nil {
		t.Fatal(err)
	}

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	defer s2cR.Close()

	srv := NewServer(config.Config{
		ServerURL:      oaSrv.URL,
		Token:          "tok",
		SyncInspector:  false,
		StateDir:       stateDir,
		RequestTimeout: 5 * time.Second,
	}, c2sR, s2cW, nil)

	srvDone := make(chan error, 1)
	go func() {
		defer s2cW.Close()
		srvDone <- srv.Run(context.Background())
	}()

	type rpcResp struct {
		Result json.RawMessage
		Error  *rpcError
	}
	var respMu sync.Mutex
	waiters := map[string]chan rpcResp{}
	var notifications []map[string]any

	go func() {
		dec := json.NewDecoder(s2cR)
		for {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg.Method == "session/update" {
				var p map[string]any
				_ = json.Unmarshal(msg.Params, &p)
				respMu.Lock()
				notifications = append(notifications, p)
				respMu.Unlock()
				continue
			}
			if len(msg.ID) > 0 {
				var idStr string
				_ = json.Unmarshal(msg.ID, &idStr)
				respMu.Lock()
				ch := waiters[idStr]
				delete(waiters, idStr)
				respMu.Unlock()
				if ch != nil {
					ch <- rpcResp{Result: msg.Result, Error: msg.Error}
				}
			}
		}
	}()

	call := func(id, method string, params any) rpcResp {
		ch := make(chan rpcResp, 1)
		respMu.Lock()
		waiters[id] = ch
		respMu.Unlock()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		_, _ = c2sW.Write(append(b, '\n'))
		return <-ch
	}

	// Finding 6: initialize with protocolVersion 2 must negotiate down to SupportedProtocolVersion (1).
	initR := call("init", "initialize", map[string]any{"protocolVersion": 2})
	if initR.Error != nil {
		t.Fatalf("initialize failed: %+v", initR.Error)
	}
	var initOut struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = json.Unmarshal(initR.Result, &initOut)
	if initOut.ProtocolVersion != SupportedProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", initOut.ProtocolVersion, SupportedProtocolVersion)
	}

	// Finding 1: loading a session with an unanswered tool call repairs it before the next gateway request.
	if r := call("load-crashed", "session/load", map[string]any{"sessionId": "crashed-sess", "cwd": workspace}); r.Error != nil {
		t.Fatalf("session/load crashed-sess failed: %+v", r.Error)
	}
	if r := call("prompt-after-crash", "session/prompt", map[string]any{
		"sessionId": "crashed-sess",
		"prompt":    []map[string]any{{"type": "text", "text": "continue safely"}},
	}); r.Error != nil {
		t.Fatalf("prompt after crash failed: %+v", r.Error)
	}

	mu.Lock()
	if len(capturedReqs) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 gateway request, got %d", len(capturedReqs))
	}
	msgs := capturedReqs[0].Messages
	mu.Unlock()

	foundToolReplyBeforeNewUser := false
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) == 1 && msgs[i].ToolCalls[0].ID == "call-interrupted-1" {
			if i+2 < len(msgs) && msgs[i+1].Role == "tool" && msgs[i+1].ToolCallID == "call-interrupted-1" && msgs[i+2].Role == "user" && msgs[i+2].Content == "continue safely" {
				foundToolReplyBeforeNewUser = true
			}
		}
	}
	if !foundToolReplyBeforeNewUser {
		t.Fatalf("expected synthetic interrupted tool result between unanswered tool call and next user message, got: %+v", msgs)
	}

	// Finding 3: valid embedded PNG resource reaches gateway AND replays on session/load; unsupported blob errors.
	newR := call("new", "session/new", map[string]any{"cwd": workspace})
	if newR.Error != nil {
		t.Fatalf("session/new failed: %+v", newR.Error)
	}
	var newOut struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newR.Result, &newOut)

	if r := call("bad-blob", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt": []map[string]any{{
			"type": "resource",
			"resource": map[string]any{
				"uri":      "file:///doc.pdf",
				"mimeType": "application/pdf",
				"blob":     "JVBERi0=",
			},
		}},
	}); r.Error == nil {
		t.Fatalf("expected unsupported PDF blob resource to return error, got %s", r.Result)
	}

	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII="
	if r := call("good-blob", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt": []map[string]any{{
			"type": "resource",
			"resource": map[string]any{
				"uri":      "file:///screen.png",
				"mimeType": "image/png",
				"blob":     pngB64,
			},
		}},
	}); r.Error != nil {
		t.Fatalf("valid PNG embedded resource prompt failed: %+v", r.Error)
	}

	mu.Lock()
	blobReq := capturedReqs[len(capturedReqs)-1]
	mu.Unlock()
	lastUser := blobReq.Messages[len(blobReq.Messages)-1]
	if len(lastUser.Images) != 1 || lastUser.Images[0] != "data:image/png;base64,"+pngB64 {
		t.Fatalf("gateway did not receive embedded PNG image: %+v", lastUser)
	}

	respMu.Lock()
	notifications = nil
	respMu.Unlock()
	if r := call("reload-blob", "session/load", map[string]any{"sessionId": newOut.SessionID, "cwd": workspace}); r.Error != nil {
		t.Fatalf("session/load after embedded blob failed: %+v", r.Error)
	}
	respMu.Lock()
	rawReplay, _ := json.Marshal(notifications)
	respMu.Unlock()
	if !strings.Contains(string(rawReplay), pngB64) || !strings.Contains(string(rawReplay), "file:///screen.png") {
		t.Fatalf("embedded PNG resource missing from replay: %s", string(rawReplay))
	}

	_ = c2sW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exit error: %v", err)
	}
}

func TestServerRound10SyncPersistenceOnCrashAndSaveFailurePropagation(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	inFlight := make(chan struct{})
	releaseLLM := make(chan struct{})
	var mu sync.Mutex
	llmCalls := 0
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_model": "proxy/gpt-5.4",
				"models":        []map[string]any{{"id": "proxy/gpt-5.4", "provider": "proxy", "model": "gpt-5.4"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "skills": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			mu.Lock()
			llmCalls++
			callNum := llmCalls
			mu.Unlock()
			if callNum == 1 {
				close(inFlight)
				select {
				case <-releaseLLM:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			doneEv, _ := json.Marshal(serverclient.StreamEvent{
				Type:       "done",
				Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "done"}},
			})
			fmt.Fprintf(w, "data: %s\n\n", doneEv)
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer oaSrv.Close()
	defer func() {
		select {
		case <-releaseLLM:
		default:
			close(releaseLLM)
		}
	}()

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	defer s2cR.Close()

	srv := NewServer(config.Config{
		ServerURL:      oaSrv.URL,
		Token:          "tok",
		SyncInspector:  true,
		StateDir:       stateDir,
		RequestTimeout: 5 * time.Second,
	}, c2sR, s2cW, nil)

	srvDone := make(chan error, 1)
	go func() {
		defer s2cW.Close()
		srvDone <- srv.Run(context.Background())
	}()

	type rpcResp struct {
		Result json.RawMessage
		Error  *rpcError
	}
	var respMu sync.Mutex
	waiters := map[string]chan rpcResp{}
	go func() {
		dec := json.NewDecoder(s2cR)
		for {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if len(msg.ID) > 0 {
				var idStr string
				_ = json.Unmarshal(msg.ID, &idStr)
				respMu.Lock()
				ch := waiters[idStr]
				delete(waiters, idStr)
				respMu.Unlock()
				if ch != nil {
					ch <- rpcResp{Result: msg.Result, Error: msg.Error}
				}
			}
		}
	}()
	startCall := func(id, method string, params any) chan rpcResp {
		ch := make(chan rpcResp, 1)
		respMu.Lock()
		waiters[id] = ch
		respMu.Unlock()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		_, _ = c2sW.Write(append(b, '\n'))
		return ch
	}

	if r := <-startCall("init", "initialize", map[string]any{"protocolVersion": 1}); r.Error != nil {
		t.Fatalf("initialize failed: %+v", r.Error)
	}
	newR := <-startCall("new", "session/new", map[string]any{"cwd": workspace})
	if newR.Error != nil {
		t.Fatalf("session/new failed: %+v", newR.Error)
	}
	var newOut struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newR.Result, &newOut)

	// Finding 1: while RunTurn is still in-flight at the gateway, the saved record on disk
	// must ALREADY contain the pending Inspector sync entry for the accepted user message.
	p1Ch := startCall("p1", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "persist sync before return"}},
	})
	<-inFlight
	st := session.NewStore(stateDir)
	rec, ok, err := st.Load(newOut.SessionID)
	if err != nil || !ok {
		t.Fatalf("load in-flight session: ok=%v err=%v", ok, err)
	}
	if len(rec.Messages) != 1 || len(rec.PendingSyncMessages) != 1 {
		t.Fatalf("expected 1 persisted message AND 1 pending sync message while in-flight, got msgs=%d pendingSync=%d", len(rec.Messages), len(rec.PendingSyncMessages))
	}
	close(releaseLLM)
	if r := <-p1Ch; r.Error != nil {
		t.Fatalf("turn 1 failed: %+v", r.Error)
	}

	// Switch to full-access while storage is writable.
	if r := <-startCall("mode-full", "session/set_mode", map[string]any{"sessionId": newOut.SessionID, "modeId": "full-access"}); r.Error != nil {
		t.Fatalf("set_mode full-access failed: %+v", r.Error)
	}

	// Finding 3 (Round 10) + Finding 6 (Round 11): block Store.Save deterministically (works even as UID 0 in Docker)
	// by creating a directory at the temp file path used by Store.Save.
	tmpBlocker := filepath.Join(stateDir, "sessions", newOut.SessionID+".json.tmp")
	if err := os.MkdirAll(tmpBlocker, 0755); err != nil {
		t.Fatalf("mkdir tmpBlocker: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpBlocker) }()

	if r := <-startCall("mode-plan-fail", "session/set_mode", map[string]any{"sessionId": newOut.SessionID, "modeId": "plan"}); r.Error == nil {
		t.Fatalf("expected session/set_mode to fail when session save fails, got %s", r.Result)
	}
	sess, err := srv.getSession(newOut.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, gotMode, _ := sess.SettingsSnapshot(); gotMode != "full-access" {
		t.Fatalf("failed set_mode mutated in-memory mode to %q, want full-access", gotMode)
	}

	mu.Lock()
	callsBeforeUnsavedPrompt := llmCalls
	mu.Unlock()
	if r := <-startCall("prompt-save-fail", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "must not be accepted without save"}},
	}); r.Error == nil {
		t.Fatalf("expected session/prompt to fail when initial save fails, got %s", r.Result)
	}
	mu.Lock()
	callsAfterUnsavedPrompt := llmCalls
	mu.Unlock()
	if callsAfterUnsavedPrompt != callsBeforeUnsavedPrompt {
		t.Fatalf("prompt called gateway despite failing to persist turn: before=%d after=%d", callsBeforeUnsavedPrompt, callsAfterUnsavedPrompt)
	}

	_ = os.RemoveAll(tmpBlocker)
	_ = c2sW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exit error: %v", err)
	}
}

func TestServerRound11AssistantSaveErrorAndMidScriptChildSyncPersistence(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()

	// Local MCP tool that blocks on the second child tool call so we can inspect
	// disk persistence of the first completed child tool (fs_write) mid-script.
	secondToolStarted := make(chan struct{})
	releaseSecondTool := make(chan struct{})
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-06-18"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{"name": "pause_step", "inputSchema": map[string]any{"type": "object"}}},
				},
			})
		case "tools/call":
			close(secondToolStarted)
			select {
			case <-releaseSecondTool:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"resumed":true}`}}},
			})
		}
	}))
	defer mcpSrv.Close()
	defer func() {
		select {
		case <-releaseSecondTool:
		default:
			close(releaseSecondTool)
		}
	}()

	var mu sync.Mutex
	llmStep := 0
	assistantReady := make(chan struct{})
	releaseAssistant := make(chan struct{})
	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"default_model": "proxy/gpt-5.4",
				"models":        []map[string]any{{"id": "proxy/gpt-5.4", "provider": "proxy", "model": "gpt-5.4"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"tools": []any{}, "skills": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			mu.Lock()
			llmStep++
			step := llmStep
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			switch step {
			case 1:
				script := `await tools.fs_write({path: "mid.txt", content: "written before crash"}); await tools.mcp_hook_pause_step({}); return "ok";`
				argsRaw, _ := json.Marshal(map[string]string{"code": script})
				doneEv, _ := json.Marshal(serverclient.StreamEvent{
					Type: "done",
					Completion: &serverclient.Completion{
						Model: "proxy/gpt-5.4",
						Message: serverclient.Message{
							Role: "assistant",
							ToolCalls: []serverclient.ToolCall{{
								ID:        "cm-1",
								Name:      "codemode",
								Arguments: argsRaw,
							}},
						},
					},
				})
				fmt.Fprintf(w, "data: %s\n\n", doneEv)
			case 2:
				doneEv, _ := json.Marshal(serverclient.StreamEvent{
					Type:       "done",
					Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "script done"}},
				})
				fmt.Fprintf(w, "data: %s\n\n", doneEv)
			default:
				// Turn 2: wait until the test blocks Store.Save AFTER the user prompt was saved.
				close(assistantReady)
				select {
				case <-releaseAssistant:
				case <-r.Context().Done():
					return
				}
				doneEv, _ := json.Marshal(serverclient.StreamEvent{
					Type:       "done",
					Completion: &serverclient.Completion{Model: "proxy/gpt-5.4", Message: serverclient.Message{Role: "assistant", Content: "reply that cannot be saved"}},
				})
				fmt.Fprintf(w, "data: %s\n\n", doneEv)
			}
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer oaSrv.Close()
	defer func() {
		select {
		case <-releaseAssistant:
		default:
			close(releaseAssistant)
		}
	}()

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	defer s2cR.Close()

	srv := NewServer(config.Config{
		ServerURL:      oaSrv.URL,
		Token:          "tok",
		SyncInspector:  true,
		StateDir:       stateDir,
		RequestTimeout: 5 * time.Second,
	}, c2sR, s2cW, nil)

	srvDone := make(chan error, 1)
	go func() {
		defer s2cW.Close()
		srvDone <- srv.Run(context.Background())
	}()

	type rpcResp struct {
		Result json.RawMessage
		Error  *rpcError
	}
	var respMu sync.Mutex
	waiters := map[string]chan rpcResp{}
	go func() {
		dec := json.NewDecoder(s2cR)
		for {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if len(msg.ID) > 0 {
				var idStr string
				_ = json.Unmarshal(msg.ID, &idStr)
				respMu.Lock()
				ch := waiters[idStr]
				delete(waiters, idStr)
				respMu.Unlock()
				if ch != nil {
					ch <- rpcResp{Result: msg.Result, Error: msg.Error}
				}
			}
		}
	}()
	startCall := func(id, method string, params any) chan rpcResp {
		ch := make(chan rpcResp, 1)
		respMu.Lock()
		waiters[id] = ch
		respMu.Unlock()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		_, _ = c2sW.Write(append(b, '\n'))
		return ch
	}

	if r := <-startCall("init", "initialize", map[string]any{"protocolVersion": 1}); r.Error != nil {
		t.Fatalf("initialize failed: %+v", r.Error)
	}
	newR := <-startCall("new", "session/new", map[string]any{
		"cwd":        workspace,
		"mcpServers": []map[string]any{{"name": "hook", "type": "http", "url": mcpSrv.URL}},
	})
	if newR.Error != nil {
		t.Fatalf("session/new failed: %+v", newR.Error)
	}
	var newOut struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newR.Result, &newOut)
	if r := <-startCall("mode", "session/set_mode", map[string]any{"sessionId": newOut.SessionID, "modeId": "full-access"}); r.Error != nil {
		t.Fatalf("set_mode failed: %+v", r.Error)
	}

	// Finding 2 (Round 11): run a script with fs_write followed by a paused MCP tool.
	// While paused in tool 2, the completed fs_write child messages & trace must ALREADY be on disk.
	p1Ch := startCall("p1", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "run script"}},
	})
	<-secondToolStarted

	st := session.NewStore(stateDir)
	rec, ok, err := st.Load(newOut.SessionID)
	if err != nil || !ok {
		t.Fatalf("load mid-script session: ok=%v err=%v", ok, err)
	}
	sawFSWriteTrace := false
	for _, tr := range rec.PendingSyncTraces {
		if tr.Name == "fs_write" {
			sawFSWriteTrace = true
		}
	}
	if !sawFSWriteTrace {
		t.Fatalf("mid-script fs_write trace not persisted before second child tool ran: traces=%+v", rec.PendingSyncTraces)
	}
	if len(rec.PendingSyncMessages) < 4 {
		t.Fatalf("expected user + assistant + fs_write child assistant/tool messages persisted mid-script, got %d", len(rec.PendingSyncMessages))
	}
	close(releaseSecondTool)
	if r := <-p1Ch; r.Error != nil {
		t.Fatalf("turn 1 failed: %+v", r.Error)
	}

	// Finding 1 (Round 11): fail Store.Save AFTER the user prompt is saved, right before assistant reply is persisted.
	// session/prompt MUST return an error instead of end_turn.
	p2Ch := startCall("p2", "session/prompt", map[string]any{
		"sessionId": newOut.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "trigger assistant save error"}},
	})
	<-assistantReady
	tmpBlocker := filepath.Join(stateDir, "sessions", newOut.SessionID+".json.tmp")
	if err := os.MkdirAll(tmpBlocker, 0755); err != nil {
		t.Fatalf("mkdir tmpBlocker: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpBlocker) }()
	close(releaseAssistant)

	if r := <-p2Ch; r.Error == nil {
		t.Fatalf("expected session/prompt to fail when assistant response cannot be saved, got %s", r.Result)
	}

	_ = os.RemoveAll(tmpBlocker)
	_ = c2sW.Close()
	if err := <-srvDone; err != nil {
		t.Fatalf("server exit error: %v", err)
	}
}
