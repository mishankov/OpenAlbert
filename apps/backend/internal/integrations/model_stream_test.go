package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations/siwc"
)

type fakeCatalogSubscription struct {
	models      []siwc.Model
	modelsByURL map[string][]siwc.Model
	stream      func(context.Context, domain.ModelProfile, []domain.Message, []domain.Tool, domain.StreamFunc) (domain.Completion, error)
}

func (f *fakeCatalogSubscription) CompleteForProfile(ctx context.Context, p domain.ModelProfile, m []domain.Message, t []domain.Tool) (domain.Completion, error) {
	return f.CompleteForProfileStream(ctx, p, m, t, nil)
}

func (f *fakeCatalogSubscription) CompleteForProfileStream(ctx context.Context, p domain.ModelProfile, m []domain.Message, t []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
	if f.stream != nil {
		return f.stream(ctx, p, m, t, onEvent)
	}
	if onEvent != nil {
		_ = onEvent(domain.StreamEvent{Type: "text_delta", Delta: "sub-hello"})
	}
	return domain.Completion{Model: p.Model, Message: domain.Message{Role: "assistant", Content: "sub-hello"}, InputTokens: 5, OutputTokens: 2, Usage: domain.ModelUsage{Model: p.Model, TokenUsageKnown: true}}, nil
}

func (f *fakeCatalogSubscription) Models(context.Context) ([]siwc.Model, error) {
	return f.models, nil
}

func (f *fakeCatalogSubscription) ModelsForProfile(_ context.Context, p domain.ModelProfile, _ bool) ([]siwc.Model, error) {
	if f.modelsByURL != nil {
		if list, ok := f.modelsByURL[p.BaseURL]; ok {
			return list, nil
		}
	}
	return f.models, nil
}

