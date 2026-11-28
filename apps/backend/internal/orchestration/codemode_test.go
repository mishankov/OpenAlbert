package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
)

type codemodeTestModel func([]domain.Message, []domain.Tool) domain.Message

func (m codemodeTestModel) Complete(_ context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	return domain.Completion{Message: m(messages, tools)}, nil
}

func script(id, source string) domain.ToolCall {
	return call(id, agent.CodemodeName, map[string]string{"code": source})
}

type concurrentReads struct {
	fakeSearch
	started atomic.Int32
	ready   chan struct{}
}

func (r *concurrentReads) Read(ctx context.Context, url string) (string, error) {
	if r.started.Add(1) == 2 {
		close(r.ready)
	}
	select {
	case <-r.ready:
		return url + " private fixture evidence", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestCodemodeRunsParallelReadsAndReplaysQueuedReply(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Codemode = true
	reads := &concurrentReads{ready: make(chan struct{})}
	a.Search = reads
	var modelSteps, replyAttempts atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, tools []domain.Tool) domain.Message {
		if len(tools) != 1 || tools[0].Name != agent.CodemodeName {
			t.Errorf("model catalog: %+v", tools)
		}
		switch modelSteps.Add(1) {
		case 1:
			return calls(script("work", `
const pages = await Promise.all([
  tools.web_read({url: "https://example.org/first"}),
  tools.web_read({url: "https://example.org/second"})
])
const text = pages.map(page => page.split(" ")[0].split("/").pop()).join(", ")
store("summary", text)
const receipt = await tools.send_message({text})
return {summary: text, delivery: receipt.state}
`))
		case 2:
			last := messages[len(messages)-1]
			if last.Role != "tool" || last.ToolCallID != "work" || !strings.Contains(last.Content, `"summary":"first, second"`) ||
				!strings.Contains(last.Content, `"delivery":"queued"`) {
				t.Errorf("unexpected model-facing result: %+v", last)
			}
			for _, msg := range messages {
				if msg.ParentToolCallID != "" || strings.Contains(msg.Content, "private fixture evidence") {
					t.Errorf("child exchange crossed model boundary: %+v", msg)
				}
			}
			return calls(script("state", `return load("summary")`))
		default:
			if last := messages[len(messages)-1]; !strings.Contains(last.Content, `"output":"first, second"`) {
				t.Errorf("lost session state: %+v", last)
			}
			return finish("")
		}
	})
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 4)
	r.hooks = &activityHooks{
		toolCall: func(ctx context.Context, in agent.ToolInput, next func(context.Context, agent.ToolInput) (string, error)) (string, error) {
			result, err := next(ctx, in)
			if err == nil && in.Call.Name == "send_message" && replyAttempts.Add(1) == 1 {
				return "", errors.New("activity completion lost after queuing reply")
			}
			return result, err
		},
	}
	if err := r.SignalSession(context.Background(), conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)

	outgoing, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(outgoing) != 1 || outgoing[0].Text != "first, second" {
		t.Fatalf("expected one queued reply, got %+v, %v", outgoing, err)
	}
	if reads.started.Load() != 2 || modelSteps.Load() != 3 || replyAttempts.Load() != 2 {
		t.Fatalf("read calls %d, model calls %d, reply attempts %d", reads.started.Load(), modelSteps.Load(), replyAttempts.Load())
	}
	records, err := repo.Messages(context.Background(), outgoing[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var childRecords int
	for _, msg := range records {
		if msg.ParentToolCallID == "work" {
			childRecords++
		}
	}
	if childRecords != 6 {
		t.Fatalf("expected three audit exchanges, got %d child records", childRecords)
	}
}

func TestCodemodeRejectsNativeCallsAndRestrictsFinalization(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Codemode = true
	var modelSteps atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, tools []domain.Tool) domain.Message {
		if len(tools) != 1 || tools[0].Name != agent.CodemodeName {
			t.Errorf("model catalog: %+v", tools)
		}
		if modelSteps.Add(1) == 1 {
			return calls(call("forged", "send_message", map[string]string{"text": "must not be queued"}))
		}
		if messages[len(messages)-2].ToolCallID != "forged" {
			t.Errorf("missing direct-tool rejection: %+v", messages)
		}
		return calls(script("final", `
if (typeof tools.task_create !== "undefined") throw Error("finalization allows task writes")
if (typeof tools.web_read !== "undefined") throw Error("finalization allows web reads")
await tools.send_message({text: "finalized"})
`))
	})
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 1)
	if err := r.SignalSession(context.Background(), conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	outgoing, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(outgoing) != 1 || outgoing[0].Text != "finalized" {
		t.Fatalf("unexpected outgoing: %+v, %v", outgoing, err)
	}
}

