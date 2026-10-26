package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openalbert/internal/domain"
	"openalbert/internal/telemetry"
)

type computerSpy struct {
	calls  []domain.ComputerRequest
	result domain.ComputerResult
	err    error
}

func (s *computerSpy) Execute(_ context.Context, r domain.ComputerRequest) (domain.ComputerResult, error) {
	s.calls = append(s.calls, r)
	return s.result, s.err
}

func TestComputerAuthorizationAlsoPrecedesCheckpointReplay(t *testing.T) {
	a, _, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	spy := &computerSpy{result: domain.ComputerResult{State: "completed"}}
	a.Computer = spy
	args := json.RawMessage(`{"command":"pwd"}`)
	in := ToolInput{RunKey: "computer-access", Conversation: owner, Call: domain.ToolCall{ID: "exec", Name: "computer_exec", Arguments: args}}
	if out, err := a.ToolCall(context.Background(), in); err != nil || strings.Contains(out, `"error"`) {
		t.Fatal(out, err)
	}
	for _, tc := range []struct {
		conv       domain.Conversation
		initiative bool
	}{
		{domain.Conversation{ID: "group:7", ChatID: -7, OwnerID: 7}, false},
		{domain.Conversation{ID: "personal:8", Private: true, ChatID: 8, OwnerID: 8}, false},
		{domain.Conversation{ID: "spoof", Private: true, ChatID: 8, OwnerID: 7}, false},
		{owner, true},
	} {
		in.Conversation = tc.conv
		in.Background = tc.initiative
		out, err := a.ToolCall(context.Background(), in)
		if err != nil || !strings.Contains(out, `"error"`) || len(spy.calls) != 1 {
			t.Fatalf("cached privilege bypass: %s %v", out, err)
		}
		for _, def := range a.toolsForTurn(Turn{Conversation: tc.conv, Input: RunInput{Initiative: tc.initiative}}) {
			if strings.HasPrefix(def.Name, "computer_") {
				t.Fatal("unauthorized tool advertised")
			}
		}
	}
	a.Computer = nil
	for _, def := range a.toolsForTurn(Turn{Conversation: owner}) {
		if strings.HasPrefix(def.Name, "computer_") {
			t.Fatal("disabled computer advertised")
		}
	}
}

