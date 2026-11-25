package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/telemetry"
)

type traceHistoryCapture struct {
	domain.Repository
	mu        sync.Mutex
	snapshots []domain.Trace
}

func (c *traceHistoryCapture) SaveTrace(ctx context.Context, tr domain.Trace) error {
	c.mu.Lock()
	c.snapshots = append(c.snapshots, tr)
	c.mu.Unlock()
	return c.Repository.SaveTrace(ctx, tr)
}

type yandexEmptySnippetSearch struct{}

func (yandexEmptySnippetSearch) Search(context.Context, domain.SearchQuery) (domain.SearchResponse, error) {
	return domain.SearchResponse{
		Provider: "yandex",
		Results: []domain.SearchResult{
			{Title: "Official docs", URL: "https://example.com", Snippet: "result"},
			{Title: "No passage", URL: "https://example.org", Snippet: ""},
		},
	}, nil
}
func (yandexEmptySnippetSearch) Read(context.Context, string) (string, error) {
	return "Public page", nil
}
func (yandexEmptySnippetSearch) SearchEnabled() bool    { return true }
func (yandexEmptySnippetSearch) SearchProvider() string { return "yandex" }

type spyExaSearch struct {
	queries []domain.SearchQuery
	resp    domain.SearchResponse
	err     error
}

func (s *spyExaSearch) Search(_ context.Context, q domain.SearchQuery) (domain.SearchResponse, error) {
	s.queries = append(s.queries, q)
	return s.resp, s.err
}
func (s *spyExaSearch) Read(context.Context, string) (string, error) { return "Public page", nil }
func (s *spyExaSearch) SearchEnabled() bool                          { return true }
func (s *spyExaSearch) SearchProvider() string                       { return "exa" }

func TestDisabledSearchIsHiddenAndCannotBeInvoked(t *testing.T) {
	a, _, _, conv := fixture(t)
	for _, tool := range a.toolDefinitions() {
		if tool.Name == "web_search" {
			goto configured
		}
	}
	t.Fatal("configured provider search was hidden")
configured:
	search, err := integrations.NewSearch(integrations.SearchConfig{})
	if err != nil {
		t.Fatal(err)
	}
	a.Search = search
	readerFound := false
	for _, tool := range a.toolDefinitions() {
		if tool.Name == "web_search" {
			t.Fatal("disabled search exposed to model")
		}
		if tool.Name == "web_read" {
			readerFound = true
		}
	}
	if !readerFound {
		t.Fatal("public URL reading was hidden")
	}
	out, err := a.ToolCall(context.Background(), ToolInput{RunKey: "disabled", Conversation: conv, Call: domain.ToolCall{ID: "search", Name: "web_search", Arguments: json.RawMessage(`{"query":"news"}`)}})
	if err != nil || !strings.Contains(out, "disabled") {
		t.Fatalf("stale search call should return useful error: %q %v", out, err)
	}
	var payload map[string]any
	if json.Unmarshal([]byte(out), &payload) != nil || payload["error"] == nil {
		t.Fatal("disabled search did not return an error")
	}
}