func TestScheduledTaskUsesCodemode(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Codemode = true
	const taskID = "codemode-scheduled"
	if _, err := repo.CreateTask(context.Background(), domain.Task{ID: taskID, ConversationID: conv.ID, Title: "Check", Goal: "Run check", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	var modelSteps atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, tools []domain.Tool) domain.Message {
		if len(tools) != 1 || tools[0].Name != agent.CodemodeName {
			t.Errorf("model catalog: %+v", tools)
		}
		if modelSteps.Add(1) == 1 {
			return calls(script("task", `await tools.task_update({id: "codemode-scheduled", result: "checked"})`))
		}
		var out codemodeOutput
		last := messages[len(messages)-1]
		if err := json.Unmarshal([]byte(last.Content), &out); err != nil || out.Error != "" {
			t.Errorf("task update result: %s", last.Content)
		}
		return finish("")
	})
	r := newTestRuntime(a, nil, time.Second, 3)
	if _, err := r.runTurn(context.Background(), agent.RunInput{Key: "codemode-task-run", SessionID: "codemode-task-session", ConversationID: conv.ID, TaskID: taskID}, 3); err != nil {
		t.Fatal(err)
	}
	task, found, err := repo.Task(context.Background(), conv.ID, taskID)
	if err != nil || !found || task.Result != "checked" {
		t.Fatalf("task mutation missing: %+v, %v", task, err)
	}
}

type lostToolReceipt struct {
	domain.Repository
	key string
}

func (r lostToolReceipt) SaveCheckpoint(ctx context.Context, key string, value json.RawMessage) error {
	if key == r.key {
		return errors.New("worker lost the mutation receipt")
	}
	return r.Repository.SaveCheckpoint(ctx, key, value)
}

func TestCodemodeDoesNotRepeatMutationWithLostReceipt(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Codemode = true
	const taskID, runKey = "codemode-mutation", "codemode-uncertain"
	original, err := repo.CreateTask(context.Background(), domain.Task{ID: taskID, ConversationID: conv.ID, Title: "Check", Goal: "Run check", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	childID := agent.ID("update", "0", "codemode")
	a.Repo = lostToolReceipt{Repository: repo, key: "tool/" + agent.ID(runKey, "0", childID)}
	var modelSteps atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, _ []domain.Tool) domain.Message {
		if modelSteps.Add(1) == 1 {
			return calls(script("update", `
try {
  await tools.task_update({id: "codemode-mutation", result: "checked"})
} catch (error) {
  return error.code
}
`))
		}
		last := messages[len(messages)-1]
		if !strings.Contains(last.Content, `"output":"tool_outcome_unknown"`) || !strings.Contains(last.Content, `"status":"unknown"`) {
			t.Errorf("missing uncertain outcome: %s", last.Content)
		}
		return finish("")
	})
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 3)
	var runErr error
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		in := agent.RunInput{Key: runKey, SessionID: runKey, ConversationID: conv.ID, TaskID: taskID}
		_, runErr = r.runTurn(context.Background(), in, 3)
	}()
	waitWithFakeClock(t, r, clk)
	if runErr != nil {
		t.Fatal(runErr)
	}
	task, found, err := repo.Task(context.Background(), conv.ID, taskID)
	if err != nil || !found || task.Result != "checked" || task.Version != original.Version+1 {
		t.Fatalf("mutation was lost or repeated: %+v, %v", task, err)
	}
}

type screenshotBrowser struct {
	domain.Browser
	data []byte
}

func (b screenshotBrowser) Execute(_ context.Context, _ domain.BrowserOwner, request domain.BrowserRequest, before func() error) (domain.BrowserResult, error) {
	if err := before(); err != nil {
		return domain.BrowserResult{}, err
	}
	if request.Action != "screenshot" {
		return domain.BrowserResult{}, errors.New("unexpected browser action")
	}
	return domain.BrowserResult{MIME: "image/png", Filename: "page.png", Data: b.data}, nil
}

func TestCodemodeBrowserActivityPreservesImageAttachments(t *testing.T) {
	a, _, _, conv := newFixture(t)
	a.Codemode = true
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	a.Browser = screenshotBrowser{data: img.Bytes()}
	var modelSteps atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, _ []domain.Tool) domain.Message {
		if modelSteps.Add(1) == 1 {
			return calls(script("screenshot", `
const screenshot = await tools.browser_screenshot({page_id: "page-1"})
await tools.artifact_read({id: screenshot.artifact_id})
return screenshot.artifact_id
`))
		}
		last := messages[len(messages)-1]
		if len(last.Images) != 1 || !strings.HasPrefix(last.Images[0], "data:image/jpeg;base64,") ||
			messages[len(messages)-2].ToolCallID != "screenshot" {
			t.Errorf("missing multimodal tool result: %+v", last)
		}
		return finish("")
	})
	r := newTestRuntime(a, nil, time.Second, 3)
	r.hooks = &activityHooks{
		browserCall: func(ctx context.Context, lane string, in agent.ToolInput, next func(context.Context, agent.ToolInput) (string, error)) (string, error) {
			if lane != ".browser-ui" {
				t.Errorf("unexpected browser lane %q", lane)
			}
			return next(ctx, in)
		},
	}
	if _, err := r.runTurn(context.Background(), agent.RunInput{Key: "codemode-browser", SessionID: "codemode-browser", ConversationID: conv.ID, IncomingIDs: []string{"first"}}, 3); err != nil {
		t.Fatal(err)
	}
}

