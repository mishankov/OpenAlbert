package desktopapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/knowledge"
	"openalbert/apps/backend/internal/storage"
	"openalbert/apps/backend/internal/telemetry"
)

type traceCaptureRepo struct {
	*storage.Memory
	mu     sync.Mutex
	traces map[string]domain.Trace
}

func newTraceCaptureRepo() *traceCaptureRepo {
	return &traceCaptureRepo{
		Memory: storage.NewMemory(),
		traces: map[string]domain.Trace{},
	}
}

func (r *traceCaptureRepo) SaveTrace(ctx context.Context, tr domain.Trace) error {
	if err := r.Memory.SaveTrace(ctx, tr); err != nil {
		return err
	}
	r.mu.Lock()
	r.traces[tr.ID] = tr
	r.mu.Unlock()
	return nil
}

func (r *traceCaptureRepo) TracesForSession(sessionID string) []domain.Trace {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Trace
	for _, tr := range r.traces {
		if tr.SessionID == sessionID {
			out = append(out, tr)
		}
	}
	return out
}

func TestCodingGatewayContextToolsInspectorSyncAndAnalysis(t *testing.T) {
	var analysisCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4","name":"GPT 5.4"},{"id":"gpt-5.4-mini"}]}`))
		case "/v1/chat/completions":
			var body struct {
				Stream   bool             `json:"stream"`
				Messages []domain.Message `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !body.Stream {
				// Analysis call
				n := analysisCalls.Add(1)
				if n == 1 {
					_, _ = w.Write([]byte(`{"model":"gpt-5.4","choices":[{"message":{"content":"{\"summary\":\"Fixed parser bug\",\"memories\":[{\"content\":\"Owner prefers table-driven Go tests\",\"kind\":\"preference\"}]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`))
				} else {
					_, _ = w.Write([]byte(`{"model":"gpt-5.4","choices":[{"message":{"content":"{\"summary\":\"Resumed session update\",\"memories\":[{\"content\":\"Owner uses gofmt before commits\",\"kind\":\"preference\"}]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`))
				}
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			fmt.Fprint(w, "data: {\"model\":\"gpt-5.4-mini\",\"choices\":[{\"delta\":{\"reasoning_content\":\"checking\"}}]}\n\n")
			flusher.Flush()
			fmt.Fprint(w, "data: {\"model\":\"gpt-5.4-mini\",\"choices\":[{\"delta\":{\"content\":\"All tests pass.\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":15,\"completion_tokens\":4}}\n\n")
			flusher.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	repo := newTraceCaptureRepo()
	ks, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router, err := integrations.NewModelRouter(domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "proxy/gpt-5.4",
			domain.RoleBackground: "proxy/gpt-5.4",
			domain.RoleAnalysis:   "proxy/gpt-5.4",
			domain.RoleCoding:     "proxy/gpt-5.4",
		},
		Providers: map[string]domain.ModelProvider{
			"proxy": {
				Name: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "test-key", DiscoverModels: true,
				DefaultCapabilities:        []string{"tools", "vision", "structured_output"},
				DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"proxy/gpt-5.4": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "test-key", Model: "gpt-5.4", Capabilities: []string{"tools", "vision", "structured_output"},
				ContextWindowTokens: 128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	billing := &integrations.BillingModel{Mode: "profiles", API: router}
	acts := &agent.Activities{
		Codemode:    true,
		OwnerUserID: 42,
		Repo:        repo,
		Knowledge:   ks,
		Model:       billing,
	}
	api := &API{
		Billing:    billing,
		Activities: acts,
		Repo:       repo,
		Memory:     ks,
		OwnerID:    42,
		Token:      strings.Repeat("t", 32),
	}
	srv := httptest.NewServer(telemetry.HTTP(slog.Default(), api.Handler()))
	defer srv.Close()

	doReq := func(method, path string, payload any) *http.Response {
		t.Helper()
		var body bytes.Buffer
		if payload != nil {
			_ = json.NewEncoder(&body).Encode(payload)
		}
		req, err := http.NewRequest(method, srv.URL+path, &body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+api.Token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// 1. Model catalog discovery
	resp := doReq(http.MethodGet, "/api/v1/llm/models", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/v1/llm/models status=%d", resp.StatusCode)
	}
	var catResp struct {
		DefaultModel string                `json:"default_model"`
		Models       []domain.CatalogModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if catResp.DefaultModel != "proxy/gpt-5.4" || len(catResp.Models) != 2 {
		t.Fatalf("unexpected catalog: %+v", catResp)
	}

	// 2. Agent context excludes Telegram delivery tools
	resp = doReq(http.MethodGet, "/api/v1/agent/context", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/v1/agent/context status=%d", resp.StatusCode)
	}
	var ctxResp agent.CodingContext
	if err := json.NewDecoder(resp.Body).Decode(&ctxResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ctxResp.OwnerID != 42 || ctxResp.Soul == "" {
		t.Fatalf("missing owner context: %+v", ctxResp)
	}
	hasMemSearch := false
	for _, tool := range ctxResp.Tools {
		if tool.Name == "send_message" || tool.Name == "send_widget" || tool.Name == "send_group_message" {
			t.Fatalf("telegram tool leaked into coding tools: %s", tool.Name)
		}
		if tool.Name == "memory_search" {
			hasMemSearch = true
		}
	}
	if !hasMemSearch {
		t.Fatal("memory_search missing from coding tools")
	}

	// 3. Create coding session
	sessionID := "coding-sess-1"
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions", map[string]any{
		"session_id": sessionID, "title": "OpenAlbert", "cwd": "/tmp/openalbert",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create session status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Coding conversations must NOT appear in ListPrivateConversations (no InitiativeWorkflow).
	privs, err := repo.ListPrivateConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range privs {
		if strings.HasPrefix(c.ID, "coding:") {
			t.Fatalf("coding conversation leaked into private initiative list: %+v", c)
		}
	}

	// 4. Streaming LLM complete records deltas and traces
	resp = doReq(http.MethodPost, "/api/v1/llm/complete", map[string]any{
		"model":      "proxy/gpt-5.4-mini",
		"session_id": sessionID,
		"turn_id":    "turn-1",
		"step":       0,
		"messages":   []domain.Message{{Role: "user", Content: "Run tests"}},
	})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected SSE status/header: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	var events []domain.StreamEvent
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev domain.StreamEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	resp.Body.Close()
	if len(events) < 3 || events[len(events)-1].Type != "done" || events[len(events)-1].Completion.Message.Content != "All tests pass." {
		t.Fatalf("unexpected SSE events: %+v", events)
	}
	// Verify completed model trace retained both Input and Output.
	traces := repo.TracesForSession(sessionID)
	if len(traces) != 1 || traces[0].Status != "completed" || len(traces[0].Input) == 0 || len(traces[0].Output) == 0 {
		t.Fatalf("expected completed model trace with Input and Output preserved, got %+v", traces)
	}

	// 5. Execute server tool (memory_update allowed, send_message and send_group_message blocked)
	resp = doReq(http.MethodPost, "/api/v1/agent/tools/memory_update", map[string]any{
		"session_id": sessionID, "turn_id": "turn-1", "step": 0, "call_id": "tc-1",
		"arguments": json.RawMessage(`{"content":"Always run go test -race","kind":"preference"}`),
	})
	var toolRes agent.CodingToolResult
	_ = json.NewDecoder(resp.Body).Decode(&toolRes)
	resp.Body.Close()
	if resp.StatusCode != 200 || toolRes.Failed {
		t.Fatalf("memory_update failed: %d %+v %s", resp.StatusCode, toolRes, toolRes.Output)
	}
	for _, blockedTool := range []string{"send_message", "send_group_message"} {
		resp = doReq(http.MethodPost, "/api/v1/agent/tools/"+blockedTool, map[string]any{
			"session_id": sessionID, "arguments": json.RawMessage(`{"text":"nope"}`),
		})
		toolRes = agent.CodingToolResult{}
		_ = json.NewDecoder(resp.Body).Decode(&toolRes)
		resp.Body.Close()
		if !toolRes.Failed {
			t.Fatalf("%s should be blocked for coding sessions", blockedTool)
		}
	}

	// 6. Sync messages + local trace, close session, and verify post-session analysis writes into personal:42
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/"+sessionID+"/sync", map[string]any{
		"messages": []map[string]any{
			{"key": "turn-1/user", "message": domain.Message{Role: "user", Content: "Always write table-driven Go tests"}},
			{"key": "turn-1/assistant/0", "message": domain.Message{Role: "assistant", Content: "All tests pass."}},
		},
		"traces": []domain.Trace{{
			ID: "local-tool-1", TurnID: "turn-1", Kind: "tool", Name: "bash", Status: "completed",
		}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("sync status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/"+sessionID+"/close", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("close status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	pending, err := repo.PendingAnalysis(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != sessionID {
		t.Fatalf("closed coding session not queued for analysis: %+v %v", pending, err)
	}
	if err := acts.Analyze(context.Background(), pending[0]); err != nil {
		t.Fatalf("Analyze failed on coding session: %v", err)
	}
	entries, err := ks.Search(context.Background(), "personal:42", "table-driven")
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected coding analysis memory in personal:42, got %+v %v", entries, err)
	}

	// 7. Reopening a closed, analyzed coding session uses a new server session generation under the same coding:<id> conversation.
	gen2ID := sessionID + "-g2"
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions", map[string]any{
		"session_id": gen2ID, "conversation_id": "coding:" + sessionID, "title": "OpenAlbert", "cwd": "/tmp/openalbert",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("create gen2 session status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/"+gen2ID+"/sync", map[string]any{
		"conversation_id": "coding:" + sessionID,
		"messages": []map[string]any{
			{"key": "turn-2/user", "message": domain.Message{Role: "user", Content: "Always run gofmt before commits"}},
			{"key": "turn-2/assistant/0", "message": domain.Message{Role: "assistant", Content: "Noted."}},
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("sync gen2 status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/"+gen2ID+"/close", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("close gen2 status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	pending, err = repo.PendingAnalysis(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != gen2ID || pending[0].ConversationID != "coding:"+sessionID {
		t.Fatalf("reopened coding session gen2 not queued for analysis: %+v %v", pending, err)
	}
	if err := acts.Analyze(context.Background(), pending[0]); err != nil {
		t.Fatalf("Analyze failed on gen2 coding session: %v", err)
	}
	entries, err = ks.Search(context.Background(), "personal:42", "gofmt")
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected resumed coding analysis memory in personal:42, got %+v %v", entries, err)
	}

	// 8. Coding endpoints must reject writes/closes against non-coding sessions, mismatched conversations, and closed sessions.
	if err := repo.StartSession(context.Background(), domain.Session{
		ID: "personal-sess-1", ConversationID: "personal:42", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/personal-sess-1/sync", map[string]any{
		"messages": []map[string]any{{"key": "k1", "message": domain.Message{Role: "user", Content: "injected"}}},
	})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 syncing to personal session, got %d", resp.StatusCode)
	}
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/personal-sess-1/close", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 closing personal session via coding endpoint, got %d", resp.StatusCode)
	}
	// Syncing to an already closed coding session must return 409.
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/"+sessionID+"/sync", map[string]any{
		"conversation_id": "coding:" + sessionID,
		"messages":        []map[string]any{{"key": "late-1", "message": domain.Message{Role: "user", Content: "too late"}}},
	})
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 syncing to closed coding session, got %d", resp.StatusCode)
	}

	// 9. P1-1: Synced trace IDs and turn checkpoints are scoped to the coding session and cannot overwrite another session's records.
	origTrace := domain.Trace{ID: "shared-trace-id", SessionID: "personal-sess-1", TurnID: "pt-1", Kind: "tool", Name: "original", Status: "completed"}
	if err := repo.SaveTrace(context.Background(), origTrace); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveCheckpoint(context.Background(), "turn-done/shared-turn-key", json.RawMessage(`{"owner":"personal"}`)); err != nil {
		t.Fatal(err)
	}
	resp = doReq(http.MethodPost, "/api/v1/coding/sessions/coding-isolated-1/sync", map[string]any{
		"messages": []map[string]any{{"key": "m1", "message": domain.Message{Role: "user", Content: "hi"}}},
		"traces":   []domain.Trace{{ID: "shared-trace-id", TurnID: "ct-1", Kind: "tool", Name: "hijacked", Status: "completed"}},
		"turn_summary": map[string]any{
			"Input": map[string]any{"Key": "shared-turn-key"},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("isolated sync failed: %d", resp.StatusCode)
	}
	personalTraces := repo.TracesForSession("personal-sess-1")
	if len(personalTraces) != 1 || personalTraces[0].Name != "original" {
		t.Fatalf("coding sync overwrote personal session trace: %+v", personalTraces)
	}
	cpRaw, ok, err := repo.LoadCheckpoint(context.Background(), "turn-done/shared-turn-key")
	if err != nil || !ok || string(cpRaw) != `{"owner":"personal"}` {
		t.Fatalf("coding sync overwrote personal turn checkpoint: %q ok=%v err=%v", string(cpRaw), ok, err)
	}

	// 10. P2-6 (Round 5): concurrent creates for the same session_id with conflicting conversation_ids produce exactly one 200 and one 409.
	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, conv := range []string{"coding:race-a", "coding:race-b"} {
		i, conv := i, conv
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := doReq(http.MethodPost, "/api/v1/coding/sessions", map[string]any{
				"session_id":      "coding-create-race",
				"conversation_id": conv,
				"title":           conv,
			})
			statuses[i] = r.StatusCode
			r.Body.Close()
		}()
	}
	wg.Wait()
	if !((statuses[0] == 200 && statuses[1] == 409) || (statuses[0] == 409 && statuses[1] == 200)) {
		t.Fatalf("expected one 200 and one 409 on conflicting concurrent session creates, got %v", statuses)
	}
}

func TestCodingLLMCompleteSuppliesOpaqueSessionHeaderWithoutInspectorTrace(t *testing.T) {
	var gotHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Opencode-Session")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"model\":\"gpt-5.4\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	repo := newTraceCaptureRepo()
	router, err := integrations.NewModelRouter(domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "proxy/gpt-5.4",
			domain.RoleBackground: "proxy/gpt-5.4",
			domain.RoleAnalysis:   "proxy/gpt-5.4",
			domain.RoleCoding:     "proxy/gpt-5.4",
		},
		Providers: map[string]domain.ModelProvider{
			"proxy": {
				Name: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "test-key", DefaultCapabilities: []string{"tools", "structured_output"},
				DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
				DefaultOptions: domain.ModelOptions{SessionHeader: "x-opencode-session"},
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"proxy/gpt-5.4": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "test-key", Model: "gpt-5.4", Capabilities: []string{"tools", "structured_output"},
				ContextWindowTokens: 128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
				Options: domain.ModelOptions{SessionHeader: "x-opencode-session"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api := &API{
		Billing: &integrations.BillingModel{Mode: "profiles", API: router},
		Repo:    repo,
		OwnerID: 42,
		Token:   strings.Repeat("t", 32),
	}
	srv := httptest.NewServer(telemetry.HTTP(slog.Default(), api.Handler()))
	defer srv.Close()

	payload, _ := json.Marshal(map[string]any{
		"model":          "proxy/gpt-5.4",
		"session_id":     "",
		"correlation_id": "ephemeral-abc",
		"turn_id":        "t1",
		"messages":       []domain.Message{{Role: "user", Content: "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/llm/complete", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+api.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || gotHeader != "ephemeral-abc" || !strings.Contains(string(rawBody), `"type":"done"`) {
		t.Fatalf("expected 200 done stream with opaque x-opencode-session header, got status=%d header=%q body=%q", resp.StatusCode, gotHeader, string(rawBody))
	}
	if len(repo.TracesForSession("")) != 0 {
		t.Fatalf("expected no persisted Inspector traces when session_id is empty")
	}
}

func TestCodingLLMCompletePersistsCancelledTraceWithInput(t *testing.T) {
	started := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-releaseUpstream:
		}
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	repo := newTraceCaptureRepo()
	router, err := integrations.NewModelRouter(domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "proxy/gpt-5.4",
			domain.RoleBackground: "proxy/gpt-5.4",
			domain.RoleAnalysis:   "proxy/gpt-5.4",
			domain.RoleCoding:     "proxy/gpt-5.4",
		},
		Providers: map[string]domain.ModelProvider{
			"proxy": {
				Name: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "test-key", DefaultCapabilities: []string{"tools", "structured_output"},
				DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"proxy/gpt-5.4": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "test-key", Model: "gpt-5.4", Capabilities: []string{"tools", "structured_output"},
				ContextWindowTokens: 128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api := &API{
		Billing: &integrations.BillingModel{Mode: "profiles", API: router},
		Repo:    repo,
		OwnerID: 42,
		Token:   strings.Repeat("t", 32),
	}
	srv := httptest.NewServer(telemetry.HTTP(slog.Default(), api.Handler()))
	defer srv.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"model":      "proxy/gpt-5.4",
		"session_id": "coding-cancel-1",
		"turn_id":    "turn-1",
		"step":       0,
		"messages":   []domain.Message{{Role: "user", Content: "Slow request"}},
	})
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost, srv.URL+"/api/v1/llm/complete", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+api.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was never reached")
	}
	cancel()
	if err == nil {
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		traces := repo.TracesForSession("coding-cancel-1")
		if len(traces) == 1 && traces[0].Status == "cancelled" {
			if len(traces[0].Input) == 0 || traces[0].EndedAt == nil {
				t.Fatalf("cancelled trace lost Input or EndedAt: %+v", traces[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	traces := repo.TracesForSession("coding-cancel-1")
	t.Fatalf("expected cancelled trace persisted after client abort, got %+v", traces)
}