func TestComputerLostResponseRetainsExecutionAndScopedArtifacts(t *testing.T) {
	a, repo, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	spy := &computerSpy{err: errors.New("connection lost")}
	a.Computer = spy
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)
	out := tool(t, a, owner, "lost-run", "exec", "computer_exec", `{"command":"touch result","wait_seconds":0}`)
	var result struct {
		ExecutionID string `json:"execution_id"`
		State       string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &result) != nil || result.State != "unknown" || result.ExecutionID == "" {
		t.Fatal(out)
	}
	var event map[string]any
	if json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event) != nil || event["result"] != "error" || event["activity_result"] != "success" || event["execution_id"] != result.ExecutionID || strings.Contains(logs.String(), "touch result") {
		t.Fatalf("uncertain execution was misreported or command logged: %s", logs.String())
	}
	if spy.calls[0].WaitSeconds != 0 {
		t.Fatal("explicit zero wait lost")
	}
	tool(t, a, owner, "lost-run", "exec", "computer_exec", `{"command":"touch result","wait_seconds":0}`)
	if len(spy.calls) != 1 {
		t.Fatal("cached failure reran command")
	}
	spy.err = nil
	spy.result = domain.ComputerResult{State: "completed"}
	out = tool(t, a, owner, "status-run", "status", "computer_execution_status", `{"execution_id":"`+result.ExecutionID+`"}`)
	if strings.Contains(out, `"error"`) || spy.calls[1].ExecutionID != result.ExecutionID || spy.calls[1].Action != "status" {
		t.Fatal(out)
	}
	if out = tool(t, a, owner, "status-run", "foreign", "computer_execution_status", `{"execution_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`); !strings.Contains(out, `"error"`) {
		t.Fatal("unregistered job accessible")
	}
	ctx := context.Background()
	if err := repo.SaveArtifact(ctx, domain.Artifact{ID: "foreign", ConversationID: "other", Data: []byte("secret")}); err != nil {
		t.Fatal(err)
	}
	before := len(spy.calls)
	out = tool(t, a, owner, "files", "foreign", "computer_upload", `{"artifact_id":"foreign","path":"/workspace/in"}`)
	if !strings.Contains(out, `"error"`) || len(spy.calls) != before {
		t.Fatal("foreign artifact copied")
	}
	if err := repo.SaveArtifact(ctx, domain.Artifact{ID: "input", ConversationID: owner.ID, Data: []byte{0, 1, 255}}); err != nil {
		t.Fatal(err)
	}
	out = tool(t, a, owner, "files", "upload", "computer_upload", `{"artifact_id":"input","path":"/workspace/in"}`)
	if strings.Contains(out, `"error"`) || string(spy.calls[len(spy.calls)-1].Data) != string([]byte{0, 1, 255}) {
		t.Fatal(out)
	}
	spy.result = domain.ComputerResult{Data: []byte("result")}
	out = tool(t, a, owner, "files", "download", "computer_download", `{"path":"/workspace/report.txt"}`)
	var artResult struct {
		ArtifactID string `json:"artifact_id"`
	}
	_ = json.Unmarshal([]byte(out), &artResult)
	art, ok, err := repo.Artifact(ctx, owner.ID, artResult.ArtifactID)
	if err != nil || !ok || string(art.Data) != "result" || art.Name != "report.txt" {
		t.Fatalf("result not saved: %s %+v %v", out, art, err)
	}
	before = len(spy.calls)
	spy.result.Data = []byte("changed after the first save")
	// Replay after S3 committed but before the outer tool checkpoint committed.
	replayed, err := a.executeComputer(ctx, ToolInput{RunKey: "files", Conversation: owner, Call: domain.ToolCall{ID: "download", Name: "computer_download", Arguments: json.RawMessage(`{"path":"/workspace/report.txt"}`)}})
	encoded, _ := json.Marshal(replayed)
	if err != nil || len(spy.calls) != before || !strings.Contains(string(encoded), `"size":6`) {
		t.Fatalf("download replay changed immutable artifact: %s %v", encoded, err)
	}
}

func TestComputerValidationAndScheduledTaskState(t *testing.T) {
	a, repo, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	spy := &computerSpy{}
	a.Computer = spy
	for _, args := range []string{`null`, `{} {}`, `{"command":""}`, `{"command":"ls","cwd":"relative"}`, `{"command":"ls","timeout_seconds":0}`, `{"command":"ls","wait_seconds":6}`, `{"command":"ls","host":"other"}`} {
		_, err := a.executeComputer(context.Background(), ToolInput{Conversation: owner, Call: domain.ToolCall{Name: "computer_exec", Arguments: json.RawMessage(args)}})
		if err == nil || len(spy.calls) != 0 {
			t.Fatalf("invalid arguments accepted: %s", args)
		}
	}
	task := domain.Task{ID: "computer-task", ConversationID: owner.ID, Status: "cancelled"}
	created, err := repo.CreateTask(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	task = created
	in := ToolInput{Conversation: owner, TaskID: task.ID, Call: domain.ToolCall{Name: "computer_exec", Arguments: json.RawMessage(`{"command":"pwd"}`)}}
	if _, err := a.executeComputer(context.Background(), in); err == nil || len(spy.calls) != 0 {
		t.Fatal("cancelled task executed")
	}
	task.Status = "running"
	if _, err := repo.UpdateTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := a.executeComputer(context.Background(), in); err != nil || len(spy.calls) != 1 {
		t.Fatal("active owner task denied", err)
	}
	visible := 0
	for _, def := range a.toolsForTurn(Turn{Conversation: owner, Input: RunInput{TaskID: task.ID}}) {
		if strings.HasPrefix(def.Name, "computer_") {
			visible++
			if !json.Valid(def.Parameters) {
				t.Fatal("invalid schema")
			}
		}
	}
	if visible != 5 {
		t.Fatalf("scheduled tools=%d", visible)
	}
}
