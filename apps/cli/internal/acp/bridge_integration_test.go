package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

func TestZeroLLMBridgeTickAndSingletonInteractiveCoherence(t *testing.T) {
	var snoozeCalls atomic.Int32
	// Fake T3 Code MCP server providing the minimal T3 tools.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"tools": []map[string]any{
					{"name": "t3_environment_read", "description": "read env", "inputSchema": map[string]any{"type": "object"}},
					{"name": "t3_project_list", "description": "list projects", "inputSchema": map[string]any{"type": "object"}},
					{"name": "orchestrator_capabilities", "description": "caps", "inputSchema": map[string]any{"type": "object"}},
					{"name": "t3_thread_launch", "description": "launch", "inputSchema": map[string]any{"type": "object"}},
					{"name": "t3_thread_read", "description": "read", "inputSchema": map[string]any{"type": "object"}},
					{"name": "t3_pending_request", "description": "pending", "inputSchema": map[string]any{"type": "object"}},
					{"name": "t3_thread_organize", "description": "organize", "inputSchema": map[string]any{"type": "object"}},
				},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			var out any
			switch p.Name {
			case "t3_environment_read":
				out = map[string]any{"environmentId": "env-acp-1", "label": "MacBook"}
			case "t3_project_list":
				out = map[string]any{"projects": []map[string]any{{"id": "p1", "title": "Repo", "workspaceRoot": "/repo"}}}
			case "orchestrator_capabilities":
				out = map[string]any{"providers": []map[string]any{{"providerInstanceId": "antigravity", "driverKind": "antigravity", "displayName": "Antigravity", "models": []map[string]any{{"id": "gemini-4-neon"}}}}}
			case "t3_thread_read":
				out = map[string]any{"thread": map[string]any{"threadId": "th-1", "status": "completed"}}
			case "t3_thread_organize":
				if p.Arguments["action"] == "snooze" && p.Arguments["snoozedUntil"] != "" {
					snoozeCalls.Add(1)
				}
				out = map[string]any{"ok": true}
			default:
				out = map[string]any{"ok": true}
			}
			raw, _ := json.Marshal(out)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}},
			})
		}
	}))
	defer mcpSrv.Close()

	var (
		llmCalls     atomic.Int32
		regCalls     atomic.Int32
		hbCalls      atomic.Int32
		onceDrains   atomic.Int32
		cmdResMu     sync.Mutex
		cmdResults   = map[string]serverclient.T3CommandResultRequest{}
		onceReturned atomic.Bool
	)

	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bridge-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/llm/models":
			_ = json.NewEncoder(w).Encode(serverclient.ModelsResponse{
				DefaultModel: "proxy/gpt-5.4",
				Models:       []serverclient.CatalogModel{{ID: "proxy/gpt-5.4", Provider: "proxy", Model: "gpt-5.4", DisplayName: "GPT 5.4", ContextWindowTokens: 128000}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/context":
			_ = json.NewEncoder(w).Encode(serverclient.AgentContext{OwnerID: 1, Soul: "Helpful"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/llm/complete":
			llmCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			send := func(ev serverclient.StreamEvent) {
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
			send(serverclient.StreamEvent{Type: "text_delta", Delta: "Interactive reply!"})
			send(serverclient.StreamEvent{
				Type: "done",
				Completion: &serverclient.Completion{
					Model:   "proxy/gpt-5.4",
					Message: serverclient.Message{Role: "assistant", Content: "Interactive reply!"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/hosts/register":
			regCalls.Add(1)
			_ = json.NewEncoder(w).Encode(serverclient.T3HostRegisterResponse{HostID: "h1", EnvironmentID: "env-acp-1", RegisteredAt: "2026-11-25T20:00:00Z"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/heartbeat":
			hbCalls.Add(1)
			_ = json.NewEncoder(w).Encode(serverclient.T3HeartbeatResponse{OK: true, ServerTime: "2026-11-25T20:00:01Z"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/t3/commands/stream" && r.URL.Query().Get("once") == "true":
			onceDrains.Add(1)
			var cmds []serverclient.T3Command
			if !onceReturned.Swap(true) {
				cmds = []serverclient.T3Command{{
					CommandID: "cmd-read-1",
					Action:    "thread_read",
					Payload:   json.RawMessage(`{"thread_id":"th-1","view":"messages","limit":5}`),
				}}
			}
			_ = json.NewEncoder(w).Encode(serverclient.T3CommandsDrainResponse{Commands: cmds})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/t3/commands/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/t3/commands/") && strings.HasSuffix(r.URL.Path, "/result"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/t3/commands/"), "/result")
			var req serverclient.T3CommandResultRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			cmdResMu.Lock()
			cmdResults[id] = req
			cmdResMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/events":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer oaSrv.Close()

	sharedStateDir := t.TempDir()
	startClient := func(name string) (call func(id, method string, params any) rpcEnvelope, getUpdates func() []string, closeFn func(), srv *Server) {
		c2sR, c2sW := io.Pipe()
		s2cR, s2cW := io.Pipe()
		cfg := config.Config{
			ServerURL:        oaSrv.URL,
			Token:            "bridge-tok",
			SyncInspector:    false,
			StateDir:         sharedStateDir,
			CustomStateDir:   true,
			MaxSteps:         5,
			RequestTimeout:   5 * time.Second,
			Bridge:           true,
			HostLabel:        "TestHost",
			BridgeTickPrompt: config.DefaultBridgeTickPrompt,
		}
		srv = NewServer(cfg, c2sR, s2cW, nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- srv.Run(ctx) }()

		var (
			wMu   sync.Mutex
			rMu   sync.Mutex
			uMu   sync.Mutex
			resps = map[string]chan rpcEnvelope{}
			texts []string
		)
		go func() {
			sc := bufio.NewScanner(s2cR)
			sc.Buffer(make([]byte, 4096), 4<<20)
			for sc.Scan() {
				var env rpcEnvelope
				if json.Unmarshal(sc.Bytes(), &env) != nil {
					continue
				}
				if env.Method == "session/update" {
					var p struct {
						Update struct {
							SessionUpdate string `json:"sessionUpdate"`
							Content       struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"update"`
					}
					if json.Unmarshal(env.Params, &p) == nil && p.Update.SessionUpdate == "agent_message_chunk" {
						uMu.Lock()
						texts = append(texts, p.Update.Content.Text)
						uMu.Unlock()
					}
					continue
				}
				if len(env.ID) > 0 {
					var key string
					if json.Unmarshal(env.ID, &key) != nil {
						key = string(env.ID)
					}
					rMu.Lock()
					ch := resps[key]
					delete(resps, key)
					rMu.Unlock()
					if ch != nil {
						ch <- env
					}
				}
			}
		}()
		call = func(id, method string, params any) rpcEnvelope {
			t.Helper()
			ch := make(chan rpcEnvelope, 1)
			rMu.Lock()
			resps[id] = ch
			rMu.Unlock()
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
			wMu.Lock()
			_, _ = c2sW.Write(append(b, '\n'))
			wMu.Unlock()
			select {
			case resp := <-ch:
				if resp.Error != nil {
					t.Fatalf("[%s] %s error: %+v", name, method, resp.Error)
				}
				return resp
			case <-time.After(10 * time.Second):
				t.Fatalf("[%s] timeout waiting for %s", name, method)
				return rpcEnvelope{}
			}
		}
		getUpdates = func() []string {
			uMu.Lock()
			defer uMu.Unlock()
			return append([]string(nil), texts...)
		}
		closeFn = func() {
			_ = c2sW.Close()
			cancel()
			<-done
		}
		return call, getUpdates, closeFn, srv
	}

	// Instance 1 acquires the singleton bridge lock.
	call1, updates1, close1, srv1 := startClient("bridge-primary")
	defer close1()

	call1("init-1", "initialize", map[string]any{"protocolVersion": 1})
	if srv1.Bridge() == nil {
		t.Fatal("primary server must acquire the singleton bridge lock")
	}

	newResp1 := call1("new-1", "session/new", map[string]any{
		"cwd": t.TempDir(),
		"mcpServers": []map[string]any{
			{"name": "t3-code", "type": "http", "url": mcpSrv.URL},
		},
	})
	var s1 struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newResp1.Result, &s1)

	// Send exact __openalbert_bridge_tick__ prompt -> MUST intercept with ZERO LLM calls!
	tickResp := call1("tick-1", "session/prompt", map[string]any{
		"sessionId": s1.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": config.DefaultBridgeTickPrompt}},
	})
	if !strings.Contains(string(tickResp.Result), `"end_turn"`) {
		t.Fatalf("unexpected tick response: %s", tickResp.Result)
	}
	if llmCalls.Load() != 0 {
		t.Fatalf("bridge tick MUST make 0 calls to /api/v1/llm/complete, got %d", llmCalls.Load())
	}
	if regCalls.Load() < 1 || hbCalls.Load() < 1 || onceDrains.Load() < 1 {
		t.Fatalf("expected register/heartbeat/drain during tick, got reg=%d hb=%d drain=%d", regCalls.Load(), hbCalls.Load(), onceDrains.Load())
	}
	cmdResMu.Lock()
	res1, ok := cmdResults["cmd-read-1"]
	cmdResMu.Unlock()
	if !ok || !res1.OK || !strings.Contains(string(res1.Result), `"th-1"`) {
		t.Fatalf("expected drained thread_read command result, got ok=%v res=%+v", ok, res1)
	}
	if joined := strings.Join(updates1(), ""); !strings.Contains(joined, "bridge ok") {
		t.Fatalf("expected minimal 'bridge ok' ACP chunk, got %q", joined)
	}
	if snoozeCalls.Load() < 1 {
		t.Fatalf("expected bridge tick to auto-snooze thread via t3_thread_organize, got %d snooze calls", snoozeCalls.Load())
	}

	// Instance 2 starts against the SAME StateDir -> lock already held -> stays interactive-only!
	call2, updates2, close2, srv2 := startClient("interactive-secondary")
	defer close2()

	call2("init-2", "initialize", map[string]any{"protocolVersion": 1})
	if srv2.Bridge() != nil {
		t.Fatal("secondary server must NOT acquire bridge lock while primary holds it")
	}
	newResp2 := call2("new-2", "session/new", map[string]any{"cwd": t.TempDir()})
	var s2 struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(newResp2.Result, &s2)

	// Tick on standby instance still incurs 0 LLM calls.
	call2("tick-2", "session/prompt", map[string]any{
		"sessionId": s2.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": config.DefaultBridgeTickPrompt}},
	})
	if llmCalls.Load() != 0 {
		t.Fatalf("standby tick must not call LLM, got %d", llmCalls.Load())
	}

	// T3 Code ACP wrapped envelope (<t3_code_instructions> + <user_request> + [T3 Code: ...] + <runtime_info>)
	// MUST ALSO intercept with 0 LLM calls!
	wrappedTick := "<t3_code_instructions>\n## T3 Code interaction mode: Default\n</t3_code_instructions>\n" +
		"<user_request>\n[T3 Code: the agent in T3 thread \"parent-1\" sent this message, not the user.]\n\n" +
		config.DefaultBridgeTickPrompt + "\n</user_request>\n" +
		"<runtime_info>running in T3 Code</runtime_info>"
	wrappedResp := call1("tick-wrapped", "session/prompt", map[string]any{
		"sessionId": s1.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": wrappedTick}},
	})
	if !strings.Contains(string(wrappedResp.Result), `"end_turn"`) {
		t.Fatalf("unexpected wrapped tick response: %s", wrappedResp.Result)
	}
	if llmCalls.Load() != 0 {
		t.Fatalf("T3-wrapped bridge tick MUST make 0 LLM calls, got %d", llmCalls.Load())
	}

	// Normal prompt inside <user_request> that merely mentions the tick marker must NOT be intercepted.
	wrappedNonTick := "<t3_code_instructions>mode</t3_code_instructions>\n" +
		"<user_request>\nPlease explain what " + config.DefaultBridgeTickPrompt + " does.\n</user_request>"
	wrappedNonTickResp := call2("prompt-wrapped-non-tick", "session/prompt", map[string]any{
		"sessionId": s2.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": wrappedNonTick}},
	})
	if !strings.Contains(string(wrappedNonTickResp.Result), `"end_turn"`) {
		t.Fatalf("unexpected wrapped non-tick response: %s", wrappedNonTickResp.Result)
	}
	if llmCalls.Load() != 1 {
		t.Fatalf("wrapped non-tick prompt must reach ordinary LLM path, got %d LLM calls", llmCalls.Load())
	}

	// Padded tick prompt must NOT be intercepted; it must go through the ordinary interactive ACP LLM path.
	paddedResp := call2("prompt-padded", "session/prompt", map[string]any{
		"sessionId": s2.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": " " + config.DefaultBridgeTickPrompt + "\n"}},
	})
	if !strings.Contains(string(paddedResp.Result), `"end_turn"`) {
		t.Fatalf("unexpected padded prompt response: %s", paddedResp.Result)
	}
	if llmCalls.Load() != 2 {
		t.Fatalf("padded tick marker must reach ordinary LLM path, got %d LLM calls", llmCalls.Load())
	}

	// Ordinary interactive prompt on secondary instance works normally!
	interResp := call2("prompt-2", "session/prompt", map[string]any{
		"sessionId": s2.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "Hello interactive ACP"}},
	})
	if !strings.Contains(string(interResp.Result), `"end_turn"`) {
		t.Fatalf("unexpected interactive prompt response: %s", interResp.Result)
	}
	if llmCalls.Load() != 3 {
		t.Fatalf("expected 3 total LLM calls (wrapped non-tick + padded + interactive), got %d", llmCalls.Load())
	}
	if joined := strings.Join(updates2(), ""); !strings.Contains(joined, "Interactive reply!") {
		t.Fatalf("expected interactive reply text, got %q", joined)
	}
}
