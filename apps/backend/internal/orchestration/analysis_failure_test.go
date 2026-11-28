package orchestration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/storage"
)

func TestAnalysisWorkflowContextFailureStopsWorkflowRetries(t *testing.T) {
	repo := storage.NewMemory()
	ctx := context.Background()
	if err := repo.StartSession(ctx, domain.Session{ID: "oversized", ConversationID: "personal:7"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.EndSession(ctx, "oversized", ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	clk := NewFakeClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	r := &Runtime{
		Repo:       repo,
		Activities: &agent.Activities{Repo: repo},
		Clock:      clk,
		hooks: &activityHooks{
			analyze: func(context.Context, domain.Session, func(context.Context, domain.Session) error) error {
				calls.Add(1)
				return domain.NewNonRetryableError("oversized", agent.AnalysisContextLimitErrorType, nil)
			},
		},
	}

	err := r.RunAnalysis(ctx, domain.Session{ID: "oversized"})
	if !domain.IsNonRetryable(err) || domain.ErrorCode(err) != agent.AnalysisContextLimitErrorType || calls.Load() != 1 {
		t.Fatalf("terminal failure retried: err=%v calls=%d", err, calls.Load())
	}

	// Dispatcher must also mark it terminal and never retry it even after hours.
	calls.Store(0)
	if err := r.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	clk.Advance(2 * time.Hour)
	if err := r.DispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if calls.Load() != 1 {
		t.Fatalf("dispatcher retried terminal analysis failure: calls=%d", calls.Load())
	}
}

func TestAnalysisWorkflowTemporaryFailureRetriesActivity(t *testing.T) {
	var calls atomic.Int32
	clk := NewFakeClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	r := &Runtime{
		Activities: &agent.Activities{},
		Clock:      clk,
		hooks: &activityHooks{
			analyze: func(context.Context, domain.Session, func(context.Context, domain.Session) error) error {
				if calls.Add(1) == 1 {
					return errors.New("temporary provider error")
				}
				return nil
			},
		},
	}
	var runErr error
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		runErr = r.RunAnalysis(context.Background(), domain.Session{ID: "retryable"})
	}()
	waitWithFakeClock(t, r, clk)
	if runErr != nil || calls.Load() != 2 {
		t.Fatalf("temporary failure did not recover: calls=%d err=%v", calls.Load(), runErr)
	}
}
