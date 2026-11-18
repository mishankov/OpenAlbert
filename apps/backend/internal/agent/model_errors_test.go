package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/temporal"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

func TestUnsupportedCapabilityIsNonRetryableAtModelActivityBoundaries(t *testing.T) {
	for _, code := range []string{"subscription_sharing_unsupported_capability", "subscription_sharing_usage_unavailable"} {
		a, _, _, conv := fixture(t)
		a.Model = &testModel{respond: func([]domain.Message) (domain.Message, error) {
			return domain.Message{}, &siwc.APIError{Status: 400, Code: code, Body: []byte(`{"message":"PRIVATE_BODY"}`)}
		}}
		_, err := a.ModelCall(context.Background(), ModelInput{Turn: Turn{Input: RunInput{Key: code, ConversationID: conv.ID}}})
		var application *temporal.ApplicationError
		if code == "subscription_sharing_unsupported_capability" {
			if !errors.As(err, &application) || !application.NonRetryable() {
				t.Fatal("Temporal will retry an invalid request")
			}
			_, err = a.analysisModelCall(context.Background(), domain.Session{ID: "s", ConversationID: conv.ID}, nil)
			if !errors.As(err, &application) || !application.NonRetryable() {
				t.Fatal("analysis will retry an invalid request")
			}
		} else if errors.As(err, &application) && application.NonRetryable() {
			t.Fatal("transient error became permanent")
		}
		if err == nil || strings.Contains(err.Error(), "PRIVATE_BODY") {
			t.Fatal("lost error or leaked body")
		}
	}
}