func TestCodemodeSteeringRetainsCompletedActions(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	a.Codemode = true
	ctx := context.Background()
	if _, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "second", Conversation: conv, Text: "Stop sending more parts."}, 2); err != nil {
		t.Fatal(err)
	}
	var modelSteps atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, _ []domain.Tool) domain.Message {
		if modelSteps.Add(1) == 1 {
			return calls(script("parts", `
await tools.send_message({text: "First part"})
await tools.send_message({text: "Second part"})
`))
		}
		last := messages[len(messages)-1]
		if last.Role != "user" || last.Content != "Stop sending more parts." {
			t.Errorf("steering input missing: %+v", last)
		}
		previous := messages[len(messages)-2]
		var output codemodeOutput
		if previous.ToolCallID != "parts" || json.Unmarshal([]byte(previous.Content), &output) != nil ||
			len(output.Actions) != 1 || output.Actions[0].Name != "send_message" || output.Actions[0].Status != "queued" {
			t.Errorf("new model call lost the completed reply: %+v", previous)
		}
		return finish("")
	})
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 5)
	var steered atomic.Bool
	r.hooks = &activityHooks{
		onStart: func(name string, arg any) {
			if name != "PersistMessage" {
				return
			}
			p, ok := arg.(persistMessageArg)
			if ok && p.Message.ParentToolCallID == "parts" &&
				len(p.Message.ToolCalls) == 1 && strings.Contains(string(p.Message.ToolCalls[0].Arguments), "Second part") &&
				steered.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "second"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	outgoing, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || !steered.Load() || len(outgoing) != 1 || outgoing[0].Text != "First part" || modelSteps.Load() != 2 {
		t.Fatalf("unexpected model steps or replies: %d, %+v, %v", modelSteps.Load(), outgoing, err)
	}
}

type uncertainComputer struct{}

func (uncertainComputer) Execute(_ context.Context, request domain.ComputerRequest) (domain.ComputerResult, error) {
	if request.Action == "exec" {
		return domain.ComputerResult{}, errors.New("connection lost")
	}
	return domain.ComputerResult{State: "completed"}, nil
}

func TestCodemodePreservesExecutionReceiptsInModelContext(t *testing.T) {
	t.Run("tool error", func(t *testing.T) {
		checkCodemodeExecutionReceipt(t, false)
	})
	t.Run("lost activity completion", func(t *testing.T) {
		checkCodemodeExecutionReceipt(t, true)
	})
}

func checkCodemodeExecutionReceipt(t *testing.T, loseCompletion bool) {
	t.Helper()
	a, _, _, conv := newFixture(t)
	a.Codemode, a.OwnerUserID, a.Computer = true, conv.OwnerID, uncertainComputer{}
	var modelSteps atomic.Int32
	a.Model = codemodeTestModel(func(messages []domain.Message, _ []domain.Tool) domain.Message {
		if modelSteps.Add(1) == 1 {
			return calls(script("run", `await tools.computer_exec({command: "pwd", wait_seconds: 0})`))
		}
		last := messages[len(messages)-1]
		var output codemodeOutput
		if err := json.Unmarshal([]byte(last.Content), &output); err != nil || len(output.Actions) != 1 {
			t.Errorf("model received a broken receipt: %v", err)
			return finish("")
		}
		action := output.Actions[0]
		if last.ToolCallID == "run" {
			if output.Error == "" || action.Status != "unknown" || len(action.ExecutionID) != 32 {
				t.Errorf("model cannot inspect uncertain execution: %+v", output)
				return finish("")
			}
			return calls(script("status", fmt.Sprintf(`await tools.computer_execution_status({execution_id: %q}); return "x".repeat(32000)`, action.ExecutionID)))
		}
		if !output.OutputTruncated || action.Name != "computer_execution_status" || action.Status != "completed" || len(action.ExecutionID) != 32 {
			t.Errorf("truncation lost action information: %+v", output)
		}
		return finish("")
	})
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 4)
	var lost atomic.Int32
	if loseCompletion {
		r.hooks = &activityHooks{
			toolCall: func(ctx context.Context, in agent.ToolInput, next func(context.Context, agent.ToolInput) (string, error)) (string, error) {
				result, err := next(ctx, in)
				if err == nil && in.Call.Name == "computer_exec" {
					lost.Add(1)
					return "", errors.New("lost Activity completion after dispatch")
				}
				return result, err
			},
		}
	}
	if err := r.SignalSession(context.Background(), conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	if got := modelSteps.Load(); got != 3 {
		t.Fatalf("model took %d steps, want 3", got)
	}
	if loseCompletion && lost.Load() != 4 {
		t.Fatalf("Activity never exhausted its retries: %d attempts", lost.Load())
	}
}
