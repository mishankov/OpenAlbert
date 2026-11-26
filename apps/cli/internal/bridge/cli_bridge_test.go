package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

func TestCLIBridgeLaunchSendWatchAndCrossPlatformArgs(t *testing.T) {
	baseDir := t.TempDir()
	userData := filepath.Join(baseDir, "userdata")
	if err := os.MkdirAll(userData, 0o700); err != nil {
		t.Fatalf("mkdir userdata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userData, "environment-id"), []byte("env-cli-1\n"), 0o600); err != nil {
		t.Fatalf("write environment-id: %v", err)
	}

	var (
		mu         sync.Mutex
		registered int
		cmdResults = map[string]serverclient.T3CommandResultRequest{}
		events     []serverclient.T3ThreadEvent
		sseCh      = make(chan serverclient.T3Command, 8)
		cliCalls   []struct {
			args  []string
			stdin string
		}
		threadStatus = "running"
		pendingCount = 0
		prURL        = ""
		assistantMsg = ""
	)

	piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/hosts/register":
			mu.Lock()
			registered++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.T3HostRegisterResponse{HostID: "h-cli", EnvironmentID: "env-cli-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/heartbeat":
			_ = json.NewEncoder(w).Encode(serverclient.T3HeartbeatResponse{OK: true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/t3/commands/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			flusher.Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case cmd := <-sseCh:
					b, _ := json.Marshal(cmd)
					fmt.Fprintf(w, "event: command\ndata: %s\n\n", b)
					flusher.Flush()
				}
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/t3/commands/") && strings.HasSuffix(r.URL.Path, "/result"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/t3/commands/"), "/result")
			var req serverclient.T3CommandResultRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			cmdResults[id] = req
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/events":
			var ev serverclient.T3ThreadEvent
			_ = json.NewDecoder(r.Body).Decode(&ev)
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer piSrv.Close()

	runner := func(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
		mu.Lock()
		cliCalls = append(cliCalls, struct {
			args  []string
			stdin string
		}{args: append([]string(nil), args...), stdin: stdin})
		st := threadStatus
		pc := pendingCount
		pr := prURL
		msg := assistantMsg
		mu.Unlock()

		if len(args) >= 2 && args[0] == "thread" && args[1] == "start" {
			return json.Marshal(map[string]any{
				"threadId": "th-cli-1",
				"title":    "Fix #64",
				"status":   "running",
				"branch":   "fix/64",
				"modelSelection": map[string]any{
					"instanceId": "antigravity",
					"model":      "gemini-4-neon",
				},
			})
		}
		if len(args) >= 2 && args[0] == "thread" && args[1] == "send" {
			return json.Marshal(map[string]any{"threadId": "th-cli-1", "delivery": "queued"})
		}
		if len(args) >= 2 && args[0] == "thread" && args[1] == "show" {
			msgs := []map[string]any{{"role": "user", "text": "Fix bug"}}
			if msg != "" {
				msgs = append(msgs, map[string]any{"role": "assistant", "text": msg})
			}
			th := map[string]any{
				"threadId":            "th-cli-1",
				"title":               "Fix #64",
				"status":              st,
				"branch":              "fix/64",
				"pendingRequestCount": pc,
				"itemCount":           len(msgs),
			}
			if pr != "" {
				th["linkedPullRequest"] = map[string]any{"url": pr}
			}
			return json.Marshal(map[string]any{"thread": th, "messages": msgs})
		}
		return nil, fmt.Errorf("unexpected args: %v", args)
	}

	cfg := config.Config{
		ServerURL:      piSrv.URL,
		Token:          "tok",
		StateDir:       t.TempDir(),
		CustomStateDir: true,
		HostLabel:      "MacBook Air",
		T3BaseDir:      baseDir,
	}
	b, err := newCLIBridgeWithRunner(cfg, serverclient.New(cfg.ServerURL, cfg.Token, 5*time.Second), nil, runner)
	if err != nil {
		t.Fatalf("newCLIBridgeWithRunner: %v", err)
	}
	b.WatchInterval = 30 * time.Millisecond
	b.HeartbeatInterval = 1 * time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor := func(desc string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if fn() {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s", desc)
	}

	waitFor("host registration", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return registered >= 1
	})

	// 1. Launch via SSE -> verifies --provider, --model, --worktree, --output-schema, stdin "-"
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-launch-1",
		Action:    "thread_launch",
		Payload: json.RawMessage(`{
			"project_id": "proj-1",
			"title": "Fix #64",
			"message": "Investigate issue #64\nLine 2",
			"model_selection": {"provider_instance_id": "antigravity", "model_id": "gemini-4-neon"},
			"workspace_strategy": {"type": "worktree", "base_ref": "origin/master"},
			"output_schema": {"type": "object"}
		}`),
	}
	waitFor("launch result", func() bool {
		mu.Lock()
		defer mu.Unlock()
		res, ok := cmdResults["cmd-launch-1"]
		return ok && res.OK && strings.Contains(string(res.Result), `"th-cli-1"`)
	})

	mu.Lock()
	launchCall := cliCalls[0]
	mu.Unlock()
	joinedArgs := strings.Join(launchCall.args, " ")
	for _, want := range []string{"thread start --json", "--provider antigravity", "--model gemini-4-neon", "--worktree origin/master", "--output-schema ", "proj-1 -"} {
		if !strings.Contains(joinedArgs, want) {
			t.Fatalf("expected launch CLI args to contain %q, got: %s", want, joinedArgs)
		}
	}
	if launchCall.stdin != "Investigate issue #64\nLine 2" {
		t.Fatalf("unexpected stdin message: %q", launchCall.stdin)
	}

	// 2. Pending request count > 0 triggers thread_waiting_input
	mu.Lock()
	pendingCount = 1
	mu.Unlock()
	waitFor("thread_waiting_input event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Kind == "thread_waiting_input" && ev.ThreadID == "th-cli-1" {
				return true
			}
		}
		return false
	})

	// 3. Completion emits thread_completed with PR URL and newest assistant summary from `messages`
	mu.Lock()
	pendingCount = 0
	threadStatus = "completed"
	prURL = "https://github.com/mishankov/polka/pull/99"
	assistantMsg = "Opened PR #99 with regression tests."
	mu.Unlock()
	waitFor("thread_completed event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Kind == "thread_completed" && ev.PRURL == prURL && strings.Contains(ev.Summary, "PR #99") {
				return true
			}
		}
		return false
	})

	// 4. thread_send passes message via stdin
	sseCh <- serverclient.T3Command{
		CommandID: "cmd-send-1",
		Action:    "thread_send",
		Payload:   json.RawMessage(`{"thread_id":"th-cli-1","message":"Please add one more test"}`),
	}
	waitFor("thread_send result", func() bool {
		mu.Lock()
		defer mu.Unlock()
		res, ok := cmdResults["cmd-send-1"]
		return ok && res.OK
	})
}
