package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

type analysisBudgetModel struct {
	*testModel
	fitCalls int
	fitError error
}

func (m *analysisBudgetModel) FitsContext(_ context.Context, messages []domain.Message, _ []domain.Tool) (bool, error) {
	m.fitCalls++
	return !strings.Contains(messages[1].Content, "oversized"), m.fitError
}

func requireAnalysisContextFailure(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrAnalysisContextLimit) || !domain.IsNonRetryable(err) || domain.ErrorCode(err) != AnalysisContextLimitErrorType {
		t.Fatalf("expected terminal context failure: %v", err)
	}
}

func TestAnalysisContextFailuresDoNotStarveLaterSession(t *testing.T) {
	a, repo, k, conv := fixture(t)
	ctx := context.Background()
	m := &analysisBudgetModel{testModel: &testModel{respond: func([]domain.Message) (domain.Message, error) {
		return domain.Message{Content: `{"summary":"fitting session","memories":[]}`}, nil
	}}}
	a.Model = m
	for i := range 51 {
		session := domain.Session{ID: fmt.Sprintf("analysis-%02d", i), ConversationID: conv.ID}
		if err := repo.StartSession(ctx, session); err != nil {
			t.Fatal(err)
		}
		content := "oversized newest record"
		if i == 50 {
			content = "fits"
		}
		if err := repo.AppendMessage(ctx, session.ID, "message", domain.Message{Role: "user", Content: content}); err != nil {
			t.Fatal(err)
		}
		if err := repo.EndSession(ctx, session.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce the dispatcher's oldest-50 batches. The next poll must reach the
	// fitting session after the first fifty receive explicit failed outcomes.
	for batch := range 2 {
		pending, err := repo.PendingAnalysis(ctx, 50)
		expected := 50
		if batch == 1 {
			expected = 1
		}
		if err != nil || len(pending) != expected {
			t.Fatalf("batch %d: pending=%d err=%v", batch, len(pending), err)
		}
		for _, session := range pending {
			err := a.Analyze(ctx, session)
			if batch == 0 {
				requireAnalysisContextFailure(t, err)
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}
	if pending, err := repo.PendingAnalysis(ctx, 50); err != nil || len(pending) != 0 || m.calls != 1 {
		t.Fatalf("queue did not drain: pending=%d provider calls=%d err=%v", len(pending), m.calls, err)
	}
	for i := range 50 {
		id := fmt.Sprintf("analysis-%02d", i)
		raw, ok, err := repo.LoadCheckpoint(ctx, "analysis/"+id)
		var result analysisResult
		if err != nil || !ok || json.Unmarshal(raw, &result) != nil || result.Failure != AnalysisContextLimitErrorType || result.Summary != "" || len(result.Memories) != 0 {
			t.Fatalf("missing durable failed receipt: %s %v", raw, err)
		}
		if _, applied, err := repo.LoadCheckpoint(ctx, "analysis-applied/"+id); err != nil || applied {
			t.Fatalf("failed analysis applied partial result: %s %v", id, err)
		}
	}
	if entries, err := k.Search(ctx, conv.ID, ""); err != nil || len(entries) != 0 {
		t.Fatalf("failed analysis wrote memories: %+v %v", entries, err)
	}
}

type failAnalysisOutcome struct {
	domain.Repository
	fail bool
}

func (r *failAnalysisOutcome) MarkAnalysisFailed(ctx context.Context, id, code string) error {
	if r.fail {
		r.fail = false
		return errors.New("temporary storage error")
	}
	return r.Repository.MarkAnalysisFailed(ctx, id, code)
}

func TestAnalysisFailureReceiptRetriesOnlyOutcomeWrite(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	session := closedAnalysisSession(t, a, repo, conv.ID, domain.Message{Role: "user", Content: "oversized"})
	m := &analysisBudgetModel{testModel: &testModel{respond: func([]domain.Message) (domain.Message, error) {
		t.Fatal("oversized analysis called provider")
		return domain.Message{}, nil
	}}}
	a.Model = m
	a.Repo = &failAnalysisOutcome{Repository: repo, fail: true}
	if err := a.Analyze(ctx, session); err == nil || errors.Is(err, ErrAnalysisContextLimit) {
		t.Fatalf("storage error must remain retryable: %v", err)
	}
	if pending, _ := repo.PendingAnalysis(ctx, 50); len(pending) != 1 {
		t.Fatal("failed outcome write prematurely removed session")
	}
	for range 2 {
		requireAnalysisContextFailure(t, a.Analyze(ctx, session))
	}
	if m.fitCalls != 1 || m.calls != 0 {
		t.Fatalf("failed receipt replay repeated model work: sizes=%d calls=%d", m.fitCalls, m.calls)
	}
	if pending, _ := repo.PendingAnalysis(ctx, 50); len(pending) != 0 {
		t.Fatal("failed outcome write did not recover")
	}
}

func TestAnalysisTemporaryAndMalformedFailuresRemainPending(t *testing.T) {
	for _, mode := range []string{"sizing", "provider", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			a, repo, k, conv := fixture(t)
			ctx := context.Background()
			session := closedAnalysisSession(t, a, repo, conv.ID, domain.Message{Role: "user", Content: "fits"})
			recovered := false
			m := &analysisBudgetModel{testModel: &testModel{respond: func([]domain.Message) (domain.Message, error) {
				if !recovered {
					if mode == "provider" {
						return domain.Message{}, errors.New("temporary provider failure")
					}
					return domain.Message{Content: "invalid JSON"}, nil
				}
				// The model cannot forge an internal failure receipt.
				return domain.Message{Content: `{"summary":"recovered","memories":[],"_failure":"context_limit"}`}, nil
			}}}
			if mode == "sizing" {
				m.fitError = errors.New("temporary profile lookup failure")
			}
			a.Model = m
			err := a.Analyze(ctx, session)
			if err == nil || errors.Is(err, ErrAnalysisContextLimit) || domain.IsNonRetryable(err) {
				t.Fatalf("temporary failure marked terminal: %v", err)
			}
			if pending, _ := repo.PendingAnalysis(ctx, 50); len(pending) != 1 {
				t.Fatal("temporary failure removed pending session")
			}
			if _, ok, err := repo.LoadCheckpoint(ctx, "analysis/"+session.ID); err != nil || ok {
				t.Fatalf("temporary failure saved completion receipt: %v", err)
			}
			if entries, err := k.Search(ctx, conv.ID, ""); err != nil || len(entries) != 0 {
				t.Fatal("temporary failure wrote memories")
			}
			recovered, m.fitError = true, nil
			if err := a.Analyze(ctx, session); err != nil {
				t.Fatal(err)
			}
			if pending, _ := repo.PendingAnalysis(ctx, 50); len(pending) != 0 {
				t.Fatal("retry did not recover")
			}
		})
	}
}
