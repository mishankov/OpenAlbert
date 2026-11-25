package inspector

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
)

const (
	exaAutoUnitCostUSD   = 0.007
	exaLiteUnitCostUSD   = 0.004
	maxRecentSearchError = 10
)

type SearchUsageRecord struct {
	TraceID      string
	SessionID    string
	StartedAt    time.Time
	CompletedAt  time.Time
	Status       string
	Trigger      string
	TaskID       string
	Provider     string
	Mode         string
	ResolvedMode string
	CostUSD      *float64
	HTTPStatus   int
	RateLimited  bool
	ResultCount  int
}

func (r SearchUsageRecord) outcomeTime() time.Time {
	if !r.CompletedAt.IsZero() {
		return r.CompletedAt.UTC()
	}
	return r.StartedAt.UTC()
}

type SearchUsageStore interface {
	InspectSearchUsage(ctx context.Context, from, to time.Time) ([]SearchUsageRecord, error)
}

type SearchStatusSource interface {
	SearchRuntimeStatus() domain.SearchRuntimeStatus
}

type SearchBudgetSummary struct {
	MonthlyBudgetUSD     float64  `json:"monthly_budget_usd"`
	SpentUSD             float64  `json:"spent_usd"`
	RemainingUSD         float64  `json:"remaining_usd"`
	UsagePercent         float64  `json:"usage_percent"`
	Status               string   `json:"status"`
	ExhaustedBy402       bool     `json:"exhausted_by_402,omitempty"`
	ProjectedMonthEndUSD *float64 `json:"projected_month_end_usd,omitempty"`
	AvgDailyUSD          float64  `json:"avg_daily_usd"`
	EstRemainingAuto     int      `json:"est_remaining_auto"`
	EstRemainingLite     int      `json:"est_remaining_lite"`
}

type SearchTotals struct {
	TotalCalls        int     `json:"total_calls"`
	SucceededCalls    int     `json:"succeeded_calls"`
	ErrorCalls        int     `json:"error_calls"`
	Quota402Calls     int     `json:"quota_402_calls"`
	RateLimit429Calls int     `json:"rate_limit_429_calls"`
	OtherErrorCalls   int     `json:"other_error_calls"`
	AvgResultsPerCall float64 `json:"avg_results_per_call"`
}

type SearchModeBreakdown struct {
	Mode       string  `json:"mode"`
	Calls      int     `json:"calls"`
	CostUSD    float64 `json:"cost_usd"`
	AvgResults float64 `json:"avg_results"`
}

type SearchTriggerBreakdown struct {
	Trigger      string  `json:"trigger"`
	Calls        int     `json:"calls"`
	CostUSD      float64 `json:"cost_usd"`
	SharePercent float64 `json:"share_percent"`
}

type SearchDailyPoint struct {
	Date         string  `json:"date"`
	Total        int     `json:"total"`
	Auto         int     `json:"auto"`
	Lite         int     `json:"lite"`
	Other        int     `json:"other"`
	Errors       int     `json:"errors"`
	Quota402     int     `json:"quota_402"`
	RateLimit429 int     `json:"rate_limit_429"`
	CostUSD      float64 `json:"cost_usd"`
}

