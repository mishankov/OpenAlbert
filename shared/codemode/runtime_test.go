package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunBatchesToolsAndReplaysResults(t *testing.T) {
	in := Input{
		Code: `
const [first, second] = await Promise.all([
  tools.read({item: "one", order: 1}),
  tools.read({item: "two"}),
]);
const prior = load("count");
store("count", prior + 1);
const result = await tools.write({text: first + second, prior});
return {result, schemas: searchTools("read").length, parameters: describeTool("read").parameters.type};
`,
		Tools: []Tool{
			{Name: "read", Description: "Read a record", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "write", Description: "Save a record", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		State: map[string]json.RawMessage{"count": json.RawMessage(`2`)},
	}
	first := runTestScript(t, in)
	if len(first.Pending) != 2 || first.Pending[0].Name != "read" || string(first.Pending[1].Arguments) != `{"item":"two"}` {
		t.Fatalf("unexpected first batch: %+v", first)
	}
	reordered := Request{Name: "read", Arguments: json.RawMessage(`{"order":1,"item":"one"}`)}
	in.History = [][]Receipt{{
		{Request: reordered, Result: json.RawMessage(`"first "`)},
		{Request: first.Pending[1], Result: json.RawMessage(`"second"`)},
	}}
	next := runTestScript(t, in)
	if len(next.Pending) != 1 || next.Pending[0].Name != "write" || string(next.Pending[0].Arguments) != `{"text":"first second","prior":2}` || next.State != nil {
		t.Fatalf("unexpected write batch or premature store commit: %+v", next)
	}
	in.History = append(in.History, []Receipt{{Request: next.Pending[0], Result: json.RawMessage(`"saved"`)}})
	final := runTestScript(t, in)
	if string(final.Output) != `{"result":"saved","schemas":1,"parameters":"object"}` || string(final.State["count"]) != "3" || len(final.Pending) != 0 {
		t.Fatalf("unexpected final result: %+v", final)
	}
	in.History[0][0].Request.Arguments = json.RawMessage(`{"order":2,"item":"one"}`)
	diverged, err := Run(context.Background(), in)
	if err != nil || !strings.Contains(diverged.Error, "different tool request") || len(diverged.Pending) != 0 {
		t.Fatalf("replay did not reject the different request: %+v %v", diverged, err)
	}
}

func TestRunSandboxAndDeterminism(t *testing.T) {
	in := Input{
		Code: `
const receipt = await tools.read({ time: Date.now(), random: Math.random() });
let moduleBlocked = false;
try { await import("std"); } catch { moduleBlocked = true; }
return {
  receipt, moduleBlocked, hiddenTool: typeof tools.private,
  fetch: typeof fetch, node: typeof process, os: typeof os,
  timer: typeof setTimeout, bridge: typeof globalThis.__openalbertEnqueue,
};
`,
		Tools:     []Tool{{Name: "read"}},
		StartedAt: time.Unix(1234, 567000000),
	}
	first := runTestScript(t, in)
	if len(first.Pending) != 1 || !strings.Contains(string(first.Pending[0].Arguments), `"time":1234567`) {
		t.Fatalf("unexpected fixed clock: %+v", first)
	}
	in.History = [][]Receipt{{{Request: first.Pending[0], Result: json.RawMessage(`true`)}}}
	final := runTestScript(t, in)
	const expected = `{"receipt":true,"moduleBlocked":true,"hiddenTool":"undefined","fetch":"undefined","node":"undefined","os":"undefined","timer":"undefined","bridge":"undefined"}`
	if string(final.Output) != expected {
		t.Fatalf("unexpected sandbox output: %s", final.Output)
	}
}

func TestRunToolErrorsAndPartialOutput(t *testing.T) {
	in := Input{
		Code:  `store("key", true); try { await tools.read({}); } catch (error) { text(error.code); } throw new Error("stopped");`,
		Tools: []Tool{{Name: "read"}},
	}
	first := runTestScript(t, in)
	in.History = [][]Receipt{{{
		Request: first.Pending[0],
		Result:  json.RawMessage(`{"error":"missing","error_code":"not_found"}`),
		Failed:  true,
	}}}
	result, err := Run(context.Background(), in)
	if err != nil || string(result.Output) != `"not_found"` || result.Error != "stopped" || result.State != nil || len(result.Pending) != 0 {
		t.Fatalf("unexpected result after rejected tool: %+v %v", result, err)
	}
}

func TestRunLimits(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{name: "deadline", code: `for (;;) {}`, want: "deadline"},
		{name: "memory", code: `return new Uint8Array(40 << 20).length`, want: "out of memory"},
		{name: "output", code: `return "x".repeat(33000)`, want: "output limit"},
		{name: "storage", code: `store("large", "x".repeat(17000))`, want: "store limit"},
		{name: "calls", code: `await Promise.all(Array.from({ length: 65 }, () => tools.read({})))`, want: "tool call limit"},
		{name: "arguments", code: `await tools.read({ input: "x".repeat(110000) })`, want: "size limit"},
		{name: "quiescence", code: `await new Promise(() => {})`, want: "without a pending tool call"},
		{name: "empty error", code: `throw ""`, want: "script failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Run(context.Background(), Input{Code: test.code, Tools: []Tool{{Name: "read"}}})
			if err != nil || !strings.Contains(result.Error, test.want) || len(result.Pending) != 0 {
				t.Fatalf("limit %q was not enforced: %+v %v", test.want, result, err)
			}
		})
	}
}