func TestModelRouterCatalogDiscoveryAndStreaming(t *testing.T) {
	var modelsHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelsHits.Add(1)
			if r.Header.Get("Authorization") != "Bearer test-api-key" || r.Header.Get("X-Proxy-Key") != "proxy-secret" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[
				{"id":"gpt-5.4","name":"GPT 5.4","context_length":200000,"max_output_tokens":16384},
				{"id":"gpt-5.4-mini","context_length":128000},
				{"id":"text-embedding-3-large"}
			]}`))
		case "/v1/chat/completions":
			var body struct {
				Model           string `json:"model"`
				Stream          bool   `json:"stream"`
				ReasoningEffort string `json:"reasoning_effort"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !body.Stream || body.Model != "gpt-5.4-mini" || body.ReasoningEffort != "high" {
				t.Errorf("unexpected stream request: %+v", body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			chunks := []string{
				`{"model":"gpt-5.4-mini","choices":[{"delta":{"reasoning_content":"think "}}]}`,
				`{"model":"gpt-5.4-mini","choices":[{"delta":{"content":"Hello "}}]}`,
				`{"model":"gpt-5.4-mini","choices":[{"delta":{"content":"world","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"codemode","arguments":"{\"code\":"}}]}}]}`,
				`{"model":"gpt-5.4-mini","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"return 1\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`{"model":"gpt-5.4-mini","choices":[],"usage":{"prompt_tokens":40,"completion_tokens":12,"prompt_tokens_details":{"cached_tokens":10}}}`,
			}
			for _, chunk := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "proxy/gpt-5.4",
			domain.RoleBackground: "proxy/gpt-5.4",
			domain.RoleAnalysis:   "proxy/gpt-5.4",
			domain.RoleCoding:     "chatgpt/codex-5",
		},
		Providers: map[string]domain.ModelProvider{
			"proxy": {
				Name:                       "proxy",
				Adapter:                    "openai_chat_completions",
				BaseURL:                    server.URL + "/v1",
				Headers:                    map[string]string{"X-Proxy-Key": "proxy-secret"},
				APIKey:                     "test-api-key",
				DiscoverModels:             true,
				IncludeModels:              []string{"gpt-*"},
				ExcludeModels:              []string{"*embedding*"},
				DefaultCapabilities:        []string{"tools", "vision", "structured_output"},
				DefaultContextWindowTokens: 128000,
				DefaultMaxOutputTokens:     8192,
				DefaultTimeout:             30 * time.Second,
			},
			"chatgpt": {
				Name:                       "chatgpt",
				Adapter:                    "openai_chatgpt",
				DiscoverModels:             true,
				DefaultCapabilities:        []string{"tools", "vision", "structured_output"},
				DefaultContextWindowTokens: 128000,
				DefaultMaxOutputTokens:     8192,
				DefaultTimeout:             30 * time.Second,
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"proxy/gpt-5.4": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: server.URL + "/v1",
				Headers: map[string]string{"X-Proxy-Key": "proxy-secret"}, APIKey: "test-api-key",
				Model: "gpt-5.4", Capabilities: []string{"tools", "vision", "structured_output"},
				SupportedReasoningEfforts: []string{"low", "medium", "high", "xhigh"},
				ContextWindowTokens:       128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
			"chatgpt/codex-5": {
				Provider: "chatgpt", Adapter: "openai_chatgpt", Model: "codex-5",
				Capabilities:        []string{"tools", "vision", "structured_output"},
				ContextWindowTokens: 128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	}
	sub := &fakeCatalogSubscription{
		models: []siwc.Model{
			{Slug: "codex-5", DisplayName: "Codex 5", Visibility: "list", SupportedReasoningLevels: []string{"low", "medium", "high"}},
			{Slug: "gpt-5.4-sub", DisplayName: "GPT 5.4 Sub", Visibility: "list"},
		},
	}
	router, err := NewModelRouterWithPlan(cfg, sub)
	if err != nil {
		t.Fatal(err)
	}

	def, catalog, err := router.Catalog(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if def != "chatgpt/codex-5" {
		t.Fatalf("unexpected default model: %s", def)
	}
	ids := map[string]domain.CatalogModel{}
	for _, m := range catalog {
		ids[m.ID] = m
	}
	if _, ok := ids["proxy/text-embedding-3-large"]; ok {
		t.Fatal("excluded embedding model leaked into catalog")
	}
	for _, want := range []string{"chatgpt/codex-5", "chatgpt/gpt-5.4-sub", "proxy/gpt-5.4", "proxy/gpt-5.4-mini"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("missing expected catalog model %s in %+v", want, catalog)
		}
	}
	if strings.Join(ids["chatgpt/codex-5"].SupportedReasoningEfforts, ",") != "low,medium,high" {
		t.Fatalf("expected discovered reasoning efforts on chatgpt/codex-5, got %+v", ids["chatgpt/codex-5"].SupportedReasoningEfforts)
	}
	if strings.Join(ids["chatgpt/gpt-5.4-sub"].SupportedReasoningEfforts, ",") != "low,medium,high,xhigh,max" {
		t.Fatalf("expected default reasoning efforts on chatgpt/gpt-5.4-sub, got %+v", ids["chatgpt/gpt-5.4-sub"].SupportedReasoningEfforts)
	}
	if strings.Join(ids["proxy/gpt-5.4"].SupportedReasoningEfforts, ",") != "low,medium,high,xhigh" {
		t.Fatalf("expected configured reasoning efforts including xhigh on proxy/gpt-5.4, got %+v", ids["proxy/gpt-5.4"].SupportedReasoningEfforts)
	}
	// Second call hits cache.
	if _, _, err := router.Catalog(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if modelsHits.Load() != 1 {
		t.Fatalf("expected 1 cached /models fetch, got %d", modelsHits.Load())
	}

	// Stream a dynamically discovered model with reasoning_effort override.
	var textOut, reasonOut strings.Builder
	var toolDeltas int
	comp, err := router.CompleteModelStream(
		context.Background(),
		"proxy/gpt-5.4-mini",
		domain.ModelOptions{ReasoningEffort: "high"},
		[]domain.Message{{Role: "user", Content: "hi"}},
		[]domain.Tool{{Name: "codemode", Parameters: json.RawMessage(`{"type":"object"}`)}},
		func(ev domain.StreamEvent) error {
			switch ev.Type {
			case "text_delta":
				textOut.WriteString(ev.Delta)
			case "reasoning_delta":
				reasonOut.WriteString(ev.Delta)
			case "tool_call_delta":
				toolDeltas++
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if textOut.String() != "Hello world" || reasonOut.String() != "think " || toolDeltas < 2 {
		t.Fatalf("unexpected stream callbacks: text=%q reason=%q toolDeltas=%d", textOut.String(), reasonOut.String(), toolDeltas)
	}
	if comp.Message.Content != "Hello world" || comp.Message.Reasoning != "think " || len(comp.Message.ToolCalls) != 1 || comp.Message.ToolCalls[0].Name != "codemode" || string(comp.Message.ToolCalls[0].Arguments) != `{"code":"return 1"}` {
		t.Fatalf("unexpected assembled completion: %+v", comp)
	}
	if comp.InputTokens != 40 || comp.OutputTokens != 12 || comp.Usage.CachedInputTokens != 10 {
		t.Fatalf("unexpected usage: %+v", comp.Usage)
	}

	// Also verify subscription streaming forwards reasoning_effort override.
	var subText, subEffort string
	sub.stream = func(ctx context.Context, p domain.ModelProfile, m []domain.Message, tools []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
		subEffort = p.Options.ReasoningEffort
		if onEvent != nil {
			_ = onEvent(domain.StreamEvent{Type: "text_delta", Delta: "sub-hello"})
		}
		return domain.Completion{Model: p.Model, Message: domain.Message{Role: "assistant", Content: "sub-hello"}, InputTokens: 5, OutputTokens: 2, Usage: domain.ModelUsage{Model: p.Model, TokenUsageKnown: true}}, nil
	}
	subComp, err := router.CompleteModelStream(context.Background(), "chatgpt/gpt-5.4-sub", domain.ModelOptions{ReasoningEffort: "xhigh"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, func(ev domain.StreamEvent) error {
		if ev.Type == "text_delta" {
			subText += ev.Delta
		}
		return nil
	})
	if err != nil || subText != "sub-hello" || subComp.Message.Content != "sub-hello" || subEffort != "xhigh" {
		t.Fatalf("unexpected subscription stream: comp=%+v text=%q effort=%q err=%v", subComp, subText, subEffort, err)
	}
	// Discovered per-model allowlist on chatgpt/codex-5 rejects unsupported effort.
	if _, err := router.CompleteModelStream(context.Background(), "chatgpt/codex-5", domain.ModelOptions{ReasoningEffort: "xhigh"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, nil); err == nil {
		t.Fatal("expected unsupported reasoning_effort xhigh to be rejected for chatgpt/codex-5")
	}
}

func TestOpenAIListModelsFormatsAndReasoningEfforts(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "models envelope with slug and levels",
			body: `{"models":[{"slug":"m1","display_name":"Model 1","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`,
		},
		{
			name: "bare array with id and efforts",
			body: `[{"id":"m1","name":"Model 1","supported_reasoning_efforts":["low","high"]}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client, err := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "catalog", HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ListModels(context.Background())
			if err != nil || len(got) != 1 || got[0].ID != "m1" || got[0].DisplayName != "Model 1" || strings.Join(got[0].SupportedReasoningEfforts, ",") != "low,high" {
				t.Fatalf("unexpected discovered models: %+v %v", got, err)
			}
		})
	}
}

func TestOpenAICompleteStreamRejectsPrematureEOF(t *testing.T) {
	cases := []struct {
		name  string
		chunk string
	}{
		{
			name:  "partial text without finish_reason or DONE",
			chunk: `{"model":"gpt-5.4","choices":[{"delta":{"content":"partial answer"}}]}`,
		},
		{
			name:  "partial valid-JSON tool call without finish_reason or DONE",
			chunk: `{"model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"codemode","arguments":"{\"code\":\"return 1\"}"}}]}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\n", tc.chunk)
				// Close stream cleanly without finish_reason or [DONE].
			}))
			defer srv.Close()

			client, err := NewOpenAI(OpenAIConfig{
				BaseURL:    srv.URL,
				APIKey:     "test-key",
				Model:      "gpt-5.4",
				HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CompleteStream(context.Background(), []domain.Message{{Role: "user", Content: "hi"}}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "without completion") {
				t.Fatalf("expected premature EOF error, got %v", err)
			}
		})
	}
}

