package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
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
			clk := NewFakeClock(time.Now().UTC())
			r := newTestRuntime(a, clk, time.Second, 12)
			if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
				t.Fatal(err)
			}
			waitWithFakeClock(t, r, clk)
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 5)
	var runErr error
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx := context.Background()
		turn, err := r.runTurn(ctx, agent.RunInput{Key: "series-retry", SessionID: "series-session", ConversationID: conv.ID, IncomingIDs: []string{"first"}}, 5)
		if err != nil {
			runErr = err
			return
		}
		runErr = r.actFinish(ctx, finishInput(turn, nil))
	}()
	waitWithFakeClock(t, r, clk)
	if runErr != nil {
		t.Fatal(runErr)
	}
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
	ctx := context.Background()
	_, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "second", Conversation: conv, Text: "Send both parts"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	a.Model = &scriptedModel{continueAfterReply: true, run: func(n int, _ []domain.Message) (domain.Message, error) {
		switch n {
		case 0:
			return reply("Part 0"), nil
		case 1:
			return finish("First done"), nil
		case 2:
			return reply("Part 1"), nil
		case 3:
			return reply("Part 2"), nil
		default:
			return finish("Done"), nil
		}
	}}
	r := newTestRuntime(a, nil, time.Second, 5)
	for i, key := range []string{"first-turn", "second-turn"} {
		turn, err := r.runTurn(ctx, agent.RunInput{Key: key, SessionID: "same-session", ConversationID: conv.ID, IncomingIDs: []string{[]string{"first", "second"}[i]}}, 5)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.actFinish(ctx, finishInput(turn, nil)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(rows) != 3 || rows[0].TurnID != "first-turn" || rows[1].TurnID != "second-turn" || rows[2].TurnID != "second-turn" {
		t.Fatalf("multi-message turn failed: %+v %v", rows, err)
	}
}

func TestNewInputAfterReplyRetainsQueuedDeliveryState(t *testing.T) {
	a, repo, _, conv := newFixture(t)
	ctx := context.Background()
	_, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "second", Conversation: conv, Text: "No more photos"}, 2)
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
	clk := NewFakeClock(time.Now().UTC())
	r := newTestRuntime(a, clk, time.Second, 5)
	var steered atomic.Bool
	r.hooks = &activityHooks{
		onComplete: func(name string, _ any, err error) {
			if name == "ToolCall" && err == nil && steered.CompareAndSwap(false, true) {
				_ = r.SignalSession(ctx, conv.ID, Event{ID: "second"})
			}
		},
	}
	if err := r.SignalSession(ctx, conv.ID, Event{ID: "first"}); err != nil {
		t.Fatal(err)
	}
	waitWithFakeClock(t, r, clk)
	rows, err := repo.PendingOutgoing(ctx, 10)
	if err != nil || len(rows) != 1 || model.count() != 2 || !steered.Load() {
		t.Fatalf("new input failed to stop the series: %+v %v", rows, err)
	}
	raw, found, err := repo.LoadCheckpoint(ctx, "turn-done/"+rows[0].TurnID)
	var turn agent.Turn
	if err != nil || !found || json.Unmarshal(raw, &turn) != nil || !turn.Sent || turn.Stats.FinishReason != "reply_queued" || len(turn.Input.IncomingIDs) != 2 {
		t.Fatalf("steering forgot the delivered part or consumed request: %+v %v", turn, err)
	}
}
