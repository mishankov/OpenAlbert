package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/mcpclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

func TestKeepaliveChecksAndWakesNonRunningThread(t *testing.T) {
	var (
		mu       sync.Mutex
		status   = "completed"
		sent     []string
		showCnt  int
		failOnce = true
	)
	k := &KeepaliveBridge{
		cfg:      config.Config{T3BaseDir: "/tmp/t3-home"},
		threadID: "th-bridge-1",
		t3Bin:    "/fake/t3",
		interval: 20 * time.Millisecond,
		runCLI: func(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(args) >= 2 && args[0] == "thread" && args[1] == "show" {
				showCnt++
				if failOnce {
					failOnce = false
					return nil, errors.New("t3 app not running yet")
				}
				return json.Marshal(map[string]any{
					"thread": map[string]any{
						"threadId": "th-bridge-1",
						"status":   status,
					},
				})
			}
			if len(args) >= 2 && args[0] == "thread" && args[1] == "send" {
				sent = append(sent, stdin)
				status = "running"
				return []byte(`{"ok":true}`), nil
			}
			return nil, errors.New("unexpected command")
		},
	}

	ctx := context.Background()
	// 1. Transient error -> no send.
	k.checkOnce(ctx)
	if len(sent) != 0 {
		t.Fatalf("expected 0 sends after show error, got %d", len(sent))
	}
	// 2. Thread completed -> wakes live turn with DefaultBridgeLivePrompt.
	k.checkOnce(ctx)
	if len(sent) != 1 || sent[0] != config.DefaultBridgeLivePrompt {
		t.Fatalf("expected 1 wake send with DefaultBridgeLivePrompt, got %v", sent)
	}
	// 3. Thread is now running -> must NOT send again.
	k.checkOnce(ctx)
	if len(sent) != 1 {
		t.Fatalf("running thread must not be poked again, got %d sends", len(sent))
	}
	// 4. Thread becomes idle -> wakes again.
	mu.Lock()
	status = "idle"
	mu.Unlock()
	k.checkOnce(ctx)
	if len(sent) != 2 {
		t.Fatalf("expected second wake after thread became idle, got %d sends", len(sent))
	}
}