func TestRunDropsPendingToolsWhenScriptFinishes(t *testing.T) {
	result := runTestScript(t, Input{
		Code:  `tools.write({text: "discard this"}); return "complete";`,
		Tools: []Tool{{Name: "write"}},
	})
	if len(result.Pending) != 0 || string(result.Output) != `"complete"` {
		t.Fatalf("unexpected result with a detached tool request: %+v", result)
	}
}

func TestRunLocalNamesCanShadowHelpers(t *testing.T) {
	result := runTestScript(t, Input{Code: `
const text = "local variable";
const tools = 2;
globalThis.text(text);
return tools;
`})
	if string(result.Output) != `["local variable",2]` {
		t.Fatalf("unexpected result with shadowed helpers: %+v", result)
	}
}

func TestRunLiveMultiBatchCustomLimitsAndSlowToolPause(t *testing.T) {
	batches := 0
	res, err := RunLive(context.Background(), LiveInput{
		Code: `
const [a, b] = await Promise.all([
  tools.read({name: "a", pad: "x".repeat(120000)}),
  tools.read({name: "b"}),
]);
store("seen", a + b);
const saved = await tools.write({value: load("seen")});
return {saved, big: "y".repeat(40000).length};
`,
		Tools: []Tool{
			{Name: "read", Description: "Read file", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Name: "write", Description: "Write file", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		Limits: Limits{
			MaxArgumentsBytes: 256 << 10,
			MaxOutputBytes:    128 << 10,
			MaxStateBytes:     64 << 10,
			StepTimeout:       150 * time.Millisecond,
		},
	}, func(ctx context.Context, reqs []Request) ([]Receipt, error) {
		batches++
		// Sleep longer than StepTimeout to prove JS CPU timeout pauses during Go tool execution.
		time.Sleep(220 * time.Millisecond)
		receipts := make([]Receipt, len(reqs))
		for i, r := range reqs {
			if r.Name == "read" {
				var p struct{ Name string }
				_ = json.Unmarshal(r.Arguments, &p)
				raw, _ := json.Marshal(p.Name)
				receipts[i] = Receipt{Request: r, Result: raw}
			} else {
				receipts[i] = Receipt{Request: r, Result: json.RawMessage(`"ok"`)}
			}
		}
		return receipts, nil
	})
	if err != nil || res.Error != "" {
		t.Fatalf("RunLive failed: %+v %v", res, err)
	}
	if batches != 2 {
		t.Fatalf("expected 2 batches, got %d", batches)
	}
	if string(res.Output) != `{"saved":"ok","big":40000}` || string(res.State["seen"]) != `"ab"` {
		t.Fatalf("unexpected RunLive output/state: %+v", res)
	}
}

func TestRunLiveCancellationAndStepDeadline(t *testing.T) {
	t.Run("step deadline", func(t *testing.T) {
		res, err := RunLive(context.Background(), LiveInput{
			Code:   `for (;;) {}`,
			Limits: Limits{StepTimeout: 100 * time.Millisecond},
		}, nil)
		if err != nil || !strings.Contains(res.Error, "deadline") {
			t.Fatalf("expected step deadline error, got %+v %v", res, err)
		}
	})
	t.Run("context cancel during tool", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := RunLive(ctx, LiveInput{
			Code:  `return await tools.slow({});`,
			Tools: []Tool{{Name: "slow"}},
		}, func(ctx context.Context, reqs []Request) ([]Receipt, error) {
			cancel()
			return nil, ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})
	t.Run("large output within rebuilt bridge limit", func(t *testing.T) {
		res, err := RunLive(context.Background(), LiveInput{
			Code: `return "A".repeat(70000);`,
			Limits: Limits{
				MaxOutputBytes:  128 << 10,
				MaxOutputBlocks: 256,
				MaxStateBytes:   64 << 10,
			},
		}, nil)
		if err != nil || res.Error != "" {
			t.Fatalf("expected 70k output to succeed, got %+v %v", res, err)
		}
		var got string
		if err := json.Unmarshal(res.Output, &got); err != nil || len(got) != 70000 {
			t.Fatalf("expected 70000-char string, got len=%d err=%v", len(got), err)
		}
	})
	t.Run("legacy Run preserves 128 output block limit", func(t *testing.T) {
		res, err := Run(context.Background(), Input{
			Code: `for (let i = 0; i < 129; i++) { text("x"); }`,
		})
		if err != nil || !strings.Contains(res.Error, "output limit") {
			t.Fatalf("expected legacy Run to reject 129 text() blocks, got %+v %v", res, err)
		}
	})
}

func runTestScript(t *testing.T, in Input) Result {
	t.Helper()
	result, err := Run(context.Background(), in)
	if err != nil || result.Error != "" {
		t.Fatalf("script failed: %+v %v", result, err)
	}
	return result
}
