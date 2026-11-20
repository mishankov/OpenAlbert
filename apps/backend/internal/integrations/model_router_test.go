package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func routerConfig(base string) domain.ModelProfiles {
	return domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{domain.RoleChat: "arbitrary-main", domain.RoleBackground: "arbitrary-main", domain.RoleAnalysis: "arbitrary-small"},
		Profiles: map[string]domain.ModelProfile{
			"arbitrary-main":  {Adapter: "openai_chat_completions", BaseURL: base + "/primary", APIKey: "main-key", Model: "primary-model", Capabilities: []string{"tools", "vision", "structured_output"}, ContextWindowTokens: 100000, MaxOutputTokens: 1000, Timeout: time.Second},
			"arbitrary-small": {Adapter: "openai_chat_completions", BaseURL: base + "/utility", APIKey: "utility-key", Model: "utility-model", Capabilities: []string{"structured_output"}, ContextWindowTokens: 100000, MaxOutputTokens: 1000, Timeout: time.Second},
		},
	}
}

func TestModelRouterSelectsExplicitRolesAndReportsActualUsage(t *testing.T) {
	for _, role := range []domain.ModelRole{domain.RoleChat, domain.RoleBackground, domain.RoleAnalysis} {
		t.Run(string(role), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				wantModel, wantKey, wantPath := "primary-model", "main-key", "/primary/chat/completions"
				if role == domain.RoleAnalysis {
					wantModel, wantKey, wantPath = "utility-model", "utility-key", "/utility/chat/completions"
				}
				if string(body["model"]) != fmt.Sprintf("%q", wantModel) || r.Header.Get("Authorization") != "Bearer "+wantKey || r.URL.Path != wantPath {
					t.Error("role reached the wrong configured model")
				}
				if (len(body["response_format"]) > 0) != (role == domain.RoleAnalysis) {
					t.Error("JSON output was not isolated to analysis")
				}
				if string(body["max_tokens"]) != "1000" {
					t.Error("profile output limit missing")
				}
				_, _ = w.Write([]byte(`{"model":"actual-provider-model","choices":[{"message":{"content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3}}}`))
			}))
			defer server.Close()
			config := routerConfig(server.URL)
			name := config.Routes[role]
			p := config.Profiles[name]
			p.Pricing = &domain.ModelPricing{InputPerMillion: 3, CachedInputPerMillion: .3, OutputPerMillion: 15}
			config.Profiles[name] = p
			router, err := NewModelRouter(config)
			if err != nil {
				t.Fatal(err)
			}
			out, err := router.Complete(domain.WithModelRole(context.Background(), role), []domain.Message{{Role: "user", Content: "Return JSON"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if requests != 1 || out.Model != p.Model || out.Usage.Role != role || out.Usage.Profile != name || out.Usage.Model != "actual-provider-model" || out.Usage.CachedInputTokens != 3 || out.InputTokens != 10 || out.OutputTokens != 5 || out.Usage.EstimatedInputTokens <= 0 {
				t.Fatalf("wrong usage: %+v", out)
			}
			if out.Usage.EstimatedCostUSD == nil || *out.Usage.EstimatedCostUSD < .00009689 || *out.Usage.EstimatedCostUSD > .00009691 {
				t.Fatalf("wrong cost estimate: %+v", out.Usage)
			}
		})
	}
}

func TestModelRouterRejectsMissingRolesAndCapabilitiesBeforeHTTP(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	defer server.Close()
	router, err := NewModelRouter(routerConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role     domain.ModelRole
		messages []domain.Message
		tools    []domain.Tool
	}{
		{messages: []domain.Message{{Role: "user", Content: "hi"}}},
		{role: "typo", messages: []domain.Message{{Role: "user", Content: "hi"}}},
		{role: domain.RoleAnalysis, messages: []domain.Message{{Role: "user", Images: []string{"image"}}}},
		{role: domain.RoleAnalysis, tools: []domain.Tool{{Name: "tool", Parameters: json.RawMessage(`{}`)}}},
		{role: domain.RoleAnalysis, messages: []domain.Message{{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}}}}},
	} {
		out, err := router.Complete(domain.WithModelRole(context.Background(), tc.role), tc.messages, tc.tools)
		if err == nil {
			t.Fatalf("invalid request accepted: %+v", tc)
		}
		if tc.role == domain.RoleAnalysis && out.Usage.Profile != "arbitrary-small" {
			t.Fatal("failure lost selected profile")
		}
	}
	if requests != 0 {
		t.Fatal("invalid request reached network")
	}
}

