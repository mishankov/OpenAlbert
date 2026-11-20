package agent

import (
	"context"

	"go.temporal.io/sdk/temporal"
)

// AnalysisContextLimitErrorType is also the durable session failure code.
const AnalysisContextLimitErrorType = "context_limit"

func (a *Activities) finishAnalysisContextFailure(ctx context.Context, sessionID string) error {
	// Keep storage failures retryable. The saved analysis receipt prevents a retry
	// from calling the model or sizing the same transcript again.
	if err := a.Repo.MarkAnalysisFailed(ctx, sessionID, AnalysisContextLimitErrorType); err != nil {
		return err
	}
	return temporal.NewNonRetryableApplicationError(ErrAnalysisContextLimit.Error(), AnalysisContextLimitErrorType, ErrAnalysisContextLimit)
}
