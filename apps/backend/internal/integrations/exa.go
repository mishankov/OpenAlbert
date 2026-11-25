package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

const (
	defaultExaEndpoint         = "https://api.exa.ai/search"
	defaultExaMonthlyBudgetUSD = 10.0
	exaRateLimitQPS            = 10
	exaDefaultNumResults       = 5
	exaMaxNumResults           = 10
	exaMaxQueryRunes           = 1000
	exaMaxDomains              = 10
	exaMaxDomainLen            = 253
	exaHighlightMaxCharacters  = 1000
	exaHighlightClipRunes      = 1200
	exaMaxAutoRetryAfter       = 5 * time.Second
	exaBillingDashboardURL     = "https://dashboard.exa.ai/billing"
	exaUserAgent               = "OpenAlbert/0.1"
	exaResponseSizeLimitBytes  = 4 << 20
	exaMaxRequestIDLen         = 64
)

type ExaConfig struct {
	APIKey           string
	Endpoint         string
	MonthlyBudgetUSD float64
	HTTPClient       *http.Client
	Web              WebConfig
	Sleep            func(context.Context, time.Duration) error
	Now              func() time.Time
}

type Exa struct {
	config ExaConfig
	client *http.Client
	reader *WebReader
	sleep  func(context.Context, time.Duration) error
	now    func() time.Time

	mu                    sync.RWMutex
	lastSuccessAt         *time.Time
	lastQuotaErrorAt      *time.Time
	lastRateLimitAt       *time.Time
	lastRetryAfterSeconds int

	rateMu     sync.Mutex
	rateStamps []time.Time
}

type rateLimitedError struct {
	err error
}

func (e *rateLimitedError) Error() string        { return e.err.Error() }
func (e *rateLimitedError) Unwrap() error        { return e.err }
func (e *rateLimitedError) WasRateLimited() bool { return true }

func wrapRateLimited(err error, rateLimited bool) error {
	if err == nil || !rateLimited {
		return err
	}
	return &rateLimitedError{err: err}
}

func (e *Exa) waitRateLimit(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.rateMu.Lock()
		now := e.now()
		cutoff := now.Add(-time.Second)
		kept := e.rateStamps[:0]
		for _, ts := range e.rateStamps {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		e.rateStamps = kept
		if len(e.rateStamps) < exaRateLimitQPS {
			e.rateStamps = append(e.rateStamps, now)
			e.rateMu.Unlock()
			return nil
		}
		wait := e.rateStamps[0].Add(time.Second).Sub(now)
		e.rateMu.Unlock()
		if wait <= 0 {
			wait = time.Millisecond
		}
		if err := e.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

type exaSearchRequest struct {
	Query              string             `json:"query"`
	Type               string             `json:"type"`
	NumResults         int                `json:"numResults"`
	IncludeDomains     []string           `json:"includeDomains,omitempty"`
	ExcludeDomains     []string           `json:"excludeDomains,omitempty"`
	StartPublishedDate string             `json:"startPublishedDate,omitempty"`
	EndPublishedDate   string             `json:"endPublishedDate,omitempty"`
	Contents           exaContentsRequest `json:"contents"`
}

// exaContentsRequest intentionally omits summary and text so AI summary charges
// (+$1/1k pages) and oversized full-page dumps are structurally impossible.
type exaContentsRequest struct {
	Highlights exaHighlightsRequest `json:"highlights"`
}

type exaHighlightsRequest struct {
	MaxCharacters int `json:"maxCharacters"`
}

type exaSearchResponse struct {
	RequestID          string `json:"requestId"`
	ResolvedSearchType string `json:"resolvedSearchType"`
	Results            []struct {
		Title         string   `json:"title"`
		URL           string   `json:"url"`
		PublishedDate string   `json:"publishedDate"`
		Highlights    []string `json:"highlights"`
	} `json:"results"`
	CostDollars *struct {
		Total *float64 `json:"total"`
	} `json:"costDollars"`
}

func NewExa(c ExaConfig) (*Exa, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, errors.New("Exa API key is required")
	}
	if c.Endpoint == "" {
		c.Endpoint = defaultExaEndpoint
	}
	endpoint, err := providerURL(c.Endpoint)
	if err != nil {
		return nil, err
	}
	c.Endpoint = endpoint
	if c.MonthlyBudgetUSD <= 0 {
		c.MonthlyBudgetUSD = defaultExaMonthlyBudgetUSD
	}
	sleepFn := c.Sleep
	if sleepFn == nil {
		sleepFn = sleepContext
	}
	nowFn := c.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &Exa{
		config: c,
		client: httpClient(c.HTTPClient, 30*time.Second),
		reader: NewWebReader(c.Web),
		sleep:  sleepFn,
		now:    nowFn,
	}, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Exa) Read(ctx context.Context, u string) (string, error) { return e.reader.Read(ctx, u) }
func (e *Exa) SearchEnabled() bool                                { return true }
func (e *Exa) SearchProvider() string                             { return "exa" }

func (e *Exa) SearchRuntimeStatus() domain.SearchRuntimeStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	status := domain.SearchRuntimeStatus{
		Provider:              "exa",
		Enabled:               true,
		MonthlyBudgetUSD:      e.config.MonthlyBudgetUSD,
		RateLimitQPS:          exaRateLimitQPS,
		MaxResultsPerCall:     exaMaxNumResults,
		DefaultNumResults:     exaDefaultNumResults,
		AllowedModes:          []string{"auto", "lite"},
		SummaryDisabled:       true,
		BillingURL:            exaBillingDashboardURL,
		LastRetryAfterSeconds: e.lastRetryAfterSeconds,
	}
	if e.lastSuccessAt != nil {
		t := *e.lastSuccessAt
		status.LastSuccessAt = &t
	}
	if e.lastQuotaErrorAt != nil {
		t := *e.lastQuotaErrorAt
		status.LastQuotaErrorAt = &t
	}
	if e.lastRateLimitAt != nil {
		t := *e.lastRateLimitAt
		status.LastRateLimitAt = &t
	}
	return status
}

