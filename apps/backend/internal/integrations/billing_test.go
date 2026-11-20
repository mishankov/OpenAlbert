package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

type billingStub struct {
	*siwc.Client
	calls      atomic.Int32
	model      string
	err        error
	completion *domain.Completion
}

func (m *billingStub) Complete(context.Context, []domain.Message, []domain.Tool) (domain.Completion, error) {
	m.calls.Add(1)
	if m.completion != nil {
		return *m.completion, m.err
	}
	return domain.Completion{Model: m.model, Message: domain.Message{Role: "assistant", Content: "ok"}}, m.err
}
func (m *billingStub) Status() siwc.Status {
	return siwc.Status{Connected: true, PlanEnabled: true, Model: "test"}
}
func TestBillingFallbackCooldownAndRecovery(t *testing.T) {
	for _, code := range []string{"subscription_sharing_usage_limit_exceeded", "subscription_sharing_usage_unavailable", "subscription_sharing_user_not_eligible", ""} {
		t.Run(code, func(t *testing.T) {
			plan := &billingStub{model: "plan-model", err: &siwc.APIError{Status: 429, Code: code, RequestID: "request-1"}}
			api := &billingStub{model: "api-model"}
			b := &BillingModel{Mode: "auto", Plan: plan, API: api, Cooldown: time.Hour}
			for range 3 {
				if out, err := b.Complete(context.Background(), nil, nil); err != nil || out.Model != api.model {
					t.Fatalf("fallback completion: %+v %v", out, err)
				}
			}
			s := b.Status()
			if plan.calls.Load() != 1 || api.calls.Load() != 3 || s.ActiveLane != "api_key" || s.Last429 == nil || s.LastRequestID != "request-1" || s.SIWCRequests != 1 || s.APIRequests != 3 {
				t.Fatalf("wrong circuit/status: %+v", s)
			}
			if code == "subscription_sharing_user_not_eligible" && (s.RetryAt != nil || !s.PlanIneligible) {
				t.Fatal("ineligible account advertised a timed retry")
			}
			b.retryAt = time.Now().Add(-time.Second)
			plan.err = nil
			_, err := b.Complete(context.Background(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if code == "subscription_sharing_user_not_eligible" {
				if plan.calls.Load() != 1 {
					t.Fatal("retried ineligible plan")
				}
				b.Reconnected()
				if b.Status().PlanIneligible {
					t.Fatal("reconnection did not clear eligibility block")
				}
				_, _ = b.Complete(context.Background(), nil, nil)
			}
			if plan.calls.Load() != 2 {
				t.Fatal("did not resume plan")
			}
		})
	}
}
func TestBillingDoesNotChangePaymentForInvalidRequests(t *testing.T) {
	for _, err := range []error{&siwc.APIError{Status: 400, Code: "subscription_sharing_unsupported_capability"}, &siwc.APIError{Status: 403, Code: "subscription_sharing_route_not_supported"}, &siwc.APIError{Status: 403, Code: "chatpass_v2_scope_not_authorized"}, errors.New("stream interrupted")} {
		plan := &billingStub{err: err}
		api := &billingStub{}
		b := &BillingModel{Mode: "auto", Plan: plan, API: api}
		if _, e := b.Complete(context.Background(), nil, nil); e == nil || api.calls.Load() != 0 {
			t.Fatal("silently changed billing for invalid request")
		}
	}
	plan := &billingStub{err: &siwc.APIError{Status: 429}}
	api := &billingStub{}
	b := &BillingModel{Mode: "siwc", Plan: plan, API: api, Cooldown: time.Hour}
	for range 2 {
		if _, err := b.Complete(context.Background(), nil, nil); err == nil {
			t.Fatal("unexpected success")
		}
	}
	if plan.calls.Load() != 1 || api.calls.Load() != 0 {
		t.Fatal("plan-only mode did not pause or billed API")
	}
	b = &BillingModel{Mode: "api_key", API: api}
	if _, err := b.Complete(context.Background(), nil, nil); err != nil || api.calls.Load() != 1 {
		t.Fatal("default API route changed")
	}
}

func TestBillingPreservesFailedPlanUsageWithoutPartialOutput(t *testing.T) {
	for _, mode := range []string{"siwc", "auto"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("ChatGPT response incomplete")
			completion := domain.Completion{
				Model: "requested-model", InputTokens: 7, OutputTokens: 3,
				Usage:   domain.ModelUsage{Model: "served-model", TokenUsageKnown: true, CachedInputTokens: 4},
				Message: domain.Message{Role: "assistant", Content: "partial", ToolCalls: []domain.ToolCall{{ID: "partial-call", Name: "lookup"}}, ResponsesItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)}},
			}
			plan := &billingStub{completion: &completion, err: failure}
			api := &billingStub{}
			b := &BillingModel{Mode: mode, Plan: plan, API: api}
			got, err := b.Complete(context.Background(), nil, nil)
			completion.Message = domain.Message{}
			if !errors.Is(err, failure) || !reflect.DeepEqual(got, completion) || plan.calls.Load() != 1 || api.calls.Load() != 0 {
				t.Fatalf("lost usage, exposed partial output or changed billing: %+v %v", got, err)
			}
		})
	}
}