type fakePlanBillingClient struct {
	model        string
	models       []siwc.Model
	modelsErr    error
	disconnected bool
	calls        atomic.Int32
	lastModel    string
	lastEffort   string
	streamErr    error
	blockCtx     bool
}

func (f *fakePlanBillingClient) Complete(ctx context.Context, m []domain.Message, t []domain.Tool) (domain.Completion, error) {
	return f.CompleteForProfileStream(ctx, domain.ModelProfile{Model: f.model}, m, t, nil)
}
func (f *fakePlanBillingClient) CompleteForProfileStream(ctx context.Context, p domain.ModelProfile, m []domain.Message, t []domain.Tool, onEvent domain.StreamFunc) (domain.Completion, error) {
	f.calls.Add(1)
	f.lastModel = p.Model
	f.lastEffort = p.Options.ReasoningEffort
	if f.blockCtx {
		<-ctx.Done()
		return domain.Completion{}, ctx.Err()
	}
	if f.streamErr != nil {
		return domain.Completion{}, f.streamErr
	}
	if onEvent != nil {
		_ = onEvent(domain.StreamEvent{Type: "text_delta", Delta: "from-plan"})
	}
	return domain.Completion{Model: p.Model, Message: domain.Message{Role: "assistant", Content: "from-plan"}}, nil
}
func (f *fakePlanBillingClient) Status() siwc.Status {
	return siwc.Status{PlanEnabled: !f.disconnected, Model: f.model}
}
func (f *fakePlanBillingClient) Begin() (string, error)               { return "", nil }
func (f *fakePlanBillingClient) Finish(context.Context, string) error { return nil }
func (f *fakePlanBillingClient) Models(context.Context) ([]siwc.Model, error) {
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.models, nil
}
func (f *fakePlanBillingClient) SetModel(context.Context, string) error { return nil }
func (f *fakePlanBillingClient) Close() error                           { return nil }