func (e *Exa) Search(ctx context.Context, q domain.SearchQuery) (domain.SearchResponse, error) {
	reqBody, err := buildExaRequest(q)
	if err != nil {
		return domain.SearchResponse{}, err
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return domain.SearchResponse{}, errors.New("invalid search request")
	}

	var out exaSearchResponse
	rateLimited := false
	for attempt := 0; attempt < 2; attempt++ {
		if err := e.waitRateLimit(ctx); err != nil {
			return domain.SearchResponse{}, wrapRateLimited(err, rateLimited)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.config.Endpoint, bytes.NewReader(payload))
		if err != nil {
			return domain.SearchResponse{}, wrapRateLimited(errors.New("invalid search request"), rateLimited)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", exaUserAgent)
		req.Header.Set("x-api-key", e.config.APIKey)

		err = request(ctx, e.client, req, &out, exaResponseSizeLimitBytes)
		if err == nil {
			now := e.now().UTC()
			e.mu.Lock()
			e.lastSuccessAt = &now
			e.mu.Unlock()
			resp := parseExaResponse(out)
			resp.RateLimited = rateLimited
			resp.CompletedAt = now
			return resp, nil
		}

		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			httpErr.Source = "exa"
			now := e.now().UTC()
			httpErr.ObservedAt = now
			if httpErr.Status == http.StatusPaymentRequired {
				e.mu.Lock()
				e.lastQuotaErrorAt = &now
				e.mu.Unlock()
				return domain.SearchResponse{}, wrapRateLimited(httpErr, rateLimited)
			}
			if httpErr.Status == http.StatusTooManyRequests {
				rateLimited = true
				retrySecs := 0
				if httpErr.RetryAfterValid && httpErr.RetryAfter > 0 {
					retrySecs = int(math.Ceil(httpErr.RetryAfter.Seconds()))
				}
				e.mu.Lock()
				e.lastRateLimitAt = &now
				e.lastRetryAfterSeconds = retrySecs
				e.mu.Unlock()
				if attempt == 0 && httpErr.RetryAfterValid && httpErr.RetryAfter <= exaMaxAutoRetryAfter {
					if sleepErr := e.sleep(ctx, httpErr.RetryAfter); sleepErr != nil {
						return domain.SearchResponse{}, wrapRateLimited(sleepErr, true)
					}
					continue
				}
				return domain.SearchResponse{}, httpErr
			}
			return domain.SearchResponse{}, wrapRateLimited(httpErr, rateLimited)
		}
		return domain.SearchResponse{}, wrapRateLimited(err, rateLimited)
	}
	return domain.SearchResponse{}, &HTTPError{Status: http.StatusTooManyRequests, Source: "exa"}
}

