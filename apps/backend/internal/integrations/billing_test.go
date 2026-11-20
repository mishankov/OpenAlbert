package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

// Explicit implementation keeps profile routing from calling the nil embedded
// client in the legacy control stub.
func (m *billingStub) CompleteForProfile(ctx context.Context, profile domain.ModelProfile, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	out, err := m.Complete(ctx, messages, tools)
	out.Model = profile.Model
	return out, err
}

func TestBillingProfilesNeverFallbackOrGateAPIRoles(t *testing.T) {
	for _, failure := range []error{siwc.ErrDisconnected, &siwc.APIError{Status: 429, Code: "subscription_sharing_usage_limit_exceeded", RequestID: "request-profile"}, &siwc.APIError{Status: 403, Code: "subscription_sharing_user_not_eligible"}} {
		t.Run(failure.Error(), func(t *testing.T) {
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apiCalls.Add(1)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()
			plan := &billingStub{err: failure, completion: &domain.Completion{InputTokens: 7, OutputTokens: 3, Usage: domain.ModelUsage{Model: "served-model", TokenUsageKnown: true}, Message: domain.Message{Content: "partial"}}}
			router, err := NewModelRouterWithPlan(subscriptionRouterConfig(server.URL), plan)
			if err != nil {
				t.Fatal(err)
			}
			billing := &BillingModel{Mode: "profiles", Plan: plan, API: router, Cooldown: time.Hour}
			// Even a stale account-wide gate must not affect configured profiles.
			billing.ineligible, billing.retryAt = true, time.Now().Add(time.Hour)
			for expectedAPI := range 2 {
				out, err := billing.Complete(domain.WithModelRole(context.Background(), domain.RoleChat), []domain.Message{{Role: "user", Content: "hi"}}, nil)
				if !errors.Is(err, failure) || out.Usage.Role != domain.RoleChat || out.Usage.Profile != "chat-plan" || out.Usage.Model != "served-model" || out.InputTokens != 7 || !out.Usage.TokenUsageKnown || out.Usage.EstimatedCostUSD != nil || out.Message.Content != "" {
					t.Fatalf("failed profile metadata/fallback: %+v %v", out, err)
				}
				if apiCalls.Load() != int32(expectedAPI) {
					t.Fatal("subscription failure billed API")
				}
				out, err = billing.Complete(domain.WithModelRole(context.Background(), domain.RoleBackground), []domain.Message{{Role: "user", Content: "hi"}}, nil)
				if err != nil || out.Model != "primary-model" || out.Usage.Profile != "arbitrary-main" {
					t.Fatalf("API role blocked by account failure: %+v %v", out, err)
				}
			}
			status := billing.Status()
			if plan.calls.Load() != 2 || apiCalls.Load() != 2 || status.SIWCRequests != 2 || status.APIRequests != 2 || status.ActiveLane != "profiles" || !status.SIWC.Connected {
				t.Fatalf("wrong status: %+v", status)
			}
			var api *siwc.APIError
			if errors.As(failure, &api) && (status.LastErrorCode != api.Code || status.LastHTTPStatus != api.Status || status.LastRequestID != api.RequestID) {
				t.Fatalf("missing account diagnostics: %+v", status)
			}
			if api != nil && api.Status == 429 && status.Last429 == nil {
				t.Fatal("quota diagnostic missing")
			}
		})
	}
}

func TestBillingProfilesFitsSelectedProfile(t *testing.T) {
	plan := &billingStub{}
	config := subscriptionRouterConfig("http://localhost")
	p := config.Profiles["chat-plan"]
	p.ContextWindowTokens = 2100
	config.Profiles["chat-plan"] = p
	router, err := NewModelRouterWithPlan(config, plan)
	if err != nil {
		t.Fatal(err)
	}
	billing := &BillingModel{Mode: "profiles", Plan: plan, API: router}
	input := []domain.Message{{Role: "user", Content: strings.Repeat("x", 1500)}}
	for _, role := range []domain.ModelRole{domain.RoleChat, domain.RoleBackground, domain.RoleAnalysis} {
		fits, err := billing.FitsContext(domain.WithModelRole(context.Background(), role), input, nil)
		if err != nil || fits != (role != domain.RoleChat) {
			t.Fatalf("wrong profile budget for %s: %v %v", role, fits, err)
		}
	}
	if plan.calls.Load() != 0 || billing.Status().SIWCRequests != 0 || billing.Status().APIRequests != 0 {
		t.Fatal("sizing executed a provider request")
	}
}

type waitingStatusPlan struct {
	*billingStub
	entered, release chan struct{}
}

func (p *waitingStatusPlan) Status() siwc.Status {
	close(p.entered)
	<-p.release
	return siwc.Status{Connected: true, PlanEnabled: true}
}

func TestProfileAPIRequestDoesNotWaitForAccountStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	plan := &waitingStatusPlan{billingStub: &billingStub{}, entered: make(chan struct{}), release: make(chan struct{})}
	router, err := NewModelRouterWithPlan(subscriptionRouterConfig(server.URL), plan)
	if err != nil {
		t.Fatal(err)
	}
	billing := &BillingModel{Mode: "profiles", API: router, Plan: plan}
	statusDone := make(chan struct{})
	go func() { billing.Status(); close(statusDone) }()
	<-plan.entered
	done := make(chan error, 1)
	go func() {
		_, err := billing.Complete(domain.WithModelRole(context.Background(), domain.RoleBackground), []domain.Message{{Role: "user", Content: "API work"}}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		close(plan.release)
		<-statusDone
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(plan.release)
		<-statusDone
		<-done
		t.Fatal("API profile waited for shared ChatGPT account status")
	}
}
