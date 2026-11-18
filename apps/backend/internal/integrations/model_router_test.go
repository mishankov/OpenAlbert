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
