package agent

import (
	"errors"

	"openalbert/apps/backend/internal/domain"
)

// Invalid provider requests need a code/configuration change before another try.
// Translate at the execution boundary so retries cannot resubmit the same body.
func modelCallError(err error) error {
	var permanent interface{ NonRetryable() bool }
	if errors.As(err, &permanent) && permanent.NonRetryable() {
		return domain.NewNonRetryableError(err.Error(), "ModelRequestInvalid", err)
	}
	return err
}