func TestExaToolDefinitionTelemetryAndTraceNoDoubleCount(t *testing.T) {
	a, repo, _, conv := fixture(t)
	cost := 0.007
	spy := &spyExaSearch{
		resp: domain.SearchResponse{
			Provider:     "exa",
			ResolvedMode: "auto",
			RequestID:    "req_123",
			CostUSD:      &cost,
			Results: []domain.SearchResult{{
				Title:         "Exa Pricing",
				URL:           "https://exa.ai/pricing",
				PublishedDate: "2026-11-01T00:00:00.000Z",
				Highlights:    []string{"Starter plan includes $10/month free credits."},
			}},
		},
	}
	a.Search = spy
	capture := &traceHistoryCapture{Repository: repo}
	a.Repo = capture
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)

	var webSearchDef *domain.Tool
	for _, d := range a.toolDefinitions() {
		if d.Name == "web_search" {
			copy := d
			webSearchDef = &copy
		}
	}
	if webSearchDef == nil || !strings.Contains(string(webSearchDef.Parameters), `"include_domains"`) || strings.Contains(string(webSearchDef.Parameters), `"deep"`) || !strings.Contains(webSearchDef.Description, "Zero Data Retention") {
		t.Fatalf("unexpected Exa tool definition: %+v", webSearchDef)
	}

	args := `{"query":"PRIVATE_QUERY_TERM","mode":"auto","num_results":5,"include_domains":["private.example"]}`
	out1 := tool(t, a, conv, "exa-run", "c1", "web_search", args)
	out2 := tool(t, a, conv, "exa-run", "c1", "web_search", args)
	if out1 != out2 || len(spy.queries) != 1 {
		t.Fatalf("expected cached replay: calls=%d out1=%s out2=%s", len(spy.queries), out1, out2)
	}
	if strings.Contains(out1, "0.007") || strings.Contains(out1, "req_123") {
		t.Fatalf("internal cost/request metadata leaked into model JSON: %s", out1)
	}
	var decoded []domain.SearchResult
	if err := json.Unmarshal([]byte(out1), &decoded); err != nil || len(decoded) != 1 || decoded[0].URL != "https://exa.ai/pricing" {
		t.Fatalf("expected JSON array tool output, got %s (err=%v)", out1, err)
	}
	if strings.Contains(logs.String(), "PRIVATE_QUERY_TERM") || strings.Contains(logs.String(), "private.example") {
		t.Fatalf("raw query or domain leaked to logs: %s", logs.String())
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log events, got %d: %s", len(lines), logs.String())
	}
	var first, second map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if first["search_provider"] != "exa" || first["search_mode"] != "auto" || first["search_cost_usd"] != float64(0.007) || first["result_count"] != float64(1) {
		t.Fatalf("missing search telemetry on live call: %v", first)
	}
	if second["cached"] != true || second["search_cost_usd"] != nil {
		t.Fatalf("cached replay must not double-count cost: %v", second)
	}
	if len(capture.snapshots) != 2 {
		t.Fatalf("expected 2 trace saves (running + completed), got %d", len(capture.snapshots))
	}
	for i, tr := range capture.snapshots {
		rawTrace, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		text := string(rawTrace)
		for _, forbidden := range []string{"PRIVATE_QUERY_TERM", "private.example", "Exa Pricing", "https://exa.ai/pricing", "Starter plan includes"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("trace snapshot %d leaked %q: %s", i, forbidden, text)
			}
		}
	}
	final := capture.snapshots[len(capture.snapshots)-1]
	if final.Search == nil || final.Search.Provider != "exa" || final.Search.HTTPStatus != 200 || final.Search.CostUSD == nil || *final.Search.CostUSD != 0.007 || final.Search.ResultCount != 1 {
		t.Fatalf("unexpected trace search metadata: %+v", final.Search)
	}
}

func TestExa402And429StructuredFallbackErrors(t *testing.T) {
	a, repo, _, conv := fixture(t)
	capture := &traceCapture{Repository: repo, traces: map[string]domain.Trace{}}
	a.Repo = capture
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)

	a.Search = &spyExaSearch{err: &integrations.HTTPError{Status: 402, Source: "exa"}}
	out402 := tool(t, a, conv, "quota-run", "c402", "web_search", `{"query":"go docs"}`)
	var p402 map[string]any
	if err := json.Unmarshal([]byte(out402), &p402); err != nil {
		t.Fatal(err)
	}
	if p402["error_code"] != "http_402" || p402["retryable"] != false || p402["fallback"] != "web_read" {
		t.Fatalf("unexpected 402 payload: %v", p402)
	}

	a.Search = &spyExaSearch{err: &integrations.HTTPError{Status: 429, Source: "exa", RetryAfter: 12 * time.Second}}
	out429 := tool(t, a, conv, "quota-run", "c429", "web_search", `{"query":"go docs"}`)
	var p429 map[string]any
	if err := json.Unmarshal([]byte(out429), &p429); err != nil {
		t.Fatal(err)
	}
	if p429["error_code"] != "http_429" || p429["retryable"] != true || p429["retry_after_seconds"] != float64(12) {
		t.Fatalf("unexpected 429 payload: %v", p429)
	}
	if !strings.Contains(logs.String(), `"http_status":402`) || !strings.Contains(logs.String(), `"http_status":429`) {
		t.Fatalf("missing HTTP status telemetry: %s", logs.String())
	}
	seen402, seen429 := false, false
	for _, tr := range capture.traces {
		if tr.Search != nil && tr.Search.HTTPStatus == 402 {
			seen402 = true
		}
		if tr.Search != nil && tr.Search.HTTPStatus == 429 {
			seen429 = true
		}
	}
	if !seen402 || !seen429 {
		t.Fatalf("expected 402 and 429 traces recorded: %+v", capture.traces)
	}
}