func TestBillingCompleteModelStreamRespectsModeAndPerModelAllowlistAndOverrides(t *testing.T) {
	var apiHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"},{"id":"no-tools-model"}]}`))
		case "/v1/chat/completions":
			apiHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"model\":\"gpt-5.4\",\"choices\":[{\"delta\":{\"content\":\"from-api\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "proxy/gpt-5.4",
			domain.RoleBackground: "proxy/gpt-5.4",
			domain.RoleAnalysis:   "proxy/gpt-5.4",
		},
		Providers: map[string]domain.ModelProvider{
			"proxy": {
				Name: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", DiscoverModels: true,
				DefaultCapabilities:        []string{"tools", "structured_output"},
				DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
			},
		},
		Models: map[string]domain.ModelProfile{
			"proxy/no-tools-model": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", Model: "no-tools-model", Capabilities: []string{"structured_output"},
				ContextWindowTokens: 128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"proxy/gpt-5.4": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", Model: "gpt-5.4", Capabilities: []string{"tools", "structured_output"},
				SupportedReasoningEfforts: []string{"low"},
				ContextWindowTokens:       128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	}
	router, err := NewModelRouter(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Finding 13: discovered model whose override disabled tools must stay out of catalog.
	_, catalog, err := router.Catalog(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range catalog {
		if m.ID == "proxy/no-tools-model" {
			t.Fatalf("model with tools disabled by override leaked into catalog: %+v", catalog)
		}
	}

	// Finding 10: gateway enforces per-model SupportedReasoningEfforts allowlist.
	noopStream := func(domain.StreamEvent) error { return nil }
	if _, err := router.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "ultra"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err == nil {
		t.Fatal("expected gateway to reject unsupported reasoning_effort 'ultra' for model allowing only 'low'")
	}
	if _, err := router.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "low"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err != nil {
		t.Fatalf("expected allowed reasoning_effort 'low' to succeed: %v", err)
	}

	// Finding 1: siwc mode calls Plan, advertises reasoning efforts, and forwards reasoning_effort.
	apiBefore := apiHits.Load()
	plan := &fakePlanBillingClient{model: "codex-5", models: []siwc.Model{{Slug: "codex-5", Visibility: "list", SupportedReasoningLevels: []string{"low", "high"}}}}
	billingSIWC := &BillingModel{Mode: "siwc", Plan: plan, API: router}
	if _, siwcCat, err := billingSIWC.Catalog(context.Background(), false); err != nil || len(siwcCat) != 1 || strings.Join(siwcCat[0].SupportedReasoningEfforts, ",") != "low,high" {
		t.Fatalf("siwc mode catalog missing reasoning efforts: %+v %v", siwcCat, err)
	}
	comp, err := billingSIWC.CompleteModelStream(context.Background(), "chatgpt/codex-5", domain.ModelOptions{ReasoningEffort: "high"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream)
	if err != nil || comp.Message.Content != "from-plan" || plan.calls.Load() != 1 || plan.lastEffort != "high" || apiHits.Load() != apiBefore {
		t.Fatalf("siwc mode did not stay on Plan lane or forward effort: comp=%+v effort=%q err=%v planCalls=%d apiDelta=%d", comp, plan.lastEffort, err, plan.calls.Load(), apiHits.Load()-apiBefore)
	}

	// Finding 1: auto mode tries Plan first and falls back to API on retryable siwc limit error.
	failingPlan := &fakePlanBillingClient{
		model:     "codex-5",
		streamErr: &siwc.APIError{Status: 429, Code: "subscription_sharing_usage_limit_exceeded"},
	}
	billingAuto := &BillingModel{Mode: "auto", Plan: failingPlan, API: router}
	// auto mode preserves model reasoning_effort options and validates them before attempting plan.
	if _, autoCat, err := billingAuto.Catalog(context.Background(), false); err != nil {
		t.Fatal(err)
	} else {
		var foundGPT bool
		for _, m := range autoCat {
			if m.ID == "proxy/gpt-5.4" {
				foundGPT = true
				if strings.Join(m.SupportedReasoningEfforts, ",") != "low" {
					t.Fatalf("auto mode altered proxy/gpt-5.4 reasoning efforts: %+v", m.SupportedReasoningEfforts)
				}
			}
		}
		if !foundGPT {
			t.Fatalf("proxy/gpt-5.4 missing from auto catalog: %+v", autoCat)
		}
	}
	if _, err := billingAuto.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "ultra"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err == nil {
		t.Fatal("auto mode must reject unsupported reasoning_effort 'ultra' before touching plan")
	}
	if failingPlan.calls.Load() != 0 {
		t.Fatalf("plan should not have been called on rejected reasoning_effort, got %d calls", failingPlan.calls.Load())
	}

	comp, err = billingAuto.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "low"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream)
	if err != nil || comp.Message.Content != "from-api" || failingPlan.calls.Load() != 1 || failingPlan.lastEffort != "low" || apiHits.Load() != apiBefore+1 {
		t.Fatalf("auto mode did not try Plan then fall back to API: comp=%+v effort=%q err=%v planCalls=%d", comp, failingPlan.lastEffort, err, failingPlan.calls.Load())
	}

	// P2-2: legacy siwc plan stream enforces upstream timeout even when caller supplies background context.
	stalledPlan := &fakePlanBillingClient{model: "codex-5", blockCtx: true}
	billingTimeout := &BillingModel{Mode: "siwc", Plan: stalledPlan, API: router, StreamTimeout: 80 * time.Millisecond}
	if _, err := billingTimeout.CompleteModelStream(context.Background(), "chatgpt/codex-5", domain.ModelOptions{}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err == nil {
		t.Fatal("expected stalled siwc stream to time out")
	}
}

func TestAutoModeIntersectsPlanAndAPIEffortsAndRestoresDuringCooldown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"model\":\"gpt-5.4\",\"choices\":[{\"delta\":{\"content\":\"api-ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	defer upstream.Close()

	router, err := NewModelRouter(domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "proxy/gpt-5.4",
			domain.RoleBackground: "proxy/gpt-5.4",
			domain.RoleAnalysis:   "proxy/gpt-5.4",
		},
		Providers: map[string]domain.ModelProvider{
			"proxy": {
				Name: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", DiscoverModels: true,
				DefaultCapabilities:        []string{"tools", "structured_output"},
				DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
				DefaultOptions: domain.ModelOptions{ReasoningEffort: "high"},
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"proxy/gpt-5.4": {
				Provider: "proxy", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", Model: "gpt-5.4", Capabilities: []string{"tools", "structured_output"},
				Options:                   domain.ModelOptions{ReasoningEffort: "high"},
				SupportedReasoningEfforts: []string{"low", "xhigh"},
				ContextWindowTokens:       128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Explicit per-model allowlist ["low","xhigh"] must NOT be expanded by provider default "high".
	_, apiCat, err := router.Catalog(context.Background(), false)
	if err != nil || len(apiCat) != 1 || strings.Join(apiCat[0].SupportedReasoningEfforts, ",") != "low,xhigh" {
		t.Fatalf("explicit per-model allowlist was altered: %+v %v", apiCat, err)
	}
	if err := router.ValidateModelOptions("proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "high"}); err == nil {
		t.Fatal("router accepted provider default effort excluded by explicit per-model allowlist")
	}

	// In auto mode with active plan supporting ["low","high"], only intersection ["low"] is offered/accepted.
	plan := &fakePlanBillingClient{
		model:     "codex-5",
		models:    []siwc.Model{{Slug: "codex-5", Visibility: "list", SupportedReasoningLevels: []string{"low", "high"}}},
		streamErr: &siwc.APIError{Status: 429, Code: "subscription_sharing_usage_limit_exceeded"},
	}
	billing := &BillingModel{Mode: "auto", Plan: plan, API: router, Cooldown: time.Minute}
	_, cat, err := billing.Catalog(context.Background(), false)
	if err != nil || len(cat) != 1 || strings.Join(cat[0].SupportedReasoningEfforts, ",") != "low" {
		t.Fatalf("auto mode active plan did not intersect API and plan efforts: %+v %v", cat, err)
	}
	noopStream := func(domain.StreamEvent) error { return nil }
	if _, err := billing.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "xhigh"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err == nil {
		t.Fatal("auto mode must reject effort unsupported by active plan model before calling plan")
	}
	if plan.calls.Load() != 0 {
		t.Fatalf("plan must not be called for unsupported effort, got %d calls", plan.calls.Load())
	}

	// Trigger cooldown via 429 on plan lane; afterwards API model's full allowlist ["low","xhigh"] is exposed and accepted.
	if _, err := billing.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "low"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err != nil {
		t.Fatalf("expected fallback to API on plan 429: %v", err)
	}
	_, cooldownCat, err := billing.Catalog(context.Background(), false)
	if err != nil || len(cooldownCat) != 1 || strings.Join(cooldownCat[0].SupportedReasoningEfforts, ",") != "low,xhigh" {
		t.Fatalf("auto mode during cooldown did not restore API efforts: %+v %v", cooldownCat, err)
	}
	if _, err := billing.CompleteModelStream(context.Background(), "proxy/gpt-5.4", domain.ModelOptions{ReasoningEffort: "xhigh"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err != nil {
		t.Fatalf("expected API effort xhigh to succeed during plan cooldown: %v", err)
	}
}

func TestAutoModeMatchesPlanSlugPerCatalogEntryAndHandlesDisconnectedPlan(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-b"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"model\":\"model-b\",\"choices\":[{\"delta\":{\"content\":\"api-ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	defer upstream.Close()

	router, err := NewModelRouter(domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "chatgpt/model-b",
			domain.RoleBackground: "chatgpt/model-b",
			domain.RoleAnalysis:   "chatgpt/model-b",
		},
		Providers: map[string]domain.ModelProvider{
			"chatgpt": {
				Name: "chatgpt", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", DiscoverModels: true,
				DefaultCapabilities:        []string{"tools", "structured_output"},
				DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"chatgpt/model-b": {
				Provider: "chatgpt", Adapter: "openai_chat_completions", BaseURL: upstream.URL + "/v1",
				APIKey: "k", Model: "model-b", Capabilities: []string{"tools", "structured_output"},
				SupportedReasoningEfforts: []string{"minimal", "low", "high"},
				ContextWindowTokens:       128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	noopStream := func(domain.StreamEvent) error { return nil }

	// Account default plan model "model-a" supports ["low"], while "model-b" supports ["high"].
	// Catalog and streaming for "chatgpt/model-b" must both use slug "model-b" (intersection: ["high"]).
	plan := &fakePlanBillingClient{
		model: "model-a",
		models: []siwc.Model{
			{Slug: "model-a", Visibility: "list", SupportedReasoningLevels: []string{"low"}},
			{Slug: "model-b", Visibility: "list", SupportedReasoningLevels: []string{"high"}},
		},
	}
	billing := &BillingModel{Mode: "auto", Plan: plan, API: router}
	_, cat, err := billing.Catalog(context.Background(), false)
	if err != nil || len(cat) != 1 || strings.Join(cat[0].SupportedReasoningEfforts, ",") != "high" {
		t.Fatalf("expected chatgpt/model-b catalog to intersect with plan slug model-b, got %+v %v", cat, err)
	}
	if _, err := billing.CompleteModelStream(context.Background(), "chatgpt/model-b", domain.ModelOptions{ReasoningEffort: "low"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err == nil {
		t.Fatal("expected effort 'low' (supported only by model-a) to be rejected for chatgpt/model-b")
	}
	comp, err := billing.CompleteModelStream(context.Background(), "chatgpt/model-b", domain.ModelOptions{ReasoningEffort: "high"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream)
	if err != nil || comp.Message.Content != "from-plan" || plan.lastModel != "model-b" || plan.lastEffort != "high" {
		t.Fatalf("expected chatgpt/model-b to dispatch to plan slug model-b with high: comp=%+v model=%q effort=%q err=%v", comp, plan.lastModel, plan.lastEffort, err)
	}

	// Disconnected plan in auto mode preserves API-only effort "minimal" and routes directly to API.
	disconnectedPlan := &fakePlanBillingClient{
		model:        "model-a",
		disconnected: true,
		modelsErr:    siwc.ErrDisconnected,
	}
	billingDisconnected := &BillingModel{Mode: "auto", Plan: disconnectedPlan, API: router}
	_, discCat, err := billingDisconnected.Catalog(context.Background(), false)
	if err != nil || len(discCat) != 1 || strings.Join(discCat[0].SupportedReasoningEfforts, ",") != "minimal,low,high" {
		t.Fatalf("disconnected plan altered API model efforts: %+v %v", discCat, err)
	}
	comp, err = billingDisconnected.CompleteModelStream(context.Background(), "chatgpt/model-b", domain.ModelOptions{ReasoningEffort: "minimal"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream)
	if err != nil || comp.Message.Content != "api-ok" || disconnectedPlan.calls.Load() != 0 {
		t.Fatalf("disconnected plan blocked API fallback with minimal effort: comp=%+v planCalls=%d err=%v", comp, disconnectedPlan.calls.Load(), err)
	}

	// Enabled plan whose /models lookup fails intersects catalog against default plan efforts (matching streaming validation).
	failingCatalogPlan := &fakePlanBillingClient{
		model:     "model-a",
		modelsErr: errors.New("temporary catalog failure"),
	}
	billingFailingCat := &BillingModel{Mode: "auto", Plan: failingCatalogPlan, API: router}
	_, failCat, err := billingFailingCat.Catalog(context.Background(), false)
	if err != nil || len(failCat) != 1 || strings.Join(failCat[0].SupportedReasoningEfforts, ",") != "low,high" {
		t.Fatalf("enabled plan with failing /models must intersect with default plan efforts: %+v %v", failCat, err)
	}
	if _, err := billingFailingCat.CompleteModelStream(context.Background(), "chatgpt/model-b", domain.ModelOptions{ReasoningEffort: "minimal"}, []domain.Message{{Role: "user", Content: "hi"}}, nil, noopStream); err == nil {
		t.Fatal("expected minimal effort to be rejected when enabled plan lookup falls back to default efforts")
	}
	if failingCatalogPlan.calls.Load() != 0 {
		t.Fatalf("plan should not be called on rejected effort, got %d calls", failingCatalogPlan.calls.Load())
	}
}

func TestMultipleSubscriptionProvidersDiscoverDistinctCatalogs(t *testing.T) {
	sub := &fakeCatalogSubscription{
		modelsByURL: map[string][]siwc.Model{
			"https://proxy-a.invalid/v1": {{Slug: "alpha-model", DisplayName: "Alpha", Visibility: "list"}},
			"https://proxy-b.invalid/v1": {{Slug: "beta-model", DisplayName: "Beta", Visibility: "list"}},
		},
	}
	router, err := NewModelRouterWithPlan(domain.ModelProfiles{
		Routes: map[domain.ModelRole]string{
			domain.RoleChat:       "sub_a/alpha-model",
			domain.RoleBackground: "sub_a/alpha-model",
			domain.RoleAnalysis:   "sub_a/alpha-model",
		},
		Providers: map[string]domain.ModelProvider{
			"sub_a": {
				Name: "sub_a", Adapter: "openai_chatgpt", BaseURL: "https://proxy-a.invalid/v1", DiscoverModels: true,
				DefaultCapabilities: []string{"tools", "structured_output"}, DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
			},
			"sub_b": {
				Name: "sub_b", Adapter: "openai_chatgpt", BaseURL: "https://proxy-b.invalid/v1", DiscoverModels: true,
				DefaultCapabilities: []string{"tools", "structured_output"}, DefaultContextWindowTokens: 128000, DefaultMaxOutputTokens: 8192, DefaultTimeout: 30 * time.Second,
			},
		},
		Profiles: map[string]domain.ModelProfile{
			"sub_a/alpha-model": {
				Provider: "sub_a", Adapter: "openai_chatgpt", BaseURL: "https://proxy-a.invalid/v1", Model: "alpha-model",
				Capabilities: []string{"tools", "structured_output"}, ContextWindowTokens: 128000, MaxOutputTokens: 8192, Timeout: 30 * time.Second,
			},
		},
	}, sub)
	if err != nil {
		t.Fatal(err)
	}
	_, catalog, err := router.Catalog(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range catalog {
		ids[m.ID] = true
	}
	if !ids["sub_a/alpha-model"] || !ids["sub_b/beta-model"] || ids["sub_b/alpha-model"] {
		t.Fatalf("multi-provider subscription discovery mixed catalogs: %+v", catalog)
	}
}
