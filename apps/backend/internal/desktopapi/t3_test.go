package desktopapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
	"openalbert/apps/backend/internal/t3"
)

func TestT3APIContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		mem := storage.NewMemory()
		t3APIContract(t, mem, mem)
	})
	t.Run("postgres", func(t *testing.T) {
		url := os.Getenv("OPENALBERT_TEST_DATABASE_URL")
		if url == "" {
			t.Skip("set OPENALBERT_TEST_DATABASE_URL to test PostgreSQL persistence")
		}
		store, err := storage.Open(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		t3APIContract(t, store, store)
	})
}

func t3APIContract(t *testing.T, store domain.T3Store, repo domain.Repository) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprintf("api-%d", time.Now().UnixNano())
	ownerID := time.Now().UnixNano()
	envID := nonce + "-env"
	convID := fmt.Sprintf("personal:%d", ownerID)
	if err := repo.SaveConversation(ctx, domain.Conversation{ID: convID, ChatID: ownerID, OwnerID: ownerID, Private: true}); err != nil {
		t.Fatal(err)
	}

	var clockMu sync.Mutex
	clockNow := time.Now().UTC()
	getNow := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clockNow
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		clockNow = clockNow.Add(d)
		clockMu.Unlock()
	}

	cp := t3.New(store, repo, t3.Config{
		OwnerUserID:              ownerID,
		KeepaliveInterval:        40 * time.Millisecond,
		DispatchTimeout:          3 * time.Second,
		CommandRedeliveryTimeout: 10 * time.Second,
		Now:                      getNow,
	})
	api := newAPI(t, repo)
	api.OwnerID = ownerID
	api.T3 = cp
	h := api.Handler()

	// Auth enforced on /api/v1/t3/*.
	unauth := httptest.NewRequest("POST", "/api/v1/t3/hosts/register", strings.NewReader(`{}`))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, unauth)
	if rw.Code != 401 {
		t.Fatalf("expected 401 without bearer token, got %d", rw.Code)
	}

	// 1. POST /api/v1/t3/hosts/register
	regBody := fmt.Sprintf(`{
		"environment_id": %q,
		"label": "MacBook Air",
		"projects": [{"id":"proj-1","title":"OpenAlbert","workspace_root":"/code/openalbert"}],
		"providers": [{
			"provider_instance_id": "ag-1",
			"driver_kind": "antigravity",
			"display_name": "Antigravity",
			"models": [{
				"id": "gemini-4-neon",
				"label": "Gemini 4 Neon",
				"options": [{"id":"effort","values":["low","high"],"default":"high"}]
			}]
		}]
	}`, envID)
	w := call(t, h, "POST", "/api/v1/t3/hosts/register", regBody)
	if w.Code != 200 {
		t.Fatalf("register failed: %d %s", w.Code, w.Body.String())
	}
	regResp := decode[struct {
		HostID        string `json:"host_id"`
		EnvironmentID string `json:"environment_id"`
		RegisteredAt  string `json:"registered_at"`
	}](t, w)
	if regResp.HostID == "" || regResp.EnvironmentID != envID {
		t.Fatalf("unexpected register response: %+v", regResp)
	}
	if _, err := time.Parse(time.RFC3339, regResp.RegisteredAt); err != nil {
		t.Fatalf("registered_at not RFC3339: %q", regResp.RegisteredAt)
	}

	// 2. POST /api/v1/t3/heartbeat
	w = call(t, h, "POST", "/api/v1/t3/heartbeat", fmt.Sprintf(`{"environment_id":%q,"active_thread_ids":["th-1"]}`, envID))
	if w.Code != 200 {
		t.Fatalf("heartbeat failed: %d %s", w.Code, w.Body.String())
	}
	hbResp := decode[struct {
		OK         bool   `json:"ok"`
		ServerTime string `json:"server_time"`
	}](t, w)
	if !hbResp.OK {
		t.Fatalf("heartbeat ok=false: %+v", hbResp)
	}
	if _, err := time.Parse(time.RFC3339, hbResp.ServerTime); err != nil {
		t.Fatalf("server_time not RFC3339: %q", hbResp.ServerTime)
	}
	if w := call(t, h, "POST", "/api/v1/t3/heartbeat", `{"environment_id":"unknown-env-xyz"}`); w.Code != 404 {
		t.Fatalf("expected 404 for unknown heartbeat env, got %d", w.Code)
	}

	// 3a. One-shot JSON drain with ?once=true, Accept: application/json, AND recovery after redelivery timeout.
	cmdOnce, err := cp.EnqueueCommand(ctx, "", envID, domain.T3ActionSyncCapabilities, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	w = call(t, h, "GET", "/api/v1/t3/commands/stream?environment_id="+envID+"&once=true", "")
	if w.Code != 200 {
		t.Fatalf("once drain failed: %d %s", w.Code, w.Body.String())
	}
	drainResp := decode[struct {
		Commands []t3StreamCommand `json:"commands"`
	}](t, w)
	if len(drainResp.Commands) != 1 || drainResp.Commands[0].CommandID != cmdOnce.ID || drainResp.Commands[0].Action != domain.T3ActionSyncCapabilities {
		t.Fatalf("unexpected once drain: %+v", drainResp)
	}
	// Immediate second drain with Accept: application/json returns empty array [].
	reqJSON := httptest.NewRequest("GET", "/api/v1/t3/commands/stream?environment_id="+envID, nil)
	reqJSON.Header.Set("Authorization", "Bearer "+testToken)
	reqJSON.Header.Set("Accept", "application/json")
	rwJSON := httptest.NewRecorder()
	h.ServeHTTP(rwJSON, reqJSON)
	if rwJSON.Code != 200 || !strings.Contains(rwJSON.Body.String(), `"commands":[]`) {
		t.Fatalf("expected empty commands array via Accept header: %d %s", rwJSON.Code, rwJSON.Body.String())
	}
	// Simulate lost one-shot response: after redelivery timeout without /result, same command_id is recovered!
	advanceClock(15 * time.Second)
	w = call(t, h, "GET", "/api/v1/t3/commands/stream?environment_id="+envID+"&once=true", "")
	if w.Code != 200 {
		t.Fatalf("redelivery drain failed: %d %s", w.Code, w.Body.String())
	}
	redelivered := decode[struct {
		Commands []t3StreamCommand `json:"commands"`
	}](t, w)
	if len(redelivered.Commands) != 1 || redelivered.Commands[0].CommandID != cmdOnce.ID {
		t.Fatalf("expected lost one-shot command redelivered under same ID, got %+v", redelivered)
	}
	// Acknowledge cmdOnce so it won't reappear on SSE tests below.
	if w := call(t, h, "POST", "/api/v1/t3/commands/"+cmdOnce.ID+"/result", fmt.Sprintf(`{"environment_id":%q,"ok":true,"result":{}}`, envID)); w.Code != 200 {
		t.Fatalf("ack cmdOnce failed: %d %s", w.Code, w.Body.String())
	}

	// 3b. Partial-batch SSE disconnect recovery: enqueue 2 commands, disconnect after reading frame 1,
	// reconnect and verify BOTH unacknowledged commands are delivered under the exact same command IDs.
	srv := httptest.NewServer(h)
	defer srv.Close()

	batch1, err := cp.EnqueueCommand(ctx, "", envID, domain.T3ActionSyncCapabilities, map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	batch2, err := cp.EnqueueCommand(ctx, "", envID, domain.T3ActionSyncCapabilities, map[string]any{"n": 2})
	if err != nil {
		t.Fatal(err)
	}

	readOneSSE := func(scanner *bufio.Scanner) (eventName, eventID, eventData string, ok bool) {
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventName = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				eventID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				eventData = strings.TrimPrefix(line, "data: ")
			case line == "" && eventData != "":
				return eventName, eventID, eventData, true
			}
		}
		return "", "", "", false
	}

	partialCtx, cancelPartial := context.WithCancel(ctx)
	req1, _ := http.NewRequestWithContext(partialCtx, "GET", srv.URL+"/api/v1/t3/commands/stream?environment_id="+envID, nil)
	req1.Header.Set("Authorization", "Bearer "+testToken)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	sc1 := bufio.NewScanner(resp1.Body)
	_, firstID, _, got1 := readOneSSE(sc1)
	cancelPartial()
	resp1.Body.Close()
	if !got1 || firstID != batch1.ID {
		t.Fatalf("expected first partial SSE frame %s, got %q (ok=%v)", batch1.ID, firstID, got1)
	}
	// Give the server handler a brief moment to run defer unsubscribe().
	time.Sleep(50 * time.Millisecond)

	// Reconnect: must receive both batch1 (unacknowledged) and batch2 under the exact same command IDs!
	recCtx, cancelRec := context.WithCancel(ctx)
	req2, _ := http.NewRequestWithContext(recCtx, "GET", srv.URL+"/api/v1/t3/commands/stream?environment_id="+envID, nil)
	req2.Header.Set("Authorization", "Bearer "+testToken)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	sc2 := bufio.NewScanner(resp2.Body)
	_, recID1, _, ok1 := readOneSSE(sc2)
	_, recID2, _, ok2 := readOneSSE(sc2)
	cancelRec()
	resp2.Body.Close()
	if !ok1 || !ok2 || recID1 != batch1.ID || recID2 != batch2.ID {
		t.Fatalf("reconnect must recover partial batch [%s, %s], got [%q, %q] (ok=%v,%v)", batch1.ID, batch2.ID, recID1, recID2, ok1, ok2)
	}
	for _, id := range []string{batch1.ID, batch2.ID} {
		if w := call(t, h, "POST", "/api/v1/t3/commands/"+id+"/result", fmt.Sprintf(`{"environment_id":%q,"ok":true,"result":{}}`, envID)); w.Code != 200 {
			t.Fatalf("ack %s failed: %d", id, w.Code)
		}
	}
	time.Sleep(50 * time.Millisecond)

	// 3c. Live SSE stream delivers newly enqueued commands instantaneously + keepalive.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	sseReq, err := http.NewRequestWithContext(streamCtx, "GET", srv.URL+"/api/v1/t3/commands/stream?environment_id="+envID, nil)
	if err != nil {
		t.Fatal(err)
	}
	sseReq.Header.Set("Authorization", "Bearer "+testToken)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	if ct := sseResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}

	// DispatchAndWait in background while SSE client reads and posts /commands/{id}/result.
	dispatchDone := make(chan t3.DispatchResult, 1)
	go func() {
		res, err := cp.DispatchAndWait(ctx, "", envID, domain.T3ActionThreadLaunch, map[string]any{
			"project_id":      "proj-1",
			"title":           "SSE Launch",
			"message":         "Build it",
			"conversation_id": convID,
		}, 3*time.Second)
		if err != nil {
			t.Errorf("DispatchAndWait: %v", err)
			return
		}
		dispatchDone <- res
	}()

	scanner := bufio.NewScanner(sseResp.Body)
	var eventName, eventID, eventData string
	sawKeepalive := false
	gotCommand := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!gotCommand || !sawKeepalive) && scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == ": keepalive":
			sawKeepalive = true
		case strings.HasPrefix(line, "event: "):
			eventName = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			eventID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			eventData = strings.TrimPrefix(line, "data: ")
		case line == "" && eventData != "":
			gotCommand = true
		}
	}
	if !gotCommand {
		t.Fatalf("did not receive SSE command frame (keepalive=%v)", sawKeepalive)
	}
	if !sawKeepalive {
		t.Fatal("did not receive SSE keepalive comment")
	}
	if eventName != "command" || eventID == "" {
		t.Fatalf("unexpected SSE headers: event=%q id=%q data=%q", eventName, eventID, eventData)
	}
	var streamed t3StreamCommand
	if err := json.Unmarshal([]byte(eventData), &streamed); err != nil {
		t.Fatalf("parse SSE data: %v (%s)", err, eventData)
	}
	if streamed.CommandID != eventID || streamed.Action != domain.T3ActionThreadLaunch {
		t.Fatalf("unexpected streamed command: %+v", streamed)
	}

	// 4. POST /api/v1/t3/commands/{command_id}/result
	if w := call(t, h, "POST", "/api/v1/t3/commands/"+streamed.CommandID+"/result", `{"environment_id":"other-env","ok":true,"result":{}}`); w.Code != 403 {
		t.Fatalf("expected 403 for mismatched env, got %d %s", w.Code, w.Body.String())
	}
	thID := nonce + "-th-1"
	resBody := fmt.Sprintf(`{"environment_id":%q,"ok":true,"result":{"thread_id":%q,"link":"t3://thread/%s","status":"running","branch":"feat/sse","model":"gemini-4-neon","provider_instance_id":"ag-1"}}`, envID, thID, thID)
	w = call(t, h, "POST", "/api/v1/t3/commands/"+streamed.CommandID+"/result", resBody)
	if w.Code != 200 {
		t.Fatalf("command result failed: %d %s", w.Code, w.Body.String())
	}
	select {
	case res := <-dispatchDone:
		if res.Status != domain.T3CommandStatusSucceeded || res.Thread == nil || res.Thread.ThreadID != thID {
			t.Fatalf("DispatchAndWait did not return thread result: %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("DispatchAndWait did not unblock after /result")
	}
	cancelStream()

	// 5a. Bridge camelCase/singular pending_request normalization ({requestId, question}) through /events, wake, and clear.
	// Use occurred_at BEFORE the server launch clock to also prove S6 (host event clock != server command clock),
	// and include spoofed conversation_id/task_id to prove S8 (persisted thread owner wins).
	evBridgeWaitID := nonce + "-ev-bridge-wait"
	bridgeWaitBody := fmt.Sprintf(`{
		"event_id": %q,
		"environment_id": %q,
		"thread_id": %q,
		"kind": "thread_waiting_input",
		"conversation_id": "personal:999999",
		"task_id": "spoofed-task",
		"pending_request": {"requestId": "req-bridge-7", "question": {"id":"q1","question":"Proceed with deploy?"}},
		"occurred_at": %q
	}`, evBridgeWaitID, envID, thID, getNow().Add(-2*time.Second).Format(time.RFC3339))
	w = call(t, h, "POST", "/api/v1/t3/events", bridgeWaitBody)
	if w.Code != 200 {
		t.Fatalf("bridge waiting_input post failed: %d %s", w.Code, w.Body.String())
	}
	thWaiting, err := cp.Thread(ctx, thID)
	if err != nil || thWaiting.PendingRequest == nil || thWaiting.PendingRequest.RequestID != "req-bridge-7" || len(thWaiting.PendingRequest.Questions) != 1 {
		t.Fatalf("bridge pending_request not normalized: %+v err=%v", thWaiting, err)
	}
	if !strings.Contains(string(thWaiting.PendingRequest.Questions[0]), "Proceed with deploy?") {
		t.Fatalf("bridge question lost: %s", thWaiting.PendingRequest.Questions[0])
	}
	incomingAfterWait, _ := repo.PendingIncoming(ctx, 100)
	foundBridgeWake := false
	for _, in := range incomingAfterWait {
		if in.ID == "t3-event:"+evBridgeWaitID && strings.Contains(in.Text, "req-bridge-7") && strings.Contains(in.Text, "Proceed with deploy?") {
			foundBridgeWake = true
		}
	}
	if !foundBridgeWake {
		t.Fatalf("owner wake missing normalized request_id/question: %+v", incomingAfterWait)
	}

	// 5b. POST /api/v1/t3/events (idempotent by event_id, wakes owning conversation).
	evID := nonce + "-ev-completed"
	evBody := fmt.Sprintf(`{
		"event_id": %q,
		"environment_id": %q,
		"thread_id": %q,
		"kind": "thread_completed",
		"status": "completed",
		"pr_url": "https://github.com/org/repo/pull/42",
		"output": {"tests_passed": true},
		"summary": "All tests green",
		"occurred_at": %q
	}`, evID, envID, thID, getNow().Add(2*time.Second).Format(time.RFC3339))
	w = call(t, h, "POST", "/api/v1/t3/events", evBody)
	if w.Code != 200 {
		t.Fatalf("events post failed: %d %s", w.Code, w.Body.String())
	}
	evResp1 := decode[struct {
		OK           bool `json:"ok"`
		Deduplicated bool `json:"deduplicated"`
	}](t, w)
	if !evResp1.OK || evResp1.Deduplicated {
		t.Fatalf("first event should not be deduplicated: %+v", evResp1)
	}
	w = call(t, h, "POST", "/api/v1/t3/events", evBody)
	if w.Code != 200 {
		t.Fatalf("events replay failed: %d %s", w.Code, w.Body.String())
	}
	evResp2 := decode[struct {
		OK           bool `json:"ok"`
		Deduplicated bool `json:"deduplicated"`
	}](t, w)
	if !evResp2.OK || !evResp2.Deduplicated {
		t.Fatalf("replayed event must be deduplicated: %+v", evResp2)
	}
	if w := call(t, h, "POST", "/api/v1/t3/events", fmt.Sprintf(`{"event_id":"%s-bad","environment_id":%q,"thread_id":%q,"kind":"bogus"}`, nonce, envID, thID)); w.Code != 400 {
		t.Fatalf("expected 400 for invalid kind, got %d", w.Code)
	}

	th, err := cp.Thread(ctx, thID)
	if err != nil || th.Status != "completed" || th.PRURL != "https://github.com/org/repo/pull/42" || th.LastSummary != "All tests green" || th.PendingRequest != nil {
		t.Fatalf("thread not updated from event: %+v err=%v", th, err)
	}
	incoming, err := repo.PendingIncoming(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	wakes := 0
	for _, in := range incoming {
		if in.ID == "t3-event:"+evID {
			wakes++
		}
	}
	if wakes != 1 {
		t.Fatalf("expected 1 wake message for %s, got %d", evID, wakes)
	}

	// 5c. Deferred wake on PostgreSQL/memory when an event with empty occurred_at ("")
	// arrives BEFORE its thread_launch result (S10).
	deferredCmd, err := cp.EnqueueCommand(ctx, "", envID, domain.T3ActionThreadLaunch, map[string]any{
		"project_id":      "proj-1",
		"title":           "Deferred Empty Timestamp",
		"message":         "quick run",
		"conversation_id": convID,
	})
	if err != nil {
		t.Fatal(err)
	}
	deferredThID := nonce + "-th-deferred"
	deferredEvID := nonce + "-ev-empty-ts"
	emptyTsBody := fmt.Sprintf(`{
		"event_id": %q,
		"environment_id": %q,
		"thread_id": %q,
		"kind": "thread_completed",
		"summary": "Done with empty occurred_at",
		"occurred_at": ""
	}`, deferredEvID, envID, deferredThID)
	if w := call(t, h, "POST", "/api/v1/t3/events", emptyTsBody); w.Code != 200 {
		t.Fatalf("empty occurred_at event failed: %d %s", w.Code, w.Body.String())
	}
	// Commit launch result afterwards -> must replay stored event and wake owner cleanly!
	deferredRes := fmt.Sprintf(`{"environment_id":%q,"ok":true,"result":{"thread_id":%q,"status":"running"}}`, envID, deferredThID)
	if w := call(t, h, "POST", "/api/v1/t3/commands/"+deferredCmd.ID+"/result", deferredRes); w.Code != 200 {
		t.Fatalf("deferred launch result failed (S10): %d %s", w.Code, w.Body.String())
	}
	incomingAfterDeferred, err := repo.PendingIncoming(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	foundDeferredWake := false
	for _, in := range incomingAfterDeferred {
		if in.ID == "t3-event:"+deferredEvID && in.Conversation.ID == convID && strings.Contains(in.Text, "Done with empty occurred_at") {
			foundDeferredWake = true
		}
	}
	if !foundDeferredWake {
		t.Fatalf("expected deferred wake for empty occurred_at event %s in conversation %s", deferredEvID, convID)
	}
}
