package agent

import (
	"errors"
	"go.temporal.io/sdk/temporal"
)

// Invalid provider requests need a code/configuration change before another try.
// Translate at the activity boundary so retries cannot resubmit the same body.
func modelCallError(err error) error {
	var permanent interface{ NonRetryable() bool }
	if errors.As(err, &permanent) && permanent.NonRetryable() {
		return temporal.NewNonRetryableApplicationError(err.Error(), "ModelRequestInvalid", err)
	}
	return err
}
