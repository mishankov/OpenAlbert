package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/domain"
)

func TestAnalysisWorkflowContextFailureStopsWorkflowRetries(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(quietLogger{})
	env := suite.NewTestWorkflowEnvironment()
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumAttempts: 3}})
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, domain.Session) error {
		calls++
		return temporal.NewNonRetryableApplicationError("oversized", agent.AnalysisContextLimitErrorType, nil)
	}, activity.RegisterOptions{Name: "Analyze"})
	env.ExecuteWorkflow(AnalysisWorkflow, domain.Session{ID: "oversized"})
	var workflowErr *temporal.WorkflowExecutionError
	if !errors.As(env.GetWorkflowError(), &workflowErr) {
		t.Fatalf("missing workflow failure: %v", env.GetWorkflowError())
	}
	// Require the workflow's direct cause to be nonretryable. Finding the nested
	// activity application error alone would miss an endlessly retried workflow.
	failure, ok := workflowErr.Unwrap().(*temporal.ApplicationError)
	if !ok || !failure.NonRetryable() || failure.Type() != agent.AnalysisContextLimitErrorType || calls != 1 {
		t.Fatalf("terminal failure retried: cause=%T %v calls=%d", workflowErr.Unwrap(), workflowErr.Unwrap(), calls)
	}
}

func TestAnalysisWorkflowTemporaryFailureRetriesActivity(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(quietLogger{})
	env := suite.NewTestWorkflowEnvironment()
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, domain.Session) error {
		calls++
		if calls == 1 {
			return errors.New("temporary provider error")
		}
		return nil
	}, activity.RegisterOptions{Name: "Analyze"})
	env.ExecuteWorkflow(AnalysisWorkflow, domain.Session{ID: "retryable"})
	if err := env.GetWorkflowError(); err != nil || calls != 2 {
		t.Fatalf("temporary failure did not recover: calls=%d err=%v", calls, err)
	}
}