func TestYandexSearchPreservesArrayShapeLiveAndCachedReplay(t *testing.T) {
	a, _, _, conv := fixture(t)
	a.Search = yandexEmptySnippetSearch{}
	out1 := tool(t, a, conv, "yandex-compat", "y1", "web_search", `{"query":"go docs"}`)
	out2 := tool(t, a, conv, "yandex-compat", "y1", "web_search", `{"query":"go docs"}`)
	if out1 != out2 {
		t.Fatalf("cached replay diverged from live result: live=%s cached=%s", out1, out2)
	}
	var results []map[string]any
	if err := json.Unmarshal([]byte(out1), &results); err != nil || len(results) != 2 || results[0]["url"] != "https://example.com" {
		t.Fatalf("expected Yandex web_search to preserve JSON array wire shape, got %s (err=%v)", out1, err)
	}
	snippetVal, hasSnippet := results[1]["snippet"]
	if !hasSnippet || snippetVal != "" {
		t.Fatalf("expected Yandex empty passage to keep explicit \"snippet\":\"\", got %s", out1)
	}
}

func TestExaExplicitZeroNumResultsRejectedWithoutHTTP(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer s.Close()
	exa, err := integrations.NewExa(integrations.ExaConfig{APIKey: "secret", Endpoint: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	a, _, _, conv := fixture(t)
	a.Search = exa
	out := tool(t, a, conv, "zero-run", "z1", "web_search", `{"query":"go docs","num_results":0}`)
	if !strings.Contains(out, "num_results must be between 1 and 10") || calls.Load() != 0 {
		t.Fatalf("expected explicit num_results:0 rejection with 0 HTTP calls, got calls=%d out=%s", calls.Load(), out)
	}
	outNull := tool(t, a, conv, "zero-run", "z2", "web_search", `{"query":"go docs","num_results":null}`)
	if !strings.Contains(outNull, `"error"`) || calls.Load() != 0 {
		t.Fatalf("expected explicit num_results:null rejection with 0 HTTP calls, got calls=%d out=%s", calls.Load(), outNull)
	}
}

func TestCodingSearchSanitizesTracesAndTracksSessionlessUsage(t *testing.T) {
	a, repo, _, _ := fixture(t)
	cost := 0.007
	a.Search = &spyExaSearch{
		resp: domain.SearchResponse{
			Provider:     "exa",
			ResolvedMode: "auto",
			RequestID:    "req_coding",
			CostUSD:      &cost,
			Results: []domain.SearchResult{{
				Title:      "Secret Result Title",
				URL:        "https://secret.example/page",
				Highlights: []string{"Confidential excerpt text"},
			}},
		},
	}
	capture := &traceHistoryCapture{Repository: repo}
	a.Repo = capture

	args := json.RawMessage(`{"query":"CODING_SECRET_QUERY","include_domains":["corp.internal"]}`)
	res, err := a.ExecuteCodingTool(context.Background(), CodingToolRequest{
		SessionID: "coding-sess-1",
		TurnID:    "coding-turn-1",
		Name:      "web_search",
		Arguments: args,
	})
	if err != nil || res.Failed {
		t.Fatalf("synced coding search failed: err=%v failed=%v out=%s", err, res.Failed, res.Output)
	}
	if len(capture.snapshots) != 2 {
		t.Fatalf("expected 2 sanitized trace snapshots for synced coding search, got %d", len(capture.snapshots))
	}
	for i, tr := range capture.snapshots {
		rawTrace, _ := json.Marshal(tr)
		text := string(rawTrace)
		for _, forbidden := range []string{"CODING_SECRET_QUERY", "corp.internal", "Secret Result Title", "https://secret.example/page", "Confidential excerpt text"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("coding trace snapshot %d leaked %q: %s", i, forbidden, text)
			}
		}
	}

	// Sessionless coding search (sync_inspector=false): preserves opt-out (empty SessionID/TurnID/Input/Output)
	// while recording anonymous SearchTrace spend/count for Inspector totals.
	capture.snapshots = nil
	res, err = a.ExecuteCodingTool(context.Background(), CodingToolRequest{
		SessionID: "",
		TurnID:    "anon-turn",
		Name:      "web_search",
		Arguments: args,
	})
	if err != nil || res.Failed {
		t.Fatalf("sessionless coding search failed: err=%v failed=%v out=%s", err, res.Failed, res.Output)
	}
	if len(capture.snapshots) != 1 {
		t.Fatalf("expected 1 anonymous usage trace for sessionless coding search, got %d", len(capture.snapshots))
	}
	anon := capture.snapshots[0]
	rawAnon, _ := json.Marshal(anon)
	if anon.SessionID != "" || anon.TurnID != "" || len(anon.Input) != 0 || len(anon.Output) != 0 || anon.Trigger != "coding" || anon.Search == nil || anon.Search.CostUSD == nil || *anon.Search.CostUSD != 0.007 {
		t.Fatalf("unexpected anonymous coding usage trace: %+v (%s)", anon, string(rawAnon))
	}
	if strings.Contains(string(rawAnon), "CODING_SECRET_QUERY") || strings.Contains(string(rawAnon), "corp.internal") || strings.Contains(string(rawAnon), "Secret Result Title") {
		t.Fatalf("anonymous coding usage trace leaked content: %s", string(rawAnon))
	}
}

func TestExaRetry429To402PersistsBothOnTraceAndTelemetry(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer s.Close()
	exa, err := integrations.NewExa(integrations.ExaConfig{
		APIKey:   "key",
		Endpoint: s.URL,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	a, repo, _, conv := fixture(t)
	a.Search = exa
	capture := &traceHistoryCapture{Repository: repo}
	a.Repo = capture
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)

	out := tool(t, a, conv, "r429-402", "c1", "web_search", `{"query":"go docs"}`)
	if !strings.Contains(out, `"error_code":"http_402"`) {
		t.Fatalf("expected http_402 error payload, got %s", out)
	}
	if !strings.Contains(logs.String(), `"search_rate_limited":true`) || !strings.Contains(logs.String(), `"http_status":402`) {
		t.Fatalf("expected both search_rate_limited and http_status 402 in logs: %s", logs.String())
	}
	final := capture.snapshots[len(capture.snapshots)-1]
	if final.Search == nil || final.Search.HTTPStatus != 402 || !final.Search.RateLimited {
		t.Fatalf("expected final trace to preserve HTTP 402 and RateLimited=true, got %+v", final.Search)
	}
}

func TestModelToolModelTracesRedactWebSearchContent(t *testing.T) {
	a, repo, _, conv := fixture(t)
	cost := 0.007
	a.Search = &spyExaSearch{
		resp: domain.SearchResponse{
			Provider:     "exa",
			ResolvedMode: "auto",
			RequestID:    "req_seq",
			CostUSD:      &cost,
			Results: []domain.SearchResult{{
				Title:      "TOP_SECRET_TITLE",
				URL:        "https://secret-result.example/doc",
				Highlights: []string{"SECRET_EXCERPT_BODY"},
			}},
		},
	}
	capture := &traceHistoryCapture{Repository: repo}
	a.Repo = capture

	step := 0
	var analysisSawCodemodeReply bool
	a.Model = &testModel{respond: func(msgs []domain.Message) (domain.Message, error) {
		step++
		if step == 1 {
			return domain.Message{
				Role: "assistant",
				ToolCalls: []domain.ToolCall{
					{
						ID:        "call-search-1",
						Name:      "web_search",
						Arguments: json.RawMessage(`{"query":"MODEL_SECRET_QUERY","include_domains":["secret-domain.example"]}`),
					},
					{
						ID:        "call-cm-1",
						Name:      CodemodeName,
						Arguments: json.RawMessage(`{"code":"const t = searchTools('exa')[0].name; await tools[t]({query:'CODEMODE_SECRET_QUERY',include_domains:['cm.example']}); await tools.send_message({text:'Tuesday at noon'});"}`),
					},
				},
			}, nil
		}
		if step == 2 {
			return domain.Message{Role: "assistant", Content: "Done."}, nil
		}
		for _, m := range msgs {
			if strings.Contains(m.Content, "Tuesday at noon") {
				analysisSawCodemodeReply = true
			}
		}
		return domain.Message{Role: "assistant", Content: `{"summary":"Checked docs.","memories":[]}`}, nil
	}}

	turn := Turn{
		Input:        RunInput{Key: "seq-run", SessionID: "seq-session", ConversationID: conv.ID},
		Conversation: conv,
		Messages:     []domain.Message{{Role: "user", Content: "check docs"}},
	}

	m1, err := a.ModelCall(context.Background(), ModelInput{Turn: turn, Step: 1})
	if err != nil {
		t.Fatal(err)
	}
	searchOut := tool(t, a, conv, "seq-run", "call-search-1", "web_search", string(m1.Message.ToolCalls[0].Arguments))

	turn.Messages = append(turn.Messages,
		m1.Message,
		domain.Message{Role: "tool", ToolCallID: "call-search-1", Content: searchOut},
		domain.Message{Role: "tool", ToolCallID: "call-cm-1", Content: `{"url":"https://cm-result.example/secret","excerpt":"CODEMODE_RESULT_EXCERPT"}`},
	)
	if _, err := a.ModelCall(context.Background(), ModelInput{Turn: turn, Step: 2}); err != nil {
		t.Fatal(err)
	}

	// Also verify closed-session analysis redacts web_search payloads serialized into the analysis transcript.
	analysisMsgs := append([]domain.Message(nil), turn.Messages...)
	analysisMsgs[0].SourceMessageID = "incoming-1"
	sess := closedAnalysisSession(t, a, repo, conv.ID, analysisMsgs...)
	if err := a.Analyze(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if !analysisSawCodemodeReply {
		t.Fatal("Analyze model call lost reply text delivered through Codemode")
	}

	if len(capture.snapshots) < 8 {
		t.Fatalf("expected >=8 trace snapshots across model->tool->model->analysis, got %d", len(capture.snapshots))
	}
	forbidden := []string{
		"MODEL_SECRET_QUERY",
		"secret-domain.example",
		"CODEMODE_SECRET_QUERY",
		"cm.example",
		"TOP_SECRET_TITLE",
		"https://secret-result.example/doc",
		"SECRET_EXCERPT_BODY",
		"https://cm-result.example/secret",
		"CODEMODE_RESULT_EXCERPT",
	}
	for i, tr := range capture.snapshots {
		raw, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, needle := range forbidden {
			if strings.Contains(text, needle) {
				t.Fatalf("trace snapshot %d (%s/%s) leaked %q: %s", i, tr.Kind, tr.Status, needle, text)
			}
		}
	}

	// Verify orphaned tool result when context fitting drops the originating assistant call:
	// the analysis model still receives the fitted result intact, while the saved trace redacts it.
	fittedPayload, _ := json.Marshal(analysisContext{
		SessionTrigger: "message",
		Transcript: []domain.Message{
			{Role: "tool", ToolCallID: "call-search-1", Content: searchOut},
			{Role: "tool", ToolCallID: "call-cm-1", Content: `{"url":"https://cm-result.example/secret","excerpt":"CODEMODE_RESULT_EXCERPT"}`},
			{Role: "assistant", Content: "Done."},
		},
	})
	var modelSawOrphanedResult bool
	a.Model = &testModel{respond: func(msgs []domain.Message) (domain.Message, error) {
		for _, m := range msgs {
			if strings.Contains(m.Content, "SECRET_EXCERPT_BODY") && strings.Contains(m.Content, "https://cm-result.example/secret") {
				modelSawOrphanedResult = true
			}
		}
		return domain.Message{Role: "assistant", Content: `{"summary":"Checked docs.","memories":[]}`}, nil
	}}
	orphanTraceStart := len(capture.snapshots)
	if _, err := a.analysisModelCall(
		context.Background(),
		sess,
		[]domain.Message{
			{Role: "system", Content: "analyze"},
			{Role: "user", Content: string(fittedPayload)},
		},
		domain.CollectRedactedToolCallIDs(turn.Messages),
	); err != nil {
		t.Fatal(err)
	}
	if !modelSawOrphanedResult {
		t.Fatal("analysis model call did not receive intact fitted tool result")
	}
	if len(capture.snapshots) <= orphanTraceStart {
		t.Fatal("expected orphaned-result analysis trace snapshot")
	}
	for i := orphanTraceStart; i < len(capture.snapshots); i++ {
		raw, _ := json.Marshal(capture.snapshots[i])
		for _, needle := range forbidden {
			if strings.Contains(string(raw), needle) {
				t.Fatalf("orphaned-result analysis trace %d leaked %q: %s", i, needle, string(raw))
			}
		}
	}

	// Verify exported coding-model helpers redact both inputs and completion outputs.
	sanitizedMsgs := SanitizeTraceMessages(turn.Messages)
	rawMsgs, _ := json.Marshal(sanitizedMsgs)
	sanitizedComp := SanitizeTraceCompletion(domain.Completion{Message: m1.Message})
	rawComp, _ := json.Marshal(sanitizedComp)
	for _, needle := range forbidden {
		if strings.Contains(string(rawMsgs), needle) {
			t.Fatalf("SanitizeTraceMessages leaked %q: %s", needle, string(rawMsgs))
		}
		if strings.Contains(string(rawComp), needle) {
			t.Fatalf("SanitizeTraceCompletion leaked %q: %s", needle, string(rawComp))
		}
	}
}

func TestExaOversizedCJKResultsPreserveArrayShapeAndIntactURLs(t *testing.T) {
	a, _, _, conv := fixture(t)
	results := make([]domain.SearchResult, 10)
	for i := range results {
		results[i] = domain.SearchResult{
			Title:         "CJK Result",
			URL:           "https://docs.example.com/article/" + string(rune('a'+i)),
			PublishedDate: "2026-11-20T00:00:00.000Z",
			Highlights:    []string{strings.Repeat("中", 1100)},
		}
	}
	a.Search = &spyExaSearch{
		resp: domain.SearchResponse{
			Provider:     "exa",
			ResolvedMode: "auto",
			Results:      results,
		},
	}

	live := tool(t, a, conv, "cjk-run", "cjk1", "web_search", `{"query":"cjk docs","num_results":10}`)
	cached := tool(t, a, conv, "cjk-run", "cjk1", "web_search", `{"query":"cjk docs","num_results":10}`)
	if live != cached {
		t.Fatalf("cached replay diverged from live result")
	}
	if len(live) > 32000 {
		t.Fatalf("tool output exceeded 32000-byte limit: %d bytes", len(live))
	}
	var decoded []domain.SearchResult
	if err := json.Unmarshal([]byte(live), &decoded); err != nil {
		t.Fatalf("oversized Exa output lost JSON array shape: %v (raw=%s)", err, live[:min(200, len(live))])
	}
	if len(decoded) != 10 {
		t.Fatalf("expected all 10 results with trimmed highlights, got %d", len(decoded))
	}
	for i, item := range decoded {
		wantURL := "https://docs.example.com/article/" + string(rune('a'+i))
		if item.URL != wantURL {
			t.Fatalf("result %d URL corrupted: got %q want %q", i, item.URL, wantURL)
		}
	}
}

func TestExaRecovered429PersistsRateLimitedFlagOnTraceAndTelemetry(t *testing.T) {
	a, repo, _, conv := fixture(t)
	cost := 0.004
	a.Search = &spyExaSearch{
		resp: domain.SearchResponse{
			Provider:     "exa",
			ResolvedMode: "lite",
			RequestID:    "req_retry_ok",
			CostUSD:      &cost,
			RateLimited:  true,
			Results:      []domain.SearchResult{{Title: "OK", URL: "https://example.com", Highlights: []string{"ok"}}},
		},
	}
	capture := &traceCapture{Repository: repo, traces: map[string]domain.Trace{}}
	a.Repo = capture
	var logs bytes.Buffer
	a.Logger = telemetry.New(&logs)

	out := tool(t, a, conv, "retry-run", "r429", "web_search", `{"query":"go docs","mode":"lite"}`)
	if strings.Contains(out, "rate_limited") {
		t.Fatalf("internal rate_limited flag leaked into model JSON: %s", out)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &first); err != nil {
		t.Fatal(err)
	}
	if first["search_rate_limited"] != true || first["search_cost_usd"] != float64(0.004) {
		t.Fatalf("expected search_rate_limited telemetry, got %v", first)
	}
	if len(capture.traces) != 1 {
		t.Fatalf("expected 1 persisted trace, got %d", len(capture.traces))
	}
	for _, tr := range capture.traces {
		if tr.Search == nil || !tr.Search.RateLimited || tr.Search.HTTPStatus != 200 || tr.Search.CostUSD == nil || *tr.Search.CostUSD != 0.004 {
			t.Fatalf("expected recovered 429 trace metadata, got %+v", tr.Search)
		}
	}
}