type SearchErrorEvent struct {
	TraceID    string    `json:"trace_id"`
	SessionID  string    `json:"session_id"`
	StartedAt  time.Time `json:"started_at"`
	Provider   string    `json:"provider"`
	Mode       string    `json:"mode,omitempty"`
	Trigger    string    `json:"trigger,omitempty"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Recovered  bool      `json:"recovered,omitempty"`
}

type SearchMonthSummary struct {
	Month      string  `json:"month"`
	TotalCalls int     `json:"total_calls"`
	SpentUSD   float64 `json:"spent_usd"`
}

type SearchLimitsReport struct {
	Runtime         domain.SearchRuntimeStatus `json:"runtime"`
	Month           string                     `json:"month"`
	PeriodStart     time.Time                  `json:"period_start"`
	PeriodEnd       time.Time                  `json:"period_end"`
	ResetAt         time.Time                  `json:"reset_at"`
	IsCurrentMonth  bool                       `json:"is_current_month"`
	AvailableMonths []string                   `json:"available_months"`
	Budget          SearchBudgetSummary        `json:"budget"`
	Totals          SearchTotals               `json:"totals"`
	ByMode          []SearchModeBreakdown      `json:"by_mode"`
	ByTrigger       []SearchTriggerBreakdown   `json:"by_trigger"`
	Daily           []SearchDailyPoint         `json:"daily"`
	RecentErrors    []SearchErrorEvent         `json:"recent_errors"`
	PreviousMonth   *SearchMonthSummary        `json:"previous_month,omitempty"`
}

// RegisterSearch exposes read-only search provider limits and monthly spend
// aggregated from persisted traces plus live adapter state.
func RegisterSearch(mux *http.ServeMux, store SearchUsageStore, statusSource any, exaMonthlyBudgetUSD float64) {
	registerSearchWithNow(mux, store, statusSource, exaMonthlyBudgetUSD, func() time.Time { return time.Now().UTC() })
}

func registerSearchWithNow(mux *http.ServeMux, store SearchUsageStore, statusSource any, exaMonthlyBudgetUSD float64, nowFn func() time.Time) {
	mux.Handle("GET /api/inspect/search/limits", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := nowFn().UTC()
		currentStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		prevStart := currentStart.AddDate(0, -1, 0)
		periodStart := currentStart

		if raw := strings.TrimSpace(r.URL.Query().Get("month")); raw != "" {
			parsed, err := time.Parse("2006-01", raw)
			if err != nil || parsed.Format("2006-01") != raw {
				fail(w, 400)
				return
			}
			periodStart = time.Date(parsed.Year(), parsed.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
		periodEnd := periodStart.AddDate(0, 1, 0)

		var runtime domain.SearchRuntimeStatus
		if src, ok := statusSource.(SearchStatusSource); ok && src != nil {
			runtime = src.SearchRuntimeStatus()
		}

		var records []SearchUsageRecord
		var prevRecords []SearchUsageRecord
		if store != nil {
			var err error
			records, err = store.InspectSearchUsage(r.Context(), periodStart, periodEnd)
			if err != nil {
				fail(w, 500)
				return
			}
			if periodStart.Equal(currentStart) {
				prevRecords, err = store.InspectSearchUsage(r.Context(), prevStart, currentStart)
				if err != nil {
					fail(w, 500)
					return
				}
			}
		}

		report := computeSearchLimitsReport(runtime, exaMonthlyBudgetUSD, records, prevRecords, periodStart, periodEnd, currentStart, prevStart, now)
		write(w, report)
	})))
}

func computeSearchLimitsReport(
	runtime domain.SearchRuntimeStatus,
	configuredExaBudgetUSD float64,
	records []SearchUsageRecord,
	prevRecords []SearchUsageRecord,
	periodStart, periodEnd, currentStart, prevStart, now time.Time,
) SearchLimitsReport {
	isCurrent := periodStart.Equal(currentStart)
	report := SearchLimitsReport{
		Runtime:         runtime,
		Month:           periodStart.Format("2006-01"),
		PeriodStart:     periodStart,
		PeriodEnd:       periodEnd,
		ResetAt:         periodEnd,
		IsCurrentMonth:  isCurrent,
		AvailableMonths: []string{currentStart.Format("2006-01"), prevStart.Format("2006-01")},
		ByMode:          []SearchModeBreakdown{},
		ByTrigger:       []SearchTriggerBreakdown{},
		Daily:           []SearchDailyPoint{},
		RecentErrors:    []SearchErrorEvent{},
	}

	budgetLimit := runtime.MonthlyBudgetUSD
	if budgetLimit <= 0 && (runtime.Provider == "exa" || hasProviderRecords(records, "exa")) {
		if configuredExaBudgetUSD > 0 {
			budgetLimit = configuredExaBudgetUSD
		} else {
			budgetLimit = 10.0
		}
	}

	type modeAcc struct {
		calls        int
		succeeded    int
		totalResults int
		costUSD      float64
	}
	modeMap := map[string]*modeAcc{
		"auto": {},
		"lite": {},
	}
	triggerOrder := []string{"message", "task", "initiative", "coding", "unknown"}
	triggerMap := map[string]*SearchTriggerBreakdown{}
	for _, tr := range triggerOrder {
		triggerMap[tr] = &SearchTriggerBreakdown{Trigger: tr}
	}
	dailyMap := map[string]*SearchDailyPoint{}

	var totalResults int
	var spentUSD float64
	var latestExa402, latestExaOK time.Time

	for _, rec := range records {
		report.Totals.TotalCalls++
		dayKey := rec.outcomeTime().Format("2006-01-02")
		day, ok := dailyMap[dayKey]
		if !ok {
			day = &SearchDailyPoint{Date: dayKey}
			dailyMap[dayKey] = day
		}
		day.Total++

		modeKey := normalizeBreakdownMode(rec.Mode, rec.ResolvedMode)
		ma, ok := modeMap[modeKey]
		if !ok {
			ma = &modeAcc{}
			modeMap[modeKey] = ma
		}
		ma.calls++

		trigKey := rec.Trigger
		if _, ok := triggerMap[trigKey]; !ok {
			trigKey = "unknown"
		}
		tb := triggerMap[trigKey]
		tb.Calls++

		isErr := rec.Status == "failed" || rec.Status == "cancelled" || (rec.HTTPStatus >= 400)
		if isErr {
			report.Totals.ErrorCalls++
			day.Errors++
			switch rec.HTTPStatus {
			case 402:
				report.Totals.Quota402Calls++
				day.Quota402++
				if outcomeAt := rec.outcomeTime(); rec.Provider == "exa" && outcomeAt.After(latestExa402) {
					latestExa402 = outcomeAt
				}
				if len(report.RecentErrors) < maxRecentSearchError {
					report.RecentErrors = append(report.RecentErrors, SearchErrorEvent{
						TraceID:    rec.TraceID,
						SessionID:  rec.SessionID,
						StartedAt:  rec.StartedAt.UTC(),
						Provider:   rec.Provider,
						Mode:       rec.Mode,
						Trigger:    trigKey,
						HTTPStatus: 402,
					})
				}
			case 429:
				report.Totals.RateLimit429Calls++
				day.RateLimit429++
				if len(report.RecentErrors) < maxRecentSearchError {
					report.RecentErrors = append(report.RecentErrors, SearchErrorEvent{
						TraceID:    rec.TraceID,
						SessionID:  rec.SessionID,
						StartedAt:  rec.StartedAt.UTC(),
						Provider:   rec.Provider,
						Mode:       rec.Mode,
						Trigger:    trigKey,
						HTTPStatus: 429,
					})
				}
			default:
				report.Totals.OtherErrorCalls++
			}
			if rec.RateLimited && rec.HTTPStatus != 429 {
				report.Totals.RateLimit429Calls++
				day.RateLimit429++
				if len(report.RecentErrors) < maxRecentSearchError {
					report.RecentErrors = append(report.RecentErrors, SearchErrorEvent{
						TraceID:    rec.TraceID,
						SessionID:  rec.SessionID,
						StartedAt:  rec.StartedAt.UTC(),
						Provider:   rec.Provider,
						Mode:       rec.Mode,
						Trigger:    trigKey,
						HTTPStatus: 429,
					})
				}
			}
		} else {
			report.Totals.SucceededCalls++
			ma.succeeded++
			ma.totalResults += rec.ResultCount
			totalResults += rec.ResultCount
			switch modeKey {
			case "auto":
				day.Auto++
			case "lite":
				day.Lite++
			default:
				day.Other++
			}
			if outcomeAt := rec.outcomeTime(); rec.Provider == "exa" && outcomeAt.After(latestExaOK) {
				latestExaOK = outcomeAt
			}
			if rec.RateLimited {
				report.Totals.RateLimit429Calls++
				day.RateLimit429++
				if len(report.RecentErrors) < maxRecentSearchError {
					report.RecentErrors = append(report.RecentErrors, SearchErrorEvent{
						TraceID:    rec.TraceID,
						SessionID:  rec.SessionID,
						StartedAt:  rec.StartedAt.UTC(),
						Provider:   rec.Provider,
						Mode:       rec.Mode,
						Trigger:    trigKey,
						HTTPStatus: 429,
						Recovered:  true,
					})
				}
			}
		}

		if rec.CostUSD != nil && !math.IsNaN(*rec.CostUSD) && !math.IsInf(*rec.CostUSD, 0) && *rec.CostUSD >= 0 {
			cost := *rec.CostUSD
			spentUSD += cost
			ma.costUSD += cost
			tb.CostUSD += cost
			day.CostUSD += cost
		}
	}

	if isCurrent && runtime.Provider == "exa" {
		if runtime.LastQuotaErrorAt != nil && !runtime.LastQuotaErrorAt.Before(periodStart) && runtime.LastQuotaErrorAt.Before(periodEnd) && runtime.LastQuotaErrorAt.After(latestExa402) {
			latestExa402 = *runtime.LastQuotaErrorAt
		}
		if runtime.LastSuccessAt != nil && !runtime.LastSuccessAt.Before(periodStart) && runtime.LastSuccessAt.Before(periodEnd) && runtime.LastSuccessAt.After(latestExaOK) {
			latestExaOK = *runtime.LastSuccessAt
		}
	}
	exhausted402 := !latestExa402.IsZero() && (latestExaOK.IsZero() || !latestExaOK.After(latestExa402))

	if report.Totals.SucceededCalls > 0 {
		report.Totals.AvgResultsPerCall = round1(float64(totalResults) / float64(report.Totals.SucceededCalls))
	}

	// Build contiguous daily series across the selected UTC month (up to today if current month).
	lastDay := periodEnd.AddDate(0, 0, -1)
	if isCurrent {
		todayUTC := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		if todayUTC.Before(lastDay) {
			lastDay = todayUTC
		}
	}
	for d := periodStart; !d.After(lastDay); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if pt, ok := dailyMap[key]; ok {
			pt.CostUSD = round4(pt.CostUSD)
			report.Daily = append(report.Daily, *pt)
		} else {
			report.Daily = append(report.Daily, SearchDailyPoint{Date: key})
		}
	}

	// Mode breakdown: auto, lite, then any additional modes sorted alphabetically.
	modeNames := []string{"auto", "lite"}
	var extraModes []string
	for name := range modeMap {
		if name != "auto" && name != "lite" {
			extraModes = append(extraModes, name)
		}
	}
	sort.Strings(extraModes)
	modeNames = append(modeNames, extraModes...)
	for _, name := range modeNames {
		ma := modeMap[name]
		avgRes := 0.0
		if ma.succeeded > 0 {
			avgRes = round1(float64(ma.totalResults) / float64(ma.succeeded))
		}
		report.ByMode = append(report.ByMode, SearchModeBreakdown{
			Mode:       name,
			Calls:      ma.calls,
			CostUSD:    round4(ma.costUSD),
			AvgResults: avgRes,
		})
	}

	for _, tr := range triggerOrder {
		tb := triggerMap[tr]
		tb.CostUSD = round4(tb.CostUSD)
		if report.Totals.TotalCalls > 0 {
			tb.SharePercent = round1(float64(tb.Calls) * 100 / float64(report.Totals.TotalCalls))
		}
		if tr == "unknown" && tb.Calls == 0 {
			continue
		}
		report.ByTrigger = append(report.ByTrigger, *tb)
	}

	// Budget computation.
	spentRounded := round4(spentUSD)
	remaining := math.Max(0, budgetLimit-spentUSD)
	usagePct := 0.0
	if budgetLimit > 0 {
		usagePct = round1(spentUSD * 100 / budgetLimit)
	}
	status := "ok"
	if !runtime.Enabled && budgetLimit == 0 {
		status = "disabled"
	} else if exhausted402 || (budgetLimit > 0 && spentUSD >= budgetLimit) {
		status = "exhausted"
	} else if budgetLimit > 0 && usagePct >= 80 {
		status = "warning"
	}

	totalMonthDays := periodEnd.Sub(periodStart).Hours() / 24
	elapsedDays := totalMonthDays
	if isCurrent {
		elapsedDays = math.Max(0, now.Sub(periodStart).Hours()/24)
	}
	avgDaily := 0.0
	if elapsedDays >= 1 {
		avgDaily = round4(spentUSD / elapsedDays)
	} else if spentUSD > 0 {
		avgDaily = round4(spentUSD)
	}
	var projected *float64
	if isCurrent && elapsedDays >= 1 && spentUSD > 0 && totalMonthDays > 0 {
		p := round2((spentUSD / elapsedDays) * totalMonthDays)
		projected = &p
	}

	estAuto := 0
	estLite := 0
	if !exhausted402 && remaining > 0 {
		estAuto = int(math.Floor(remaining / exaAutoUnitCostUSD))
		estLite = int(math.Floor(remaining / exaLiteUnitCostUSD))
	}

	report.Budget = SearchBudgetSummary{
		MonthlyBudgetUSD:     round2(budgetLimit),
		SpentUSD:             spentRounded,
		RemainingUSD:         round4(remaining),
		UsagePercent:         usagePct,
		Status:               status,
		ExhaustedBy402:       exhausted402,
		ProjectedMonthEndUSD: projected,
		AvgDailyUSD:          avgDaily,
		EstRemainingAuto:     estAuto,
		EstRemainingLite:     estLite,
	}

	if isCurrent {
		var prevSpent float64
		for _, rec := range prevRecords {
			if rec.CostUSD != nil && !math.IsNaN(*rec.CostUSD) && !math.IsInf(*rec.CostUSD, 0) && *rec.CostUSD >= 0 {
				prevSpent += *rec.CostUSD
			}
		}
		report.PreviousMonth = &SearchMonthSummary{
			Month:      prevStart.Format("2006-01"),
			TotalCalls: len(prevRecords),
			SpentUSD:   round4(prevSpent),
		}
	}

	return report
}

func hasProviderRecords(records []SearchUsageRecord, provider string) bool {
	for _, r := range records {
		if r.Provider == provider {
			return true
		}
	}
	return false
}

func normalizeBreakdownMode(mode, resolved string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "auto", "lite":
		return strings.ToLower(strings.TrimSpace(mode))
	}
	switch strings.ToLower(strings.TrimSpace(resolved)) {
	case "auto", "lite":
		return strings.ToLower(strings.TrimSpace(resolved))
	}
	return "other"
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
