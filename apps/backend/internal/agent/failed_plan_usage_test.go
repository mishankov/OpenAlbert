package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/integrations/siwc"
	"openalbert/apps/backend/internal/telemetry"
)

type failedUsagePlan struct{ *siwc.Client }

func (*failedUsagePlan) Complete(context.Context, []domain.Message, []domain.Tool) (domain.Completion, error) {
	return domain.Completion{
		Model: "requested-plan-model", InputTokens: 7, OutputTokens: 3,
		Usage: domain.ModelUsage{Model: "served-plan-model", TokenUsageKnown: true, CachedInputTokens: 4},
	}, errors.New("ChatGPT response incomplete")
}

func (*failedUsagePlan) Status() siwc.Status {
	return siwc.Status{Connected: true, PlanEnabled: true, Model: "requested-plan-model"}
}

func TestFailedPlanUsageReachesActivityTelemetryWithoutPartialCalls(t *testing.T) {
	for _, mode := range []string{"siwc", "auto"} {
		t.Run(mode, func(t *testing.T) {
			a, repo, _, conv := fixture(t)
			a.Model = &integrations.BillingModel{Mode: mode, Plan: &failedUsagePlan{}}
			var logs bytes.Buffer
			a.Logger = telemetry.New(&logs)
			out, err := a.ModelCall(context.Background(), ModelInput{Turn: Turn{Input: RunInput{Key: "failed-plan", SessionID: "session", ConversationID: conv.ID}}})
			if err == nil || out.InputTokens != 7 || out.OutputTokens != 3 || out.Model != "requested-plan-model" || out.Usage.Model != "served-plan-model" || !out.Usage.TokenUsageKnown || !reflect.DeepEqual(out.Message, domain.Message{}) {
				t.Fatalf("lost failed usage or exposed partial output: %+v %v", out, err)
			}
			if _, ok, err := repo.LoadCheckpoint(context.Background(), "model/failed-plan/0"); err != nil || ok {
				t.Fatalf("failed response checkpointed: %v %v", ok, err)
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &event); err != nil {
				t.Fatalf("invalid model event: %s: %v", logs.String(), err)
			}
			if event["operation"] != "llm.complete" || event["result"] != "error" || event["input_tokens"] != float64(7) || event["output_tokens"] != float64(3) || event["cached_input_tokens"] != float64(4) || event["token_usage_known"] != true || event["model"] != "served-plan-model" || event["tool_call_count"] != float64(0) {
				t.Fatalf("failed usage missing from telemetry: %v", event)
			}
		})
	}
}
