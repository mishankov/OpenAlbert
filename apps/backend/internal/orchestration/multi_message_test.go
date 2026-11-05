package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"
)

func TestOneRequestSendsEveryPhotoAcrossModelSteps(t *testing.T) {
	for _, format := range []string{"send_message", "send_rich_message", "mixed"} {
		t.Run(format, func(t *testing.T) {
			a, repo, _, conv := newFixture(t)
			ctx := context.Background()
			for i := range 8 {
				if err := repo.SaveArtifact(ctx, domain.Artifact{ID: fmt.Sprint(i), ConversationID: conv.ID, Name: fmt.Sprintf("console-%d.png", i), MIME: "image/png", Data: []byte("synthetic photo")}); err != nil {
					t.Fatal(err)
				}
			}
			model := &scriptedModel{continueAfterReply: true, run: func(n int, _ []domain.Message) (domain.Message, error) {
				if n == 1 {
					// Sending a result must not stop subsequent research either.
					return calls(call("source", "web_read", map[string]string{"url": "https://example.com"})), nil
				}
				if n == 9 {
					return finish("Series complete; no extra confirmation needed."), nil
				}
				if n > 9 {
					return domain.Message{}, errors.New("model called after its final response")
				}
				i := n
				if n > 1 {
					i--
				}
				name := format
				if format == "mixed" {
					name = []string{"send_message", "send_rich_message"}[i%2]
				}
				return calls(call(fmt.Sprintf("part-%d", i), name, map[string]string{"text": fmt.Sprintf("%d/8 — console", i+1), "artifact_id": fmt.Sprint(i)})), nil
			}}
			a.Model = model
			env := newEnv(a)
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
			env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 12})
			requireWorkflowOK(t, env)
			rows, err := repo.PendingOutgoing(ctx, 20)
			if err != nil || len(rows) != 8 || model.count() != 10 {
				t.Fatalf("series stopped, repeated, or required another request: %d replies, %d model calls, %v", len(rows), model.count(), err)
			}
			for i, row := range rows {
				if row.ArtifactID != fmt.Sprint(i) || row.Text != fmt.Sprintf("%d/8 — console", i+1) || row.TurnID != rows[0].TurnID {
					t.Fatalf("series lost order or escaped its turn: %+v", rows)
				}
			}
			pending, err := repo.PendingIncoming(ctx, 10)
			if err != nil || len(pending) != 0 {
				t.Fatalf("completed request remained pending: %+v %v", pending, err)
			}
		})
	}
}

type replyCheckpointRetry struct {
	domain.Repository
	mu     sync.Mutex
	key    string
	failed bool
}

func (r *replyCheckpointRetry) SaveCheckpoint(ctx context.Context, key string, raw json.RawMessage) error {
	r.mu.Lock()
	fail := key == r.key && !r.failed
	if fail {
		r.failed = true
	}
	r.mu.Unlock()
	if fail {
		return errors.New("synthetic checkpoint interruption after enqueue")
	}
	return r.Repository.SaveCheckpoint(ctx, key, raw)
}

func TestMessageSeriesResumesToolRetryWithoutRepeatingQueuedParts(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	retrying := &replyCheckpointRetry{Repository: repo, key: "tool/" + agent.ID("series-retry", "0", "part-0")}
	a.Repo = retrying
	model := &scriptedModel{continueAfterReply: true, run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 2 {
			return finish("Done"), nil
		}
		return calls(call(fmt.Sprintf("part-%d", n), []string{"send_message", "send_rich_message"}[n], map[string]string{"text": fmt.Sprintf("Part %d", n)})), nil
	}}
	a.Model = model
	env := newEnv(a)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = options(ctx)
		turn, err := runTurn(ctx, agent.RunInput{Key: "series-retry", SessionID: "series-session", ConversationID: conv.ID, IncomingIDs: []string{"first"}}, 5)
		if err != nil {
			return err
		}
		return workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, nil)).Get(ctx, nil)
	})
	requireWorkflowOK(t, env)
	if !retrying.failed {
		t.Fatal("checkpoint interruption was not exercised")
	}
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 2 || rows[0].Text != "Part 0" || rows[1].Text != "Part 1" || model.count() != 3 {
		t.Fatalf("retry repeated a reply or skipped the remaining series: %+v %v", rows, err)
	}
}

func TestNewTurnInExistingSessionCanContinueAfterReply(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	_, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "second", Conversation: conv, Text: "Send both parts"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{continueAfterReply: true, run: func(n int, _ []domain.Message) (domain.Message, error) {
		if n == 3 {
			return finish("Done"), nil
		}
		return reply(fmt.Sprintf("Part %d", n)), nil
	}}
	env := newEnv(a)
	env.OnGetVersion("multi-message-turn/old", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion).Once()
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = options(ctx)
		for i, key := range []string{"old", "new"} {
			turn, err := runTurn(ctx, agent.RunInput{Key: key, SessionID: "same-session", ConversationID: conv.ID, IncomingIDs: []string{[]string{"first", "second"}[i]}}, 5)
			if err != nil {
				return err
			}
			if err := workflow.ExecuteActivity(ctx, "Finish", finishInput(turn, nil)).Get(ctx, nil); err != nil {
				return err
			}
		}
		return nil
	})
	requireWorkflowOK(t, env)
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 3 || rows[0].TurnID != "old" || rows[1].TurnID != "new" || rows[2].TurnID != "new" {
		t.Fatalf("legacy behavior leaked into a new turn: %+v %v", rows, err)
	}
}

func TestNewInputAfterReplyRetainsQueuedDeliveryState(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	_, err := repo.AcceptIncoming(context.Background(), domain.Incoming{ID: "second", Conversation: conv, Text: "No more photos"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{continueAfterReply: true, run: func(n int, messages []domain.Message) (domain.Message, error) {
		if n == 0 {
			return reply("First requested part"), nil
		}
		last := messages[len(messages)-1]
		if n != 1 || last.Role != "user" || last.Content != "No more photos" {
			return domain.Message{}, errors.New("new instruction was not applied before continuing")
		}
		return finish("Stop sending the remaining parts."), nil
	}}
	a.Model = model
	env := newEnv(a)
	steered := false
	env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
		if info.ActivityType.Name == "ToolCall" && err == nil && !steered {
			steered = true
			env.SignalWorkflow(MessageSignal, Event{ID: "second"})
		}
	})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(MessageSignal, Event{ID: "first"}) }, 0)
	env.ExecuteWorkflow(SessionWorkflow, SessionInput{ConversationID: conv.ID, IdleTimeout: time.Second, MaxSteps: 5})
	requireWorkflowOK(t, env)
	rows, err := repo.PendingOutgoing(context.Background(), 10)
	if err != nil || len(rows) != 1 || model.count() != 2 || !steered {
		t.Fatalf("new input failed to stop the series: %+v %v", rows, err)
	}
	raw, found, err := repo.LoadCheckpoint(context.Background(), "turn-done/"+rows[0].TurnID)
	var turn agent.Turn
	if err != nil || !found || json.Unmarshal(raw, &turn) != nil || !turn.Sent || turn.Stats.FinishReason != "reply_queued" || len(turn.Input.IncomingIDs) != 2 {
		t.Fatalf("steering forgot the delivered part or consumed request: %+v %v", turn, err)
	}
}