func TestModelRouterHasNoFallbackAndPreservesFailureMetadata(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(503)
		_, _ = w.Write([]byte("private provider failure"))
	}))
	defer server.Close()
	config := routerConfig(server.URL)
	p := config.Profiles["arbitrary-main"]
	p.Pricing = &domain.ModelPricing{InputPerMillion: 3, OutputPerMillion: 15}
	config.Profiles["arbitrary-main"] = p
	router, err := NewModelRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	out, err := router.Complete(domain.WithModelRole(context.Background(), domain.RoleChat), []domain.Message{{Role: "user", Content: "hi"}}, nil)
	var upstream *HTTPError
	if !errors.As(err, &upstream) || upstream.Status != 503 || requests != 1 || out.Usage.Profile != "arbitrary-main" || out.Usage.Model != "primary-model" || out.Usage.TokenUsageKnown || out.Usage.EstimatedCostUSD != nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("failure changed routing or leaked body: %+v %v", out, err)
	}
}

func TestModelRouterPreservesBilledUsageForRejectedResponses(t *testing.T) {
	for _, tc := range []struct{ name, response string }{
		{"truncated", `{"model":"actual-model","choices":[{"message":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`},
		{"missing choices", `{"model":"actual-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`},
		{"invalid content", `{"model":"actual-model","choices":[{"message":{"content":true},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.response)) }))
			defer server.Close()
			config := routerConfig(server.URL)
			p := config.Profiles["arbitrary-main"]
			p.Pricing = &domain.ModelPricing{InputPerMillion: 3, OutputPerMillion: 15}
			config.Profiles["arbitrary-main"] = p
			router, err := NewModelRouter(config)
			if err != nil {
				t.Fatal(err)
			}
			out, err := router.Complete(domain.WithModelRole(context.Background(), domain.RoleChat), []domain.Message{{Role: "user", Content: "hi"}}, nil)
			if err == nil || out.Usage.Model != "actual-model" || out.InputTokens != 10 || out.OutputTokens != 5 || !out.Usage.TokenUsageKnown || out.Usage.EstimatedCostUSD == nil || out.Message.Content != "" {
				t.Fatalf("rejected response lost known usage or exposed partial content: %+v %v", out, err)
			}
		})
	}
}

func TestModelRouterSharedProfileKeepsConcurrentRolesSeparate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			Format *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 1 {
			t.Error("messages missing")
			w.WriteHeader(400)
			return
		}
		if (body.Format != nil) != (body.Messages[0].Content == "analysis") {
			t.Error("role-specific output mode leaked across concurrent requests")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	config := routerConfig(server.URL)
	config.Routes[domain.RoleAnalysis] = "arbitrary-main"
	router, err := NewModelRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	var calls sync.WaitGroup
	for i := 0; i < 12; i++ {
		for _, role := range []domain.ModelRole{domain.RoleChat, domain.RoleAnalysis} {
			calls.Go(func() {
				out, err := router.Complete(domain.WithModelRole(context.Background(), role), []domain.Message{{Role: "user", Content: string(role)}}, nil)
				if err != nil || out.Usage.Role != role {
					t.Errorf("concurrent route failed: %+v %v", out, err)
				}
			})
		}
	}
	calls.Wait()
}

func TestModelRouterProfileOptionsAreExplicit(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Reasoning string `json:"reasoning_effort"`
			Thinking  struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Max      int  `json:"max_completion_tokens"`
			OldMax   *int `json:"max_tokens"`
			Messages []struct {
				Role      string  `json:"role"`
				Reasoning *string `json:"reasoning_content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Reasoning != "high" || body.Thinking.Type != "enabled" || body.Max != 1000 || body.OldMax != nil || r.Header.Get("x-opencode-session") != "opaque-session" {
			t.Errorf("missing explicit request options: %+v", body)
		}
		if body.Messages[1].Reasoning == nil || *body.Messages[1].Reasoning != "fixture reasoning" {
			t.Error("reasoning replay option ignored")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"prompt_cache_hit_tokens":4}}`))
	}))
	defer server.Close()
	config := routerConfig(server.URL)
	p := config.Profiles["arbitrary-main"]
	p.Options = domain.ModelOptions{ReasoningEffort: "high", Thinking: "enabled", IncludeReasoningContent: true, SessionHeader: "x-opencode-session", OutputTokenField: "max_completion_tokens"}
	config.Profiles["arbitrary-main"] = p
	router, err := NewModelRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := domain.WithModelSession(domain.WithModelRole(context.Background(), domain.RoleChat), "opaque-session")
	out, err := router.Complete(ctx, []domain.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "answer", Reasoning: "fixture reasoning"}}, nil)
	if err != nil || out.Usage.CachedInputTokens != 4 || out.Usage.EstimatedCostUSD != nil || requests != 1 {
		t.Fatalf("bad explicit profile: %+v %v", out, err)
	}
}

func TestModelContextDropsOnlyHistoricalTurnsAndRetainsCurrentEvidence(t *testing.T) {
	old := strings.Repeat("old", 4000)
	input := []domain.Message{
		{Role: "system", Content: "Instructions"},
		{Role: "user", Content: old, TurnID: "old"},
		{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "old-call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "old-call", Content: "old result"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "Current actual request", TurnID: "current"},
		{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "current-call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}, Reasoning: "fixture reasoning"},
		{Role: "tool", ToolCallID: "current-call", Content: "current evidence"},
		{Role: "user", Content: "Artifact view", Images: []string{"data:image/jpeg;base64,YQ=="}},
	}
	before, _ := json.Marshal(input)
	client, err := NewOpenAI(OpenAIConfig{Model: "fixture", Options: domain.ModelOptions{IncludeReasoningContent: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := domain.WithModelTurn(context.Background(), "current")
	out, estimate, err := fitModelContext(ctx, client, input, nil, 2000)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]domain.Message{input[0]}, input[5:]...)
	if !reflect.DeepEqual(out, want) || estimate > 2000 {
		t.Fatalf("current turn changed: %+v (%d)", out, estimate)
	}
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("context fitting changed persisted transcript")
	}
	// A resumed old checkpoint has no marker. It must not lose evidence based on
	// a guessed user boundary, including synthetic artifact-view messages.
	if _, _, err = fitModelContext(domain.WithModelTurn(context.Background(), "missing"), client, input, nil, 2000); err == nil {
		t.Fatal("unmarked turn was destructively compacted")
	}
}

func TestModelRouterRejectsRequiredOversizedContextBeforeHTTP(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	config := routerConfig(server.URL)
	p := config.Profiles["arbitrary-main"]
	p.ContextWindowTokens = 2500
	config.Profiles["arbitrary-main"] = p
	router, err := NewModelRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		messages []domain.Message
		tools    []domain.Tool
	}{
		{messages: []domain.Message{{Role: "system", Content: strings.Repeat("x", 3000)}, {Role: "user", Content: "hi"}}},
		{messages: []domain.Message{{Role: "user", Content: strings.Repeat("я", 1500)}}},
		{messages: []domain.Message{{Role: "user", Images: []string{strings.Repeat("x", 3000)}}}},
		{messages: []domain.Message{{Role: "user", Content: "hi"}}, tools: []domain.Tool{{Name: "lookup", Description: strings.Repeat("x", 3000), Parameters: json.RawMessage(`{}`)}}},
	} {
		out, err := router.Complete(domain.WithModelRole(context.Background(), domain.RoleChat), tc.messages, tc.tools)
		if err == nil || out.Usage.EstimatedInputTokens <= 1500 {
			t.Fatalf("oversized context accepted: %+v %v", out, err)
		}
	}
	if requests != 0 {
		t.Fatal("oversized input reached provider")
	}
}

func TestModelRouterCancellationAndProfileTimeout(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	config := routerConfig(server.URL)
	p := config.Profiles["arbitrary-main"]
	p.Timeout = 30 * time.Millisecond
	config.Profiles["arbitrary-main"] = p
	router, err := NewModelRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(domain.WithModelRole(context.Background(), domain.RoleChat))
	cancel()
	if _, err = router.Complete(ctx, nil, nil); !errors.Is(err, context.Canceled) || requests.Load() != 0 {
		t.Fatalf("cancelled request was sent: %v", err)
	}
	started := time.Now()
	_, err = router.Complete(domain.WithModelRole(context.Background(), domain.RoleChat), []domain.Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil || time.Since(started) > time.Second || requests.Load() != 1 {
		t.Fatalf("profile timeout ignored: %v", err)
	}
}

func TestModelRouterValidatesRoleAssignments(t *testing.T) {
	for _, change := range []func(*domain.ModelProfiles){
		func(c *domain.ModelProfiles) { delete(c.Routes, domain.RoleAnalysis) },
		func(c *domain.ModelProfiles) { c.Routes["default"] = "arbitrary-main" },
		func(c *domain.ModelProfiles) { c.Routes[domain.RoleChat] = "missing" },
		func(c *domain.ModelProfiles) { c.Routes[domain.RoleBackground] = "arbitrary-small" },
		func(c *domain.ModelProfiles) {
			p := c.Profiles["arbitrary-small"]
			p.Capabilities = nil
			c.Profiles["arbitrary-small"] = p
		},
	} {
		config := routerConfig("http://localhost")
		change(&config)
		if _, err := NewModelRouter(config); err == nil {
			t.Fatal("invalid routing accepted")
		}
	}
}

func TestOpenAIHasNoModelOrEndpointHeuristics(t *testing.T) {
	for _, base := range []string{"https://opencode.ai/inference/go/openai/v1", "https://example.com/v1"} {
		model, err := NewOpenAI(OpenAIConfig{BaseURL: base, Model: "deepseek-fixture"})
		if err != nil {
			t.Fatal(err)
		}
		data, err := model.requestData([]domain.Message{{Role: "assistant", Content: "answer", Reasoning: "fixture"}}, nil)
		if err != nil || strings.Contains(string(data), "reasoning_content") || model.config.Options.SessionHeader != "" {
			t.Fatal("profile behavior inferred from model or hostname")
		}
	}
}

type subscriptionRouterStub struct {
	complete func(context.Context, domain.ModelProfile, []domain.Message, []domain.Tool) (domain.Completion, error)
}

func (s *subscriptionRouterStub) CompleteForProfile(ctx context.Context, profile domain.ModelProfile, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	return s.complete(ctx, profile, messages, tools)
}

func subscriptionRouterConfig(base string) domain.ModelProfiles {
	config := routerConfig(base)
	config.Routes = map[domain.ModelRole]string{domain.RoleChat: "chat-plan", domain.RoleBackground: "arbitrary-main", domain.RoleAnalysis: "analysis-plan"}
	config.Profiles["chat-plan"] = domain.ModelProfile{Adapter: "openai_chatgpt", Model: "explicit-chat", Capabilities: []string{"tools", "vision"}, ContextWindowTokens: 100000, MaxOutputTokens: 1000, Timeout: time.Second}
	config.Profiles["analysis-plan"] = domain.ModelProfile{Adapter: "openai_chatgpt", Model: "explicit-analysis", Capabilities: []string{"structured_output"}, ContextWindowTokens: 100000, MaxOutputTokens: 1000, Timeout: time.Second}
	return config
}

func TestSubscriptionRouterMixedConcurrentModelsAndUsage(t *testing.T) {
	var apiCalls, planCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		if r.Header.Get("X-Proxy-Key") != "api-proxy" {
			t.Error("API profile did not send its proxy header")
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "primary-model" {
			t.Errorf("wrong API route: %+v %v", body, err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer server.Close()
	config := subscriptionRouterConfig(server.URL)
	apiProfile := config.Profiles["arbitrary-main"]
	apiProfile.Pricing = &domain.ModelPricing{InputPerMillion: 3, OutputPerMillion: 15}
	apiProfile.Headers = map[string]string{"X-Proxy-Key": "api-proxy"}
	config.Profiles["arbitrary-main"] = apiProfile
	chatProfile := config.Profiles["chat-plan"]
	chatProfile.BaseURL = "https://chat-proxy.example.invalid/v1"
	chatProfile.Headers = map[string]string{"X-Proxy-Key": "chat-proxy"}
	config.Profiles["chat-plan"] = chatProfile
	plan := &subscriptionRouterStub{complete: func(ctx context.Context, profile domain.ModelProfile, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
		planCalls.Add(1)
		want := config.Profiles[config.Routes[domain.ModelRoleFromContext(ctx)]]
		if profile.Model != want.Model || profile.BaseURL != want.BaseURL || profile.Headers["X-Proxy-Key"] != want.Headers["X-Proxy-Key"] {
			t.Error("concurrent subscription request used the wrong profile transport")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("profile timeout missing")
		}
		return domain.Completion{Model: profile.Model, Message: domain.Message{Role: "assistant", Content: "{}"}, InputTokens: 10, OutputTokens: 5, Usage: domain.ModelUsage{Model: "served-" + profile.Model, TokenUsageKnown: true, CachedInputTokens: 3}}, nil
	}}
	router, err := NewModelRouterWithPlan(config, plan)
	if err != nil {
		t.Fatal(err)
	}
	billing := &BillingModel{Mode: "profiles", API: router}
	var calls sync.WaitGroup
	for range 10 {
		for _, role := range []domain.ModelRole{domain.RoleChat, domain.RoleBackground, domain.RoleAnalysis} {
			calls.Go(func() {
				out, err := billing.Complete(domain.WithModelRole(context.Background(), role), []domain.Message{{Role: "user", Content: "hi"}}, nil)
				if err != nil || out.Usage.Role != role || out.Usage.Profile != config.Routes[role] || out.Model != config.Profiles[config.Routes[role]].Model || !out.Usage.TokenUsageKnown || out.InputTokens != 10 || out.OutputTokens != 5 || out.Usage.EstimatedInputTokens <= 0 {
					t.Errorf("wrong profile usage: %+v %v", out, err)
				}
				if (out.Usage.EstimatedCostUSD != nil) != (role == domain.RoleBackground) {
					t.Errorf("wrong price lane: %+v", out.Usage)
				}
			})
		}
	}
	calls.Wait()
	status := billing.Status()
	if apiCalls.Load() != 10 || planCalls.Load() != 20 || status.APIRequests != 10 || status.SIWCRequests != 20 || status.ActiveLane != "profiles" || len(status.Profiles) != 3 {
		t.Fatalf("wrong lane counters: %+v", status)
	}
	if status.Profiles[domain.RoleChat] != (ProfileRouteStatus{Profile: "chat-plan", Model: "explicit-chat", AuthMode: "siwc"}) || status.Profiles[domain.RoleBackground].AuthMode != "api_key" {
		t.Fatalf("wrong profile status: %+v", status.Profiles)
	}
	delete(status.Profiles, domain.RoleChat)
	if len(billing.Status().Profiles) != 3 {
		t.Fatal("caller mutated router status")
	}
}

func TestSubscriptionRouterUsesProfileBudgetAndProtectsCurrentTurn(t *testing.T) {
	var calls int
	var received []domain.Message
	plan := &subscriptionRouterStub{complete: func(ctx context.Context, profile domain.ModelProfile, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
		calls++
		received = messages
		return domain.Completion{Model: profile.Model}, nil
	}}
	config := subscriptionRouterConfig("http://localhost")
	p := config.Profiles["chat-plan"]
	p.ContextWindowTokens, p.MaxOutputTokens = 2200, 1000
	config.Profiles["chat-plan"] = p
	router, err := NewModelRouterWithPlan(config, plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx := domain.WithModelTurn(domain.WithModelRole(context.Background(), domain.RoleChat), "current")
	input := []domain.Message{{Role: "system", Content: "instructions"}, {Role: "user", Content: strings.Repeat("old", 1000), TurnID: "old"}, {Role: "assistant", Content: "old answer"}, {Role: "user", Content: "current", TurnID: "current"}}
	out, err := router.Complete(ctx, input, nil)
	if err != nil || out.Usage.DroppedMessages != 2 || len(received) != 2 || received[1].Content != "current" || out.Usage.EstimatedInputTokens > 1200 {
		t.Fatalf("bad compaction: %+v %+v %v", out, received, err)
	}
	if fits, err := router.FitsContext(ctx, received, nil); !fits || err != nil {
		t.Fatalf("fit estimate diverged: %v %v", fits, err)
	}
	oversized := []domain.Message{{Role: "user", Content: strings.Repeat("я", 1000), TurnID: "current"}}
	if fits, err := router.FitsContext(ctx, oversized, nil); fits || err != nil {
		t.Fatalf("output reserve ignored: %v %v", fits, err)
	}
	out, err = router.Complete(ctx, oversized, nil)
	if err == nil || calls != 1 || out.Usage.Role != domain.RoleChat || out.Usage.Profile != "chat-plan" || out.Usage.Model != "explicit-chat" || out.Usage.EstimatedInputTokens <= 1200 {
		t.Fatalf("required input changed or called provider: %+v %v", out, err)
	}
}

func TestSubscriptionRouterCapabilityAndHistoricalImageRules(t *testing.T) {
	calls := 0
	plan := &subscriptionRouterStub{complete: func(ctx context.Context, profile domain.ModelProfile, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
		calls++
		for _, message := range messages {
			if len(message.Images) != 0 {
				t.Error("unsupported image reached provider")
			}
		}
		return domain.Completion{Model: profile.Model}, nil
	}}
	router, err := NewModelRouterWithPlan(subscriptionRouterConfig("http://localhost"), plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx := domain.WithModelTurn(domain.WithModelRole(context.Background(), domain.RoleAnalysis), "current")
	input := []domain.Message{{Role: "system", Content: "instructions"}, {Role: "user", Images: []string{"image"}, TurnID: "old"}, {Role: "assistant", Content: "old answer"}, {Role: "user", Content: "current", TurnID: "current"}}
	out, err := router.Complete(ctx, input, nil)
	if err != nil || out.Usage.DroppedMessages != 2 || calls != 1 {
		t.Fatalf("historical image handling: %+v %v", out, err)
	}
	for _, messages := range [][]domain.Message{
		{{Role: "user", Images: []string{"image"}, TurnID: "current"}},
		{{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "call", Name: "lookup"}}}},
		{{Role: "user", ContextOverflow: true}},
	} {
		if _, err := router.Complete(ctx, messages, nil); err == nil {
			t.Fatal("unsupported required input accepted")
		}
	}
	if _, err := router.Complete(ctx, nil, []domain.Tool{{Name: "lookup", Parameters: json.RawMessage(`{}`)}}); err == nil || calls != 1 {
		t.Fatal("unsupported tools accepted")
	}
}

func TestSubscriptionRouterTimeoutAndFailedUsage(t *testing.T) {
	plan := &subscriptionRouterStub{complete: func(ctx context.Context, profile domain.ModelProfile, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
		<-ctx.Done()
		return domain.Completion{Model: profile.Model, InputTokens: 7, OutputTokens: 3, Usage: domain.ModelUsage{Model: "served", TokenUsageKnown: true, CachedInputTokens: 4}, Message: domain.Message{Content: "partial"}}, ctx.Err()
	}}
	config := subscriptionRouterConfig("http://localhost")
	p := config.Profiles["chat-plan"]
	p.Timeout = 20 * time.Millisecond
	config.Profiles["chat-plan"] = p
	router, err := NewModelRouterWithPlan(config, plan)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	out, err := router.Complete(domain.WithModelRole(context.Background(), domain.RoleChat), []domain.Message{{Role: "user", Content: "hi"}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second || out.Message.Content != "" || out.Usage.Profile != "chat-plan" || out.Usage.Model != "served" || out.InputTokens != 7 || out.OutputTokens != 3 || !out.Usage.TokenUsageKnown || out.Usage.CachedInputTokens != 4 || out.Usage.EstimatedCostUSD != nil {
		t.Fatalf("lost timeout or failed usage: %+v %v", out, err)
	}
}

func TestSubscriptionRouterRequiresClientAndRejectsAPIOptions(t *testing.T) {
	config := subscriptionRouterConfig("http://localhost")
	if _, err := NewModelRouter(config); err == nil {
		t.Fatal("subscription without client accepted")
	}
	plan := &subscriptionRouterStub{}
	for _, change := range []func(*domain.ModelProfile){
		func(p *domain.ModelProfile) { p.APIKey = "secret" },
		func(p *domain.ModelProfile) { p.APIKeyEnv = "KEY" },
		func(p *domain.ModelProfile) { p.Options.ReasoningEffort = "high" },
		func(p *domain.ModelProfile) { p.Pricing = &domain.ModelPricing{InputPerMillion: 1} },
	} {
		config := subscriptionRouterConfig("http://localhost")
		p := config.Profiles["chat-plan"]
		change(&p)
		config.Profiles["chat-plan"] = p
		if _, err := NewModelRouterWithPlan(config, plan); err == nil {
			t.Fatal("API-only subscription field accepted")
		}
	}
}