func TestBridgeSteerIdleFallbackArtifactsAttachmentsAndBackgroundGuard(t *testing.T) {
	var (
		mu             sync.Mutex
		sendModes      []string
		sendClientIDs  []string
		lastLaunchMsg  string
		lastReadArgs   map[string]any
		uploadedName   string
		uploadedData   string
		rejectIdleOnce = true
	)

	worktreeDir := t.TempDir()
	imgPath := filepath.Join(worktreeDir, "preview.png")
	if err := os.WriteFile(imgPath, []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}

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
			tools := []string{"t3_environment_read", "t3_project_list", "orchestrator_capabilities", "t3_thread_launch", "t3_thread_send", "t3_thread_read"}
			list := make([]map[string]any, 0, len(tools))
			for _, name := range tools {
				list = append(list, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": list}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			mu.Lock()
			switch p.Name {
			case "t3_environment_read":
				mu.Unlock()
				raw, _ := json.Marshal(map[string]any{"environmentId": "env-parity", "label": "Mac"})
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}}})
				return
			case "t3_project_list":
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"projects":[]}`}}}})
				return
			case "orchestrator_capabilities":
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"providers":[]}`}}}})
				return
			case "t3_thread_launch":
				lastLaunchMsg, _ = p.Arguments["message"].(string)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"threadId":"th-att","status":"running"}`}}}})
				return
			case "t3_thread_send":
				mode, _ := p.Arguments["mode"].(string)
				cid, _ := p.Arguments["clientRequestId"].(string)
				sendModes = append(sendModes, mode)
				sendClientIDs = append(sendClientIDs, cid)
				if mode == "steer" && rejectIdleOnce {
					rejectIdleOnce = false
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0",
						"id":      req.ID,
						"result": map[string]any{
							"isError": true,
							"content": []map[string]string{{"type": "text", "text": "Cannot steer thread: thread is idle with no active turn"}},
						},
					})
					return
				}
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"runId":"run-9"}`}}}})
				return
			case "t3_thread_read":
				lastReadArgs = p.Arguments
				tid, _ := p.Arguments["threadId"].(string)
				mu.Unlock()
				var payload map[string]any
				if tid == "th-bg" {
					payload = map[string]any{
						"thread": map[string]any{
							"threadId":               "th-bg",
							"status":                 "completed",
							"latestRunId":            "run-bg-1",
							"pendingBackgroundTasks": []map[string]any{{"id": "bg-1"}},
						},
						"recentRuns": []map[string]any{{"runId": "run-bg-1", "status": "completed"}},
					}
				} else {
					payload = map[string]any{
						"thread": map[string]any{
							"threadId":     tid,
							"status":       "running",
							"worktreePath": worktreeDir,
						},
					}
				}
				raw, _ := json.Marshal(payload)
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(raw)}}}})
				return
			}
			mu.Unlock()
		}
	}))
	defer mcpSrv.Close()

	mgr, warnings := mcpclient.Connect(context.Background(), []mcpclient.ServerConfig{{Name: "t3-code", Type: "http", URL: mcpSrv.URL}}, nil)
	if len(warnings) > 0 {
		t.Fatalf("MCP warnings: %v", warnings)
	}
	defer mgr.Close()

	piSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/hosts/register":
			_ = json.NewEncoder(w).Encode(serverclient.T3HostRegisterResponse{
				HostID:          "h1",
				EnvironmentID:   "env-parity",
				ActiveThreadIDs: []string{"th-recovered"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/t3/artifacts/art-in-1/content":
			w.Header().Set("Content-Disposition", `attachment; filename="mockup.png"`)
			_, _ = w.Write([]byte("MOCKUPBYTES"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/t3/artifacts":
			_ = r.ParseMultipartForm(1 << 20)
			file, header, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			defer file.Close()
			buf := make([]byte, 64)
			n, _ := file.Read(buf)
			mu.Lock()
			uploadedName = header.Filename
			uploadedData = string(buf[:n])
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(serverclient.T3ArtifactUploadResponse{
				ArtifactID: "t3art-out-1",
				Name:       header.Filename,
				MIME:       "image/png",
				SizeBytes:  int64(n),
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer piSrv.Close()

	cfg := config.Config{
		ServerURL:      piSrv.URL,
		Token:          "tok",
		StateDir:       t.TempDir(),
		CustomStateDir: true,
		T3BaseDir:      t.TempDir(),
	}
	b, ok, err := New(cfg, serverclient.New(cfg.ServerURL, cfg.Token, 5*time.Second), nil)
	if err != nil || !ok {
		t.Fatalf("New failed: ok=%v err=%v", ok, err)
	}
	defer b.Close()

	ctx := context.Background()
	if err := b.SyncCapabilities(ctx, mgr); err != nil {
		t.Fatalf("SyncCapabilities: %v", err)
	}
	if ids := b.ActiveThreadIDs(); len(ids) != 1 || ids[0] != "th-recovered" {
		t.Fatalf("expected active_thread_ids recovery, got %v", ids)
	}

	// 1. thread_launch with attachment_ids downloads file to disk and appends path.
	_, err = b.dispatchCommand(ctx, mgr, serverclient.T3Command{
		CommandID: "cmd-launch-att",
		Action:    "thread_launch",
		Payload:   json.RawMessage(`{"project_id":"p1","title":"Check UI","message":"Inspect mockup","attachment_ids":["art-in-1"],"attachment_conversation_id":"conv-1"}`),
	})
	if err != nil {
		t.Fatalf("thread_launch with attachment_ids failed: %v", err)
	}
	mu.Lock()
	gotLaunchMsg := lastLaunchMsg
	mu.Unlock()
	if !strings.Contains(gotLaunchMsg, "mockup.png") {
		t.Fatalf("expected downloaded attachment path in launch message, got %q", gotLaunchMsg)
	}

	// 2. thread_send with mode="steer" falls back to mode="auto" when thread is idle.
	_, err = b.dispatchCommand(ctx, mgr, serverclient.T3Command{
		CommandID: "cmd-steer-idle",
		Action:    "thread_send",
		Payload:   json.RawMessage(`{"thread_id":"th-att","message":"Continue","mode":"steer"}`),
	})
	if err != nil {
		t.Fatalf("thread_send steer idle fallback failed: %v", err)
	}
	mu.Lock()
	gotModes := append([]string(nil), sendModes...)
	gotCIDs := append([]string(nil), sendClientIDs...)
	mu.Unlock()
	if len(gotModes) != 2 || gotModes[0] != "steer" || gotModes[1] != "auto" {
		t.Fatalf("expected steer->auto fallback sequence, got %v", gotModes)
	}
	if len(gotCIDs) != 2 || gotCIDs[0] != "cmd-steer-idle" || gotCIDs[1] != "cmd-steer-idle:auto" {
		t.Fatalf("expected distinct clientRequestId on fallback, got %v", gotCIDs)
	}

	// 3. artifact_fetch resolves worktreePath via t3_thread_read and uploads file.
	artRes, err := b.dispatchCommand(ctx, mgr, serverclient.T3Command{
		CommandID: "cmd-art-fetch",
		Action:    "artifact_fetch",
		Payload:   json.RawMessage(`{"thread_id":"th-att","path":` + mustJSON(imgPath) + `,"conversation_id":"conv-1"}`),
	})
	if err != nil || !strings.Contains(string(artRes), `"t3art-out-1"`) {
		t.Fatalf("artifact_fetch failed: res=%s err=%v", artRes, err)
	}
	mu.Lock()
	gotUpName, gotUpData := uploadedName, uploadedData
	mu.Unlock()
	if gotUpName != "preview.png" || gotUpData != "PNGDATA" {
		t.Fatalf("unexpected uploaded artifact: name=%q data=%q", gotUpName, gotUpData)
	}

	// 4. thread_read forwards maxCharsPerItem=20000 by default so long worker reports are not truncated at 500 chars.
	if _, err := b.dispatchCommand(ctx, mgr, serverclient.T3Command{
		CommandID: "cmd-read-full",
		Action:    "thread_read",
		Payload:   json.RawMessage(`{"thread_id":"th-att","view":"messages","limit":5}`),
	}); err != nil {
		t.Fatalf("thread_read failed: %v", err)
	}
	mu.Lock()
	gotMaxChars, _ := lastReadArgs["maxCharsPerItem"].(float64)
	mu.Unlock()
	if gotMaxChars != 20000 {
		t.Fatalf("expected thread_read maxCharsPerItem=20000, got %v", gotMaxChars)
	}

	// 5. pendingBackgroundTasks prevents premature thread_completed.
	b.trackThread(&trackedThread{threadID: "th-bg", wasActive: true})
	ev, err := b.pollSingleThread(ctx, mgr, "th-bg", false, false)
	if err != nil {
		t.Fatalf("pollSingleThread th-bg failed: %v", err)
	}
	if ev.Kind != "thread_progress" || ev.Status != "running" {
		t.Fatalf("expected thread_progress/running while pendingBackgroundTasks non-empty, got kind=%q status=%q", ev.Kind, ev.Status)
	}
	if !containsString(b.ActiveThreadIDs(), "th-bg") {
		t.Fatalf("th-bg must stay tracked while pendingBackgroundTasks non-empty, active=%v", b.ActiveThreadIDs())
	}
	mu.Lock()
	gotPollMaxChars, _ := lastReadArgs["maxCharsPerItem"].(float64)
	mu.Unlock()
	if gotPollMaxChars != 4000 {
		t.Fatalf("expected pollSingleThread maxCharsPerItem=4000, got %v", gotPollMaxChars)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func containsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
