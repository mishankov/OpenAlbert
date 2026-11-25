package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

func intPtr(v int) *int { return &v }

func TestExaContractCostSafeDefaultsAndParsing(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var seenBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if r.Header.Get("x-api-key") != "exa-secret" {
			t.Errorf("missing x-api-key header")
		}
		if r.Header.Get("User-Agent") != "OpenAlbert/0.1" {
			t.Errorf("wrong user-agent: %q", r.Header.Get("User-Agent"))
		}
		if err := json.NewDecoder(r.Body).Decode(&seenBody); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, `{
			"requestId": "req_abc123",
			"resolvedSearchType": "auto",
			"results": [
				{
					"title": "Exa Docs",
					"url": "https://docs.exa.ai/reference/search",
					"publishedDate": "2026-11-10T00:00:00.000Z",
					"highlights": ["Highlight one.", "`+strings.Repeat("я", 1300)+`"]
				},
				{"title": "Missing URL", "url": ""}
			],
			"costDollars": {"total": 0.007}
		}`)
	}))
	defer s.Close()

	exa, err := NewExa(ExaConfig{
		APIKey:           "exa-secret",
		Endpoint:         s.URL,
		MonthlyBudgetUSD: 10,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := exa.Search(context.Background(), domain.SearchQuery{
		Query:              "Exa search pricing",
		IncludeDomains:     []string{"docs.exa.ai"},
		ExcludeDomains:     []string{"spam.example"},
		StartPublishedDate: "2026-11-01",
		EndPublishedDate:   "2026-11-25",
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenBody["type"] != "auto" || seenBody["numResults"] != float64(5) {
		t.Fatalf("unexpected defaults: %v", seenBody)
	}
	if seenBody["startPublishedDate"] != "2026-11-01T00:00:00.000Z" || seenBody["endPublishedDate"] != "2026-11-25T23:59:59.999Z" {
		t.Fatalf("dates not normalized: %v", seenBody)
	}
	contents, ok := seenBody["contents"].(map[string]any)
	if !ok {
		t.Fatalf("missing contents: %v", seenBody)
	}
	if _, hasSummary := contents["summary"]; hasSummary {
		t.Fatal("summary must never be requested")
	}
	if _, hasText := contents["text"]; hasText {
		t.Fatal("full text must never be requested")
	}
	highlights, ok := contents["highlights"].(map[string]any)
	if !ok || highlights["maxCharacters"] != float64(1000) {
		t.Fatalf("unexpected highlights config: %v", contents)
	}
	if resp.Provider != "exa" || resp.ResolvedMode != "auto" || resp.RequestID != "req_abc123" {
		t.Fatalf("unexpected response metadata: %+v", resp)
	}
	if resp.CostUSD == nil || *resp.CostUSD != 0.007 {
		t.Fatalf("unexpected cost: %+v", resp.CostUSD)
	}
	if len(resp.Results) != 1 || resp.Results[0].URL != "https://docs.exa.ai/reference/search" {
		t.Fatalf("unexpected results: %+v", resp.Results)
	}
	totalRunes := 0
	for _, h := range resp.Results[0].Highlights {
		totalRunes += len([]rune(h))
	}
	if totalRunes > exaHighlightClipRunes {
		t.Fatalf("highlights were not clipped: %d runes", totalRunes)
	}
	status := exa.SearchRuntimeStatus()
	if !status.Enabled || status.Provider != "exa" || status.LastSuccessAt == nil || !status.LastSuccessAt.Equal(now) {
		t.Fatalf("runtime status not updated: %+v", status)
	}
}

func TestExaRejectsCostTrapsAndInvalidInputsWithoutHTTP(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer s.Close()
	exa, err := NewExa(ExaConfig{APIKey: "secret", Endpoint: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	bad := []domain.SearchQuery{
		{Query: "   "},
		{Query: strings.Repeat("a", 1001)},
		{Query: "ok", Mode: "deep"},
		{Query: "ok", Mode: "deep-lite"},
		{Query: "ok", Mode: "invalid"},
		{Query: "ok", NumResults: intPtr(0)},
		{Query: "ok", NumResults: intPtr(-1)},
		{Query: "ok", NumResults: intPtr(11)},
		{Query: "ok", IncludeDomains: make([]string, 11)},
		{Query: "ok", IncludeDomains: []string{"https://example.com"}},
		{Query: "ok", ExcludeDomains: []string{"bad host"}},
		{Query: "ok", StartPublishedDate: "not-a-date"},
		{Query: "ok", StartPublishedDate: "2026-11-25", EndPublishedDate: "2026-11-01"},
	}
	for i, q := range bad {
		if _, err := exa.Search(context.Background(), q); err == nil {
			t.Errorf("case %d accepted invalid query: %+v", i, q)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid queries reached network: %d calls", calls.Load())
	}
}

func TestExa402NeverRetriesAndHidesSecretBody(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, "API_KEY_BUDGET_EXCEEDED SECRET_BODY "+r.Header.Get("x-api-key"))
	}))
	defer s.Close()
	exa, err := NewExa(ExaConfig{APIKey: "EXA_SUPER_SECRET", Endpoint: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = exa.Search(context.Background(), domain.SearchQuery{Query: "news"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 402 {
		t.Fatalf("expected HTTP 402 error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("402 must not retry, got %d calls", calls.Load())
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), s.URL) {
		t.Fatalf("unsafe 402 error: %v", err)
	}
	status := exa.SearchRuntimeStatus()
	if status.LastQuotaErrorAt == nil {
		t.Fatal("402 did not update runtime quota status")
	}
}

func TestExa429BoundedSingleRetryAndLongRetryAfter(t *testing.T) {
	t.Run("recovers on second attempt when Retry-After <= 5s", func(t *testing.T) {
		var calls atomic.Int32
		var slept []time.Duration
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"requestId":"r2","resolvedSearchType":"lite","results":[{"title":"OK","url":"https://example.com","highlights":["ok"]}],"costDollars":{"total":0.004}}`)
		}))
		defer s.Close()
		exa, err := NewExa(ExaConfig{
			APIKey:   "key",
			Endpoint: s.URL,
			Sleep: func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := exa.Search(context.Background(), domain.SearchQuery{Query: "test", Mode: "lite", NumResults: intPtr(3)})
		if err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 || len(slept) != 1 || slept[0] != 2*time.Second || out.ResolvedMode != "lite" || !out.RateLimited {
			t.Fatalf("unexpected retry behavior: calls=%d slept=%v out=%+v", calls.Load(), slept, out)
		}
		if exa.SearchRuntimeStatus().LastRateLimitAt == nil {
			t.Fatalf("expected runtime status to record recovered 429 timestamp: %+v", exa.SearchRuntimeStatus())
		}
	})

	t.Run("does not sleep when Retry-After > 5s", func(t *testing.T) {
		var calls atomic.Int32
		slept := false
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer s.Close()
		exa, err := NewExa(ExaConfig{
			APIKey:   "key",
			Endpoint: s.URL,
			Sleep: func(_ context.Context, _ time.Duration) error {
				slept = true
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = exa.Search(context.Background(), domain.SearchQuery{Query: "test"})
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != 429 || httpErr.RetryAfter != 30*time.Second {
			t.Fatalf("expected HTTP 429 with RetryAfter=30s, got %v", err)
		}
		if slept || calls.Load() != 1 {
			t.Fatalf("long Retry-After should not sleep or retry: slept=%v calls=%d", slept, calls.Load())
		}
		if exa.SearchRuntimeStatus().LastRetryAfterSeconds != 30 {
			t.Fatalf("runtime status missing retry-after: %+v", exa.SearchRuntimeStatus())
		}
	})

	t.Run("preserves long Retry-After without clamping and does not retry malformed header", func(t *testing.T) {
		headers := []string{"3600", "not-a-delay", "", "+1", "-0"}
		var idx atomic.Int32
		var calls atomic.Int32
		slept := false
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			h := headers[int(idx.Load())]
			w.Header()["Retry-After"] = []string{h}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer s.Close()
		exa, err := NewExa(ExaConfig{
			APIKey:   "key",
			Endpoint: s.URL,
			Sleep: func(_ context.Context, _ time.Duration) error {
				slept = true
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		idx.Store(0)
		_, err = exa.Search(context.Background(), domain.SearchQuery{Query: "long"})
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.RetryAfter != 3600*time.Second || !httpErr.RetryAfterValid {
			t.Fatalf("expected preserved 3600s Retry-After, got %+v (%v)", httpErr, err)
		}
		if slept || calls.Load() != 1 {
			t.Fatalf("3600s Retry-After must not retry locally: slept=%v calls=%d", slept, calls.Load())
		}

		for i := 1; i < len(headers); i++ {
			idx.Store(int32(i))
			before := calls.Load()
			_, err = exa.Search(context.Background(), domain.SearchQuery{Query: "malformed"})
			httpErr = nil
			if !errors.As(err, &httpErr) || !httpErr.RetryAfterPresent || httpErr.RetryAfterValid || httpErr.RetryAfter != 0 {
				t.Fatalf("header %q: expected malformed Retry-After metadata, got %+v (%v)", headers[i], httpErr, err)
			}
			if slept || calls.Load()-before != 1 {
				t.Fatalf("header %q: malformed Retry-After must not trigger retry: slept=%v attempts=%d", headers[i], slept, calls.Load()-before)
			}
		}
	})

	t.Run("does not retry 429 when Retry-After header is absent", func(t *testing.T) {
		var calls atomic.Int32
		slept := false
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer s.Close()
		exa, err := NewExa(ExaConfig{
			APIKey:   "key",
			Endpoint: s.URL,
			Sleep: func(_ context.Context, _ time.Duration) error {
				slept = true
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = exa.Search(context.Background(), domain.SearchQuery{Query: "absent"})
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != 429 || httpErr.RetryAfterPresent || httpErr.RetryAfterValid {
			t.Fatalf("expected 429 with absent Retry-After, got %+v (%v)", httpErr, err)
		}
		if slept || calls.Load() != 1 {
			t.Fatalf("absent Retry-After must not trigger retry: slept=%v calls=%d", slept, calls.Load())
		}
	})

	t.Run("carries rate-limited flag when retry fails with 402", func(t *testing.T) {
		var calls atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusPaymentRequired)
		}))
		defer s.Close()
		exa, err := NewExa(ExaConfig{
			APIKey:   "key",
			Endpoint: s.URL,
			Sleep:    func(_ context.Context, _ time.Duration) error { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = exa.Search(context.Background(), domain.SearchQuery{Query: "test"})
		var httpErr *HTTPError
		var rl interface{ WasRateLimited() bool }
		if !errors.As(err, &httpErr) || httpErr.Status != 402 || !errors.As(err, &rl) || !rl.WasRateLimited() {
			t.Fatalf("expected 402 carrying WasRateLimited=true, got %v", err)
		}
	})
}

func TestExaRateLimiterEnforces10QPSAcrossCallersAndRetries(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 10 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"requestId":"ok","resolvedSearchType":"auto","results":[{"title":"OK","url":"https://example.com","highlights":["ok"]}],"costDollars":{"total":0.007}}`)
	}))
	defer s.Close()

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var sleeps int
	var attemptTimes []time.Time
	exa, err := NewExa(ExaConfig{
		APIKey:   "key",
		Endpoint: s.URL,
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			clockMu.Lock()
			sleeps++
			now = now.Add(d)
			clockMu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Run 11 overlapping searches where one also triggers a 429 retry (12 outbound attempts total).
	var wg sync.WaitGroup
	errs := make(chan error, 11)
	for i := 0; i < 11; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := exa.Search(context.Background(), domain.SearchQuery{Query: "concurrent"})
			clockMu.Lock()
			attemptTimes = append(attemptTimes, now)
			clockMu.Unlock()
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected concurrent search error: %v", err)
		}
	}
	if calls.Load() != 12 {
		t.Fatalf("expected 12 outbound attempts (11 calls + 1 retry), got %d", calls.Load())
	}
	if sleeps < 2 {
		t.Fatalf("expected limiter sleeps for attempts beyond 10 QPS, got %d sleeps", sleeps)
	}
	// Context cancellation while rate-limited aborts before hitting network.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exa.Search(cancelCtx, domain.SearchQuery{Query: "canceled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(attemptTimes) != 11 {
		t.Fatalf("expected 11 completed searches, got %d", len(attemptTimes))
	}
}

func TestNewSearchProviderSelection(t *testing.T) {
	exaOnly, err := NewSearch(SearchConfig{Exa: ExaConfig{APIKey: "exa-key"}})
	if err != nil || exaOnly.(interface{ SearchProvider() string }).SearchProvider() != "exa" {
		t.Fatalf("exa-only failed: %v", err)
	}
	yandexOnly, err := NewSearch(SearchConfig{Yandex: YandexConfig{APIKey: "yk", FolderID: "yf"}})
	if err != nil || yandexOnly.(interface{ SearchProvider() string }).SearchProvider() != "yandex" {
		t.Fatalf("yandex-only failed: %v", err)
	}
	if _, err := NewSearch(SearchConfig{
		Exa:    ExaConfig{APIKey: "exa-key"},
		Yandex: YandexConfig{APIKey: "yk", FolderID: "yf"},
	}); err == nil {
		t.Fatal("ambiguous dual-provider config must fail")
	}
	chosen, err := NewSearch(SearchConfig{
		Provider: "exa",
		Exa:      ExaConfig{APIKey: "exa-key"},
		Yandex:   YandexConfig{APIKey: "yk", FolderID: "yf"},
	})
	if err != nil || chosen.(interface{ SearchProvider() string }).SearchProvider() != "exa" {
		t.Fatalf("explicit provider override failed: %v", err)
	}
}
