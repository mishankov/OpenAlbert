package inspector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
)

type fakeSearchUsageStore struct {
	records []SearchUsageRecord
}

func (f fakeSearchUsageStore) InspectSearchUsage(_ context.Context, from, to time.Time) ([]SearchUsageRecord, error) {
	out := []SearchUsageRecord{}
	for _, r := range f.records {
		if !r.StartedAt.Before(from) && r.StartedAt.Before(to) {
			out = append(out, r)
		}
	}
	return out, nil
}

type fakeSearchStatus struct{ status domain.SearchRuntimeStatus }

func (f fakeSearchStatus) SearchRuntimeStatus() domain.SearchRuntimeStatus { return f.status }

func floatPtr(v float64) *float64 { return &v }

func TestInspectorSearchLimitsReportAndReadOnlyPolicy(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	store := fakeSearchUsageStore{
		records: []SearchUsageRecord{
			{TraceID: "t3", SessionID: "s1", StartedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC), Status: "failed", Trigger: "message", Provider: "exa", Mode: "auto", HTTPStatus: 429},
			{TraceID: "t2", SessionID: "s1", StartedAt: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC), Status: "completed", Trigger: "task", Provider: "exa", Mode: "lite", ResolvedMode: "lite", CostUSD: floatPtr(0.004), HTTPStatus: 200, ResultCount: 5},
			{TraceID: "t1", SessionID: "s1", StartedAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), Status: "completed", Trigger: "message", Provider: "exa", Mode: "auto", ResolvedMode: "auto", CostUSD: floatPtr(8.5), HTTPStatus: 200, ResultCount: 7},
			{TraceID: "p1", SessionID: "s0", StartedAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC), Status: "completed", Trigger: "message", Provider: "exa", Mode: "auto", ResolvedMode: "auto", CostUSD: floatPtr(2.1), HTTPStatus: 200, ResultCount: 5},
		},
	}
	runtimeStatus := fakeSearchStatus{
		status: domain.SearchRuntimeStatus{
			Provider:          "exa",
			Enabled:           true,
			MonthlyBudgetUSD:  10,
			RateLimitQPS:      10,
			MaxResultsPerCall: 10,
			DefaultNumResults: 5,
			AllowedModes:      []string{"auto", "lite"},
			SummaryDisabled:   true,
			BillingURL:        "https://dashboard.exa.ai/billing",
		},
	}
	mux := http.NewServeMux()
	registerSearchWithNow(mux, store, runtimeStatus, 10, func() time.Time { return now })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/inspect/search/limits", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
	var report SearchLimitsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Month != "2026-11" || !report.IsCurrentMonth || report.Budget.Status != "warning" {
		t.Fatalf("unexpected report header/status: %+v", report.Budget)
	}
	if report.Budget.SpentUSD != 8.504 || report.Totals.TotalCalls != 3 || report.Totals.RateLimit429Calls != 1 {
		t.Fatalf("unexpected totals: budget=%+v totals=%+v", report.Budget, report.Totals)
	}
	if report.Budget.ProjectedMonthEndUSD == nil || *report.Budget.ProjectedMonthEndUSD <= 10 {
		t.Fatalf("expected projection above $10: %+v", report.Budget.ProjectedMonthEndUSD)
	}
	if report.PreviousMonth == nil || report.PreviousMonth.Month != "2026-10" || report.PreviousMonth.SpentUSD != 2.1 {
		t.Fatalf("unexpected previous month: %+v", report.PreviousMonth)
	}
	if len(report.RecentErrors) != 1 || report.RecentErrors[0].TraceID != "t3" || report.RecentErrors[0].HTTPStatus != 429 {
		t.Fatalf("unexpected recent errors: %+v", report.RecentErrors)
	}
	if len(report.Daily) != 15 {
		t.Fatalf("expected 15 contiguous days Nov 1..15, got %d", len(report.Daily))
	}

	// Previous month query
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/inspect/search/limits?month=2026-10", nil))
	if rec.Code != 200 {
		t.Fatalf("previous month failed: %d", rec.Code)
	}
	var prev SearchLimitsReport
	_ = json.Unmarshal(rec.Body.Bytes(), &prev)
	if prev.Month != "2026-10" || prev.IsCurrentMonth || prev.Totals.TotalCalls != 1 || len(prev.Daily) != 31 {
		t.Fatalf("unexpected previous month report: %+v", prev)
	}

	// Bad month & non-GET method
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/inspect/search/limits?month=bad", nil))
	if rec.Code != 400 {
		t.Fatalf("bad month accepted: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "http://localhost/api/inspect/search/limits", nil))
	if rec.Code != 405 {
		t.Fatalf("POST accepted: %d", rec.Code)
	}
}

func TestInspectorSearch402ExhaustionAndRecovery(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	t402 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	tOK := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	rt := domain.SearchRuntimeStatus{Provider: "exa", Enabled: true, MonthlyBudgetUSD: 10}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	prev := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	exhausted := computeSearchLimitsReport(rt, 10, []SearchUsageRecord{
		{TraceID: "e1", SessionID: "s1", StartedAt: t402, Status: "failed", Provider: "exa", Mode: "auto", HTTPStatus: 402},
	}, nil, start, end, start, prev, now)
	if exhausted.Budget.Status != "exhausted" || !exhausted.Budget.ExhaustedBy402 || exhausted.Budget.EstRemainingAuto != 0 {
		t.Fatalf("402 should mark budget exhausted: %+v", exhausted.Budget)
	}

	recovered := computeSearchLimitsReport(rt, 10, []SearchUsageRecord{
		{TraceID: "ok1", SessionID: "s1", StartedAt: tOK, Status: "completed", Provider: "exa", Mode: "auto", CostUSD: floatPtr(0.007), HTTPStatus: 200},
		{TraceID: "e1", SessionID: "s1", StartedAt: t402, Status: "failed", Provider: "exa", Mode: "auto", HTTPStatus: 402},
	}, nil, start, end, start, prev, now)
	if recovered.Budget.ExhaustedBy402 || recovered.Budget.Status != "ok" {
		t.Fatalf("later 200 should clear 402 exhaustion state: %+v", recovered.Budget)
	}

	// R3 #4: Overlapping calls with reversed completion order must follow CompletedAt, not StartedAt
	// Search A starts at 10:00:00 and succeeds at 10:00:03; Search B starts at 10:00:01 and fails 402 at 10:00:02.
	overlapRecovered := computeSearchLimitsReport(rt, 10, []SearchUsageRecord{
		{
			TraceID: "b-402", SessionID: "s1",
			StartedAt:   time.Date(2026, 11, 20, 10, 0, 1, 0, time.UTC),
			CompletedAt: time.Date(2026, 11, 20, 10, 0, 2, 0, time.UTC),
			Status:      "failed", Provider: "exa", Mode: "auto", HTTPStatus: 402,
		},
		{
			TraceID: "a-ok", SessionID: "s1",
			StartedAt:   time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
			CompletedAt: time.Date(2026, 11, 20, 10, 0, 3, 0, time.UTC),
			Status:      "completed", Provider: "exa", Mode: "auto", CostUSD: floatPtr(0.007), HTTPStatus: 200,
		},
	}, nil, start, end, start, prev, now)
	if overlapRecovered.Budget.ExhaustedBy402 || overlapRecovered.Budget.Status != "ok" {
		t.Fatalf("overlapping call completing 200 after 402 should remain recovered after restart: %+v", overlapRecovered.Budget)
	}
}

func TestInspectorSearchRecovered429LimitSlotPriorityAndExactDailyModes(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	prev := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rt := domain.SearchRuntimeStatus{Provider: "exa", Enabled: true, MonthlyBudgetUSD: 10}

	// S2: recovered 429->200 retry counts call & cost once, increments 429 count, and exposes trace link
	rec429 := computeSearchLimitsReport(rt, 10, []SearchUsageRecord{
		{
			TraceID: "t-retry-ok", SessionID: "s1",
			StartedAt: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC),
			Status:    "completed", Trigger: "message", Provider: "exa",
			Mode: "lite", ResolvedMode: "lite", CostUSD: floatPtr(0.004),
			HTTPStatus: 200, RateLimited: true, ResultCount: 4,
		},
	}, nil, start, end, start, prev, now)
	if rec429.Totals.TotalCalls != 1 || rec429.Totals.SucceededCalls != 1 || rec429.Totals.ErrorCalls != 0 || rec429.Totals.RateLimit429Calls != 1 || rec429.Budget.SpentUSD != 0.004 {
		t.Fatalf("recovered 429 totals wrong: totals=%+v budget=%+v", rec429.Totals, rec429.Budget)
	}
	if len(rec429.RecentErrors) != 1 || rec429.RecentErrors[0].TraceID != "t-retry-ok" || rec429.RecentErrors[0].HTTPStatus != 429 || !rec429.RecentErrors[0].Recovered {
		t.Fatalf("recovered 429 missing from recent limit events: %+v", rec429.RecentErrors)
	}

	// R2 #5: 429->402 retry failure counts call once, increments BOTH 402 and 429 counters, and surfaces both events
	retry429To402 := computeSearchLimitsReport(rt, 10, []SearchUsageRecord{
		{
			TraceID: "t-429-402", SessionID: "s1",
			StartedAt: time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC),
			Status:    "failed", Trigger: "message", Provider: "exa",
			Mode: "auto", HTTPStatus: 402, RateLimited: true,
		},
	}, nil, start, end, start, prev, now)
	if retry429To402.Totals.TotalCalls != 1 || retry429To402.Totals.ErrorCalls != 1 || retry429To402.Totals.Quota402Calls != 1 || retry429To402.Totals.RateLimit429Calls != 1 {
		t.Fatalf("429->402 totals wrong: %+v", retry429To402.Totals)
	}
	if len(retry429To402.RecentErrors) != 2 || retry429To402.RecentErrors[0].HTTPStatus != 402 || retry429To402.RecentErrors[1].HTTPStatus != 429 || retry429To402.RecentErrors[1].Recovered {
		t.Fatalf("429->402 recent events wrong: %+v", retry429To402.RecentErrors)
	}

	// S3: 12 newer HTTP 500 errors must not evict an older HTTP 402 from RecentErrors
	records := make([]SearchUsageRecord, 0, 13)
	for i := 0; i < 12; i++ {
		records = append(records, SearchUsageRecord{
			TraceID:   "err-500",
			SessionID: "s1",
			StartedAt: time.Date(2026, 11, 19, 11, i, 0, 0, time.UTC),
			Status:    "failed", Provider: "exa", Mode: "auto", HTTPStatus: 500,
		})
	}
	records = append(records, SearchUsageRecord{
		TraceID: "err-402", SessionID: "s1",
		StartedAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		Status:    "failed", Provider: "exa", Mode: "auto", HTTPStatus: 402,
	})
	limitSlots := computeSearchLimitsReport(rt, 10, records, nil, start, end, start, prev, now)
	if limitSlots.Totals.OtherErrorCalls != 12 || limitSlots.Totals.Quota402Calls != 1 {
		t.Fatalf("unexpected error aggregate counts: %+v", limitSlots.Totals)
	}
	if len(limitSlots.RecentErrors) != 1 || limitSlots.RecentErrors[0].TraceID != "err-402" || limitSlots.RecentErrors[0].HTTPStatus != 402 {
		t.Fatalf("non-limit 500 errors evicted 402 event: %+v", limitSlots.RecentErrors)
	}

	// S4: 1 succeeded auto + 1 failed lite on same day => day.Auto=1, day.Lite=0, day.Errors=1
	dailyReport := computeSearchLimitsReport(rt, 10, []SearchUsageRecord{
		{TraceID: "d-auto", SessionID: "s1", StartedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC), Status: "completed", Provider: "exa", Mode: "auto", ResolvedMode: "auto", CostUSD: floatPtr(0.007), HTTPStatus: 200},
		{TraceID: "d-lite-err", SessionID: "s1", StartedAt: time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC), Status: "failed", Provider: "exa", Mode: "lite", HTTPStatus: 500},
	}, nil, start, end, start, prev, now)
	day15 := dailyReport.Daily[14]
	if day15.Date != "2026-11-15" || day15.Total != 2 || day15.Auto != 1 || day15.Lite != 0 || day15.Errors != 1 {
		t.Fatalf("daily mode buckets attributed error to wrong mode: %+v", day15)
	}

	// R2 #10: Switching away from Exa preserves configured EXA_MONTHLY_BUDGET_USD for historical Exa spend
	switchedReport := computeSearchLimitsReport(
		domain.SearchRuntimeStatus{Provider: "yandex", Enabled: true},
		25,
		[]SearchUsageRecord{
			{TraceID: "exa-hist", SessionID: "s1", StartedAt: time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC), Status: "completed", Provider: "exa", Mode: "auto", ResolvedMode: "auto", CostUSD: floatPtr(12.0), HTTPStatus: 200},
		},
		nil, start, end, start, prev, now,
	)
	if switchedReport.Budget.MonthlyBudgetUSD != 25 || switchedReport.Budget.Status != "ok" || switchedReport.Budget.RemainingUSD != 13 {
		t.Fatalf("switched provider dropped configured Exa budget: %+v", switchedReport.Budget)
	}
}
