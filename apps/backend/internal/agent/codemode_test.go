package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

func TestCodemodeRejectsOversizedEncodedBatch(t *testing.T) {
	a, _, _, conv := fixture(t)
	args, err := json.Marshal(map[string]string{
		"code": `await Promise.all(Array.from({length: 4}, () => tools.artifact_save({name: "sample", content: "<".repeat(90000)})))`,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.CodemodeStep(context.Background(), CodemodeInput{ToolInput: ToolInput{
		RunKey: "large-batch", Conversation: conv, SessionID: "large-batch",
		Call: domain.ToolCall{ID: "script", Name: CodemodeName, Arguments: args},
	}})
	if err != nil || len(result.Pending) != 0 || !strings.Contains(result.Error, "batch is too large") {
		t.Fatalf("oversized request batch was accepted: %d requests, %s, %v", len(result.Pending), result.Error, err)
	}
}

type lostCodemodeCheckpoint struct {
	domain.Repository
	key  string
	lost bool
}

func (r *lostCodemodeCheckpoint) SaveCheckpoint(ctx context.Context, key string, raw json.RawMessage) error {
	if key == r.key && !r.lost {
		r.lost = true
		return errors.New("interrupted before tool receipt")
	}
	return r.Repository.SaveCheckpoint(ctx, key, raw)
}

func TestCodemodeRecoversDownloadAfterLostReceipt(t *testing.T) {
	a, repo, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	computer := &computerSpy{result: domain.ComputerResult{Data: []byte("original")}}
	a.Computer = computer
	a.Repo = &lostCodemodeCheckpoint{Repository: repo, key: "tool/" + ID("download-run", "0", "download")}
	in := ToolInput{RunKey: "download-run", Conversation: owner, ParentToolCallID: "script", Call: domain.ToolCall{
		ID: "download", Name: "computer_download", Arguments: json.RawMessage(`{"path":"/workspace/report.txt"}`),
	}}
	ctx := context.Background()
	if _, err := a.ToolCall(ctx, in); err == nil {
		t.Fatal("lost tool receipt was not exercised")
	}
	computer.result.Data = []byte("changed")
	result, err := a.ToolCall(ctx, in)
	artifactID := ID("computer-artifact", "download-run", "0", "download")
	if err != nil || len(computer.calls) != 1 || !strings.Contains(result, artifactID) || !strings.Contains(result, `"size":8`) {
		t.Fatalf("retry failed to recover original artifact: %s, %v", result, err)
	}
}

func TestCodemodeExecRetryReadsSQLiteRegistration(t *testing.T) {
	ctx := context.Background()
	repo, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	a, _, _, owner := fixture(t)
	runKey := ID(t.Name(), time.Now().String())
	computer := &computerSpy{result: domain.ComputerResult{State: "completed"}}
	a.OwnerUserID, a.Computer = owner.OwnerID, computer
	a.Repo = &lostCodemodeCheckpoint{Repository: repo, key: "tool/" + ID(runKey, "0", "exec")}
	in := ToolInput{RunKey: runKey, Conversation: owner, ParentToolCallID: "script", Call: domain.ToolCall{
		ID: "exec", Name: "computer_exec", Arguments: json.RawMessage(`{"command":"pwd","wait_seconds":0}`),
	}}
	if _, err := a.ToolCall(ctx, in); err == nil || len(computer.calls) != 1 {
		t.Fatalf("expected a dispatched command with a lost receipt: %v", err)
	}
	result, err := a.ToolCall(ctx, in)
	id := ComputerExecutionID(in)
	if err != nil || !strings.Contains(result, id) || !strings.Contains(result, `"state":"completed"`) ||
		len(computer.calls) != 2 || computer.calls[0].ExecutionID != id || computer.calls[1].ExecutionID != id {
		t.Fatalf("retry failed to reuse the SQLite execution registration: %s, %v", result, err)
	}
}