func buildExaRequest(q domain.SearchQuery) (exaSearchRequest, error) {
	query := strings.TrimSpace(q.Query)
	if query == "" || utf8.RuneCountInString(query) > exaMaxQueryRunes {
		return exaSearchRequest{}, errors.New("search query must contain 1 to 1000 characters")
	}
	mode := strings.TrimSpace(q.Mode)
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "lite" {
		return exaSearchRequest{}, errors.New("search mode must be auto or lite")
	}
	numResults := exaDefaultNumResults
	if q.NumResults != nil {
		numResults = *q.NumResults
	}
	if numResults < 1 || numResults > exaMaxNumResults {
		return exaSearchRequest{}, errors.New("num_results must be between 1 and 10")
	}
	includeDomains, err := validateDomains(q.IncludeDomains)
	if err != nil {
		return exaSearchRequest{}, err
	}
	excludeDomains, err := validateDomains(q.ExcludeDomains)
	if err != nil {
		return exaSearchRequest{}, err
	}
	startStr, startTime, err := normalizeSearchDate(q.StartPublishedDate, false)
	if err != nil {
		return exaSearchRequest{}, err
	}
	endStr, endTime, err := normalizeSearchDate(q.EndPublishedDate, true)
	if err != nil {
		return exaSearchRequest{}, err
	}
	if !startTime.IsZero() && !endTime.IsZero() && startTime.After(endTime) {
		return exaSearchRequest{}, errors.New("start_published_date must not be after end_published_date")
	}
	return exaSearchRequest{
		Query:              query,
		Type:               mode,
		NumResults:         numResults,
		IncludeDomains:     includeDomains,
		ExcludeDomains:     excludeDomains,
		StartPublishedDate: startStr,
		EndPublishedDate:   endStr,
		Contents: exaContentsRequest{
			Highlights: exaHighlightsRequest{
				MaxCharacters: exaHighlightMaxCharacters,
			},
		},
	}, nil
}

func validateDomains(domains []string) ([]string, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	if len(domains) > exaMaxDomains {
		return nil, errors.New("at most 10 domain filters are allowed")
	}
	out := make([]string, 0, len(domains))
	for _, raw := range domains {
		d := strings.TrimSpace(raw)
		if d == "" || len(d) > exaMaxDomainLen || strings.Contains(d, "://") || strings.ContainsAny(d, " \t\r\n@?#") || strings.IndexFunc(d, unicode.IsControl) >= 0 {
			return nil, errors.New("invalid domain filter")
		}
		out = append(out, d)
	}
	return out, nil
}

func normalizeSearchDate(raw string, endOfDay bool) (string, time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", time.Time{}, nil
	}
	if len(raw) == len("2006-01-02") {
		if d, err := time.Parse("2006-01-02", raw); err == nil {
			if endOfDay {
				t := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 999_000_000, time.UTC)
				return t.Format("2006-01-02T15:04:05.000Z"), t, nil
			}
			t := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
			return t.Format("2006-01-02T15:04:05.000Z"), t, nil
		}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", time.Time{}, errors.New("published date must be YYYY-MM-DD or RFC3339")
	}
	t = t.UTC()
	return t.Format(time.RFC3339), t, nil
}

func parseExaResponse(raw exaSearchResponse) domain.SearchResponse {
	results := make([]domain.SearchResult, 0, len(raw.Results))
	for _, r := range raw.Results {
		u := strings.TrimSpace(r.URL)
		if u == "" {
			continue
		}
		highlights := clipHighlights(r.Highlights, exaHighlightClipRunes)
		results = append(results, domain.SearchResult{
			Title:         strings.TrimSpace(r.Title),
			URL:           u,
			PublishedDate: strings.TrimSpace(r.PublishedDate),
			Highlights:    highlights,
		})
	}
	resp := domain.SearchResponse{
		Provider:     "exa",
		ResolvedMode: sanitizeExaMode(raw.ResolvedSearchType),
		RequestID:    sanitizeRequestID(raw.RequestID),
		Results:      results,
	}
	if raw.CostDollars != nil && raw.CostDollars.Total != nil {
		cost := *raw.CostDollars.Total
		if !math.IsNaN(cost) && !math.IsInf(cost, 0) && cost >= 0 {
			resp.CostUSD = &cost
		}
	}
	return resp
}

func clipHighlights(items []string, maxRunes int) []string {
	if len(items) == 0 || maxRunes <= 0 {
		return nil
	}
	remaining := maxRunes
	var out []string
	for _, item := range items {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		runes := []rune(text)
		if len(runes) <= remaining {
			out = append(out, text)
			remaining -= len(runes)
			if remaining == 0 {
				break
			}
			continue
		}
		if remaining > 1 {
			out = append(out, strings.TrimSpace(string(runes[:remaining-1]))+"…")
		}
		break
	}
	return out
}

func sanitizeExaMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto", "lite", "neural", "keyword", "fast", "deep", "deep-lite":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func sanitizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > exaMaxRequestIDLen {
		return ""
	}
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return ""
		}
	}
	return raw
}

var _ domain.Search = (*Exa)(nil)
