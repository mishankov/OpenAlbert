package agent

import (
	"context"
	"encoding/json"
	"errors"
	"go.temporal.io/sdk/temporal"
	"jarvis/internal/domain"
	"strings"
	"testing"
	"time"
)

type fakeBrowser struct {
	calls         int
	owner         domain.BrowserOwner
	result        domain.BrowserResult
	err           error
	closed        []string
	closedTasks   []string
	beforeFailure bool
}

func (f *fakeBrowser) Execute(ctx context.Context, o domain.BrowserOwner, r domain.BrowserRequest, before func() error) (domain.BrowserResult, error) {
	if f.err != nil {
		return domain.BrowserResult{}, f.err
	}
	if before != nil {
		if err := before(); err != nil {
			return domain.BrowserResult{}, err
		}
	}
	f.calls++
	f.owner = o
	if f.beforeFailure {
		return domain.BrowserResult{}, errors.New("connection lost after effect")
	}
	return f.result, nil
}
func (f *fakeBrowser) CloseSession(id string)               { f.closed = append(f.closed, id) }
func (f *fakeBrowser) CloseTask(id string)                  { f.closedTasks = append(f.closedTasks, id) }
func (f *fakeBrowser) Reconcile(map[string]bool, time.Time) {}
func (f *fakeBrowser) Close() error                         { return nil }
func TestBrowserCapabilityArtifactsAndRetryReceipts(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	for _, tool := range a.toolDefinitions() {
		if strings.HasPrefix(tool.Name, "browser_") {
			t.Fatal("disabled browser tools advertised")
		}
	}
	f := &fakeBrowser{result: domain.BrowserResult{InstanceID: "browser-one", PageID: "page-one", Data: []byte("PNG"), MIME: "image/png", Filename: "browser.png"}}
	a.Browser = f
	found := false
	for _, tool := range a.toolDefinitions() {
		if tool.Name == "browser_open" {
			found = true
		}
	}
	if !found {
		t.Fatal("enabled tools hidden")
	}
	in := ToolInput{RunKey: "browser-run", SessionID: "browser-session", Conversation: conv, Call: domain.ToolCall{ID: "shot", Name: "browser_screenshot", Arguments: json.RawMessage(`{"page_id":"page-one"}`)}}
	result, err := a.BrowserToolCall(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var saved domain.BrowserResult
	if err = json.Unmarshal([]byte(result), &saved); err != nil || saved.ArtifactID == "" {
		t.Fatal("screenshot artifact not returned", result, err)
	}
	artifact, ok, err := repo.Artifact(ctx, conv.ID, saved.ArtifactID)
	if err != nil || !ok || string(artifact.Data) != "PNG" {
		t.Fatal("screenshot not persisted")
	}
	_, err = a.BrowserToolCall(ctx, in)
	if err != nil || f.calls != 1 {
		t.Fatal("receipt replay repeated browser work")
	}
	if f.owner.SessionID != in.SessionID || f.owner.ConversationID != conv.ID || f.owner.Background {
		t.Fatal("browser owner derived incorrectly")
	}
}
func TestBrowserCapacityRetriesWithoutCachingOrPerformingAnAction(t *testing.T) {
	a, _, _, conv := fixture(t)
	f := &fakeBrowser{err: domain.ErrBrowserCapacity}
	a.Browser = f
	in := ToolInput{RunKey: "capacity", SessionID: "session", Conversation: conv, Call: domain.ToolCall{ID: "open", Name: "browser_open", Arguments: json.RawMessage(`{"url":"https://example.com"}`)}}
	_, err := a.BrowserToolCall(context.Background(), in)
	var capacity *temporal.ApplicationError
	if !errors.As(err, &capacity) || capacity.Type() != "browser_capacity" {
		t.Fatal("capacity was cached as a tool error", err)
	}
	f.err = nil
	if out, err := a.BrowserToolCall(context.Background(), in); err != nil || strings.Contains(out, `"error"`) || f.calls != 1 {
		t.Fatal("retry did not resume after admission", out, err)
	}
}
func TestAmbiguousBrowserWriteIsNeverBlindlyReplayed(t *testing.T) {
	a, repo, _, conv := fixture(t)
	f := &fakeBrowser{beforeFailure: true}
	a.Browser = f
	in := ToolInput{RunKey: "write", SessionID: "session", Conversation: conv, Call: domain.ToolCall{ID: "click", Name: "browser_click", Arguments: json.RawMessage(`{"page_id":"p","ref":"r"}`)}}
	_, err := a.BrowserToolCall(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a lost completion receipt while keeping the durable attempt marker.
	key := "tool/" + ID(in.RunKey, "0", in.Call.ID)
	if err = repo.SaveCheckpoint(context.Background(), key, json.RawMessage(`{"error":"previous result lost"}`)); err != nil {
		t.Fatal(err)
	}
	_, err = a.BrowserToolCall(context.Background(), in)
	if err != nil || f.calls != 1 {
		t.Fatal("cached retry repeated click")
	}
	// A fresh execution path with a pre-existing attempt but no result must also stop.
	in.Call.ID = "unacknowledged"
	attempt := "browser-attempt/" + ID(in.RunKey, in.Call.ID, in.SessionID+"/0")
	if err = repo.SaveCheckpoint(context.Background(), attempt, json.RawMessage(`true`)); err != nil {
		t.Fatal(err)
	}
	out, err := a.BrowserToolCall(context.Background(), in)
	if err != nil || !strings.Contains(out, "may already have executed") || f.calls != 1 {
		t.Fatal("uncertain effect repeated", out, err)
	}
}
func TestBrowserLifetimeFollowsSessionAndBackgroundRun(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	f := &fakeBrowser{}
	a.Browser = f
	session := domain.Session{ID: "interactive", ConversationID: conv.ID}
	if err := repo.StartSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := a.Finish(ctx, FinishInput{Turn: Turn{Input: RunInput{Key: "turn", SessionID: session.ID}}}); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 0 {
		t.Fatal("browser closed after reply instead of session end")
	}
	if err := a.CloseSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != session.ID {
		t.Fatal("session closure did not release browser")
	}
	if err := a.Finish(ctx, FinishInput{Turn: Turn{Input: RunInput{Key: "task-run", SessionID: "task-session", TaskID: "task"}}}); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 2 || f.closed[1] != "task-session" {
		t.Fatal("background browser retained during schedule wait")
	}
	task, err := repo.CreateTask(ctx, domain.Task{ID: "task", ConversationID: conv.ID, Title: "test", Goal: "test", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	out := tool(t, a, conv, "cancel", "cancel", "task_update", `{"id":"task","status":"cancelled"}`)
	if strings.Contains(out, `"error"`) || len(f.closedTasks) != 1 || f.closedTasks[0] != task.ID {
		t.Fatal("task cancellation did not release browser", out)
	}
	out, err = a.BrowserToolCall(ctx, ToolInput{RunKey: "stale", TaskID: task.ID, SessionID: "queued-session", Conversation: conv, Call: domain.ToolCall{ID: "open", Name: "browser_open", Arguments: json.RawMessage(`{"url":"https://example.com"}`)}})
	if err != nil || !strings.Contains(out, "no longer active") || f.calls != 0 {
		t.Fatal("cancelled capacity wait resumed browser work", out, err)
	}
}

func TestLargeBrowserObservationKeepsStructuredPageAndReferences(t *testing.T) {
	a, _, _, conv := fixture(t)
	f := &fakeBrowser{result: domain.BrowserResult{InstanceID: "instance", PageID: "page", Text: strings.Repeat("😀", 12000)}}
	for range 80 {
		f.result.Elements = append(f.result.Elements, domain.BrowserElement{Ref: "ref", Name: strings.Repeat("я", 180), URL: "https://example.com/" + strings.Repeat("a", 600)})
	}
	a.Browser = f
	out, err := a.BrowserToolCall(context.Background(), ToolInput{RunKey: "large", SessionID: "session", Conversation: conv, Call: domain.ToolCall{ID: "snapshot", Name: "browser_snapshot", Arguments: json.RawMessage(`{"page_id":"page"}`)}})
	var result domain.BrowserResult
	if err != nil || json.Unmarshal([]byte(out), &result) != nil || result.PageID != "page" || !result.Truncated || len(result.Elements) == 0 || len(out) > 28000 {
		t.Fatalf("large observation lost its envelope: %s %v", out, err)
	}
}
