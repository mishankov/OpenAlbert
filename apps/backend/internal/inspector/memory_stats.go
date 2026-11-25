package inspector

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
)

// UserProfileCharLimit matches clip(profile, 8000) in agent Prepare() and coding context.
const UserProfileCharLimit = 8000

const (
	maxDailyWindowDays = 90
	maxTopGenerators   = 15
)

var canonicalMemoryKinds = []string{"profile", "preference", "fact", "episode", "hypothesis"}

type SessionOrigin struct {
	SessionID string
	Trigger   string
	TaskID    string
	TaskTitle string
	CreatedAt time.Time
}

// MemoryMetadataStore is optionally implemented by ConversationStore backends
// backed by Postgres to attach session triggers, creation timestamps, task
// metadata, and consolidation receipts to memory analytics without widening
// the read-only Inspector boundary.
type MemoryMetadataStore interface {
	InspectSessionOrigins(ctx context.Context, scope string, sessionIDs []string) (map[string]SessionOrigin, error)
	InspectLatestConsolidation(ctx context.Context, scope string) (string, json.RawMessage, bool, error)
}

type TriggerCounts struct {
	Message    int `json:"message"`
	Task       int `json:"task"`
	Initiative int `json:"initiative"`
	Coding     int `json:"coding"`
	Unknown    int `json:"unknown"`
}

func (c *TriggerCounts) add(trigger string) {
	switch trigger {
	case "message":
		c.Message++
	case "task":
		c.Task++
	case "initiative":
		c.Initiative++
	case "coding":
		c.Coding++
	default:
		c.Unknown++
	}
}

type MemoryTrend struct {
	Last7Days    int      `json:"last_7_days"`
	Prev7Days    int      `json:"prev_7_days"`
	Delta        int      `json:"delta"`
	DeltaPercent *float64 `json:"delta_percent,omitempty"`
	AvgPerDay    float64  `json:"avg_per_day"`
}

type DailyGrowthPoint struct {
	Date  string `json:"date"`
	Total int    `json:"total"`
	TriggerCounts
}

type TruncatedProfileEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	Chars     int       `json:"chars"`
	StartChar int       `json:"start_char"`
	EndChar   int       `json:"end_char"`
	Partial   bool      `json:"partial"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type ProfileBudget struct {
	Limit            int                     `json:"limit"`
	TotalChars       int                     `json:"total_chars"`
	IncludedChars    int                     `json:"included_chars"`
	TruncatedChars   int                     `json:"truncated_chars"`
	UsagePercent     float64                 `json:"usage_percent"`
	Status           string                  `json:"status"`
	TotalEntries     int                     `json:"total_entries"`
	IncludedEntries  int                     `json:"included_entries"`
	TruncatedEntries int                     `json:"truncated_entries"`
	TruncatedItems   []TruncatedProfileEntry `json:"truncated_items"`
}

type KindBreakdown struct {
	Kind  string `json:"kind"`
	Total int    `json:"total"`
	Chars int    `json:"chars"`
	TriggerCounts
}

type BackgroundGenerator struct {
	Trigger  string     `json:"trigger"`
	TaskID   string     `json:"task_id,omitempty"`
	Title    string     `json:"title,omitempty"`
	Count    int        `json:"count"`
	Chars    int        `json:"chars"`
	LatestAt *time.Time `json:"latest_at,omitempty"`
}

type ConsolidationOpCounts struct {
	Merged       int `json:"merged"`
	Reclassified int `json:"reclassified"`
	Deleted      int `json:"deleted"`
	Kept         int `json:"kept"`
	Updated      int `json:"updated"`
	Skipped      int `json:"skipped"`
}

type ConsolidationSummary struct {
	ID                 string                `json:"id"`
	CompletedAt        *time.Time            `json:"completed_at,omitempty"`
	BeforeEntries      int                   `json:"before_entries"`
	AfterEntries       int                   `json:"after_entries"`
	BeforeProfileChars int                   `json:"before_profile_chars"`
	AfterProfileChars  int                   `json:"after_profile_chars"`
	ProfileCharsDelta  int                   `json:"profile_chars_delta"`
	Operations         ConsolidationOpCounts `json:"operations"`
	Log                json.RawMessage       `json:"log,omitempty"`
}

type MemoryStats struct {
	Scope                  string                `json:"scope"`
	TotalEntries           int                   `json:"total_entries"`
	TotalChars             int                   `json:"total_chars"`
	TotalBytes             int                   `json:"total_bytes"`
	OldestAt               *time.Time            `json:"oldest_at,omitempty"`
	NewestAt               *time.Time            `json:"newest_at,omitempty"`
	SpanDays               int                   `json:"span_days"`
	FallbackDateEntries    int                   `json:"fallback_date_entries,omitempty"`
	Triggers               TriggerCounts         `json:"triggers"`
	BackgroundEntries      int                   `json:"background_entries"`
	BackgroundSharePercent float64               `json:"background_share_percent"`
	Trend                  MemoryTrend           `json:"trend"`
	DailyGrowth            []DailyGrowthPoint    `json:"daily_growth"`
	DailyGrowthWindowDays  int                   `json:"daily_growth_window_days,omitempty"`
	ProfileBudget          ProfileBudget         `json:"profile_budget"`
	Kinds                  []KindBreakdown       `json:"kinds"`
	TopGenerators          []BackgroundGenerator `json:"top_generators"`
	LatestConsolidation    *ConsolidationSummary `json:"latest_consolidation,omitempty"`
}

// originSessionID extracts the originating session identifier from a memory entry's Source.
// Analyze() appends later session IDs separated by "; " and legacy records may prefix "session:".
func originSessionID(source string) string {
	for _, part := range strings.Split(source, ";") {
		candidate := strings.TrimSpace(part)
		if candidate == "" {
			continue
		}
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, "session:"))
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func entryOriginSessionID(e domain.MemoryEntry) string {
	if id := originSessionID(e.OriginSource); id != "" {
		return id
	}
	return originSessionID(e.Source)
}

func uniqueOriginSessionIDs(entries []domain.MemoryEntry) []string {
	seen := make(map[string]struct{}, len(entries))
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		id := entryOriginSessionID(e)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// entryAddedAt returns a stable creation timestamp for daily additions and 7-day
// growth. Prefer the entry's persisted CreatedAt, then the originating session's
// CreatedAt for legacy entries, falling back to UpdatedAt only when neither exists.
func entryAddedAt(e domain.MemoryEntry, origin SessionOrigin, hasOrigin bool) (ts time.Time, fallback bool) {
	if !e.CreatedAt.IsZero() {
		return e.CreatedAt.UTC(), false
	}
	if hasOrigin && !origin.CreatedAt.IsZero() {
		return origin.CreatedAt.UTC(), false
	}
	if !e.UpdatedAt.IsZero() {
		return e.UpdatedAt.UTC(), true
	}
	return time.Time{}, false
}

func computeMemoryStats(
	scope string,
	entries []domain.MemoryEntry,
	profile string,
	origins map[string]SessionOrigin,
	consolidation *ConsolidationSummary,
	now time.Time,
) MemoryStats {
	stats := MemoryStats{
		Scope:               scope,
		TotalEntries:        len(entries),
		DailyGrowth:         []DailyGrowthPoint{},
		ProfileBudget:       computeProfileBudget(entries, profile, UserProfileCharLimit),
		TopGenerators:       []BackgroundGenerator{},
		LatestConsolidation: consolidation,
	}

	kindMap := make(map[string]*KindBreakdown, len(canonicalMemoryKinds))
	for _, k := range canonicalMemoryKinds {
		kindMap[k] = &KindBreakdown{Kind: k}
	}
	var extraKinds []string

	dailyMap := make(map[string]*DailyGrowthPoint)
	type genKey struct {
		trigger string
		taskID  string
	}
	genMap := make(map[genKey]*BackgroundGenerator)

	todayUTC := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	last7Start := todayUTC.AddDate(0, 0, -6)
	last7End := todayUTC.AddDate(0, 0, 1)
	prev7Start := todayUTC.AddDate(0, 0, -13)

	var oldest, newest time.Time

	for _, e := range entries {
		chars := utf8.RuneCountInString(e.Content)
		stats.TotalChars += chars
		stats.TotalBytes += len(e.Content)

		sid := entryOriginSessionID(e)
		origin, hasOrigin := origins[sid]
		trigger := "unknown"
		if hasOrigin && origin.Trigger != "" {
			trigger = origin.Trigger
		}
		stats.Triggers.add(trigger)

		kind := e.Kind
		if kind == "" {
			kind = "fact"
		}
		kb, exists := kindMap[kind]
		if !exists {
			kb = &KindBreakdown{Kind: kind}
			kindMap[kind] = kb
			extraKinds = append(extraKinds, kind)
		}
		kb.Total++
		kb.Chars += chars
		kb.add(trigger)

		if trigger == "task" || trigger == "initiative" {
			key := genKey{trigger: trigger}
			if trigger == "task" {
				key.taskID = origin.TaskID
			}
			g, found := genMap[key]
			if !found {
				g = &BackgroundGenerator{
					Trigger: trigger,
					TaskID:  key.taskID,
					Title:   origin.TaskTitle,
				}
				genMap[key] = g
			}
			if g.Title == "" && origin.TaskTitle != "" {
				g.Title = origin.TaskTitle
			}
			g.Count++
			g.Chars += chars
			if !e.UpdatedAt.IsZero() {
				ts := e.UpdatedAt.UTC()
				if g.LatestAt == nil || ts.After(*g.LatestAt) {
					t := ts
					g.LatestAt = &t
				}
			}
		}

		if ts, fallback := entryAddedAt(e, origin, hasOrigin); !ts.IsZero() {
			if fallback {
				stats.FallbackDateEntries++
			}
			if oldest.IsZero() || ts.Before(oldest) {
				oldest = ts
			}
			if newest.IsZero() || ts.After(newest) {
				newest = ts
			}
			dayKey := ts.Format("2006-01-02")
			pt, found := dailyMap[dayKey]
			if !found {
				pt = &DailyGrowthPoint{Date: dayKey}
				dailyMap[dayKey] = pt
			}
			pt.Total++
			pt.add(trigger)

			if !ts.Before(last7Start) && ts.Before(last7End) {
				stats.Trend.Last7Days++
			} else if !ts.Before(prev7Start) && ts.Before(last7Start) {
				stats.Trend.Prev7Days++
			}
		}
	}

	if !oldest.IsZero() {
		o, n := oldest, newest
		stats.OldestAt = &o
		stats.NewestAt = &n
		oldDay := time.Date(oldest.Year(), oldest.Month(), oldest.Day(), 0, 0, 0, 0, time.UTC)
		newDay := time.Date(newest.Year(), newest.Month(), newest.Day(), 0, 0, 0, 0, time.UTC)
		stats.SpanDays = int(newDay.Sub(oldDay)/(24*time.Hour)) + 1

		// Keep the daily chart strictly contiguous by calendar day. For spans
		// longer than maxDailyWindowDays, show the bounded recent 90-day window.
		startDay := oldDay
		if stats.SpanDays > maxDailyWindowDays {
			startDay = newDay.AddDate(0, 0, -(maxDailyWindowDays - 1))
			stats.DailyGrowthWindowDays = maxDailyWindowDays
		}
		for d := startDay; !d.After(newDay); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			if pt, ok := dailyMap[key]; ok {
				stats.DailyGrowth = append(stats.DailyGrowth, *pt)
			} else {
				stats.DailyGrowth = append(stats.DailyGrowth, DailyGrowthPoint{Date: key})
			}
		}
	}

	stats.BackgroundEntries = stats.Triggers.Task + stats.Triggers.Initiative
	if stats.TotalEntries > 0 {
		stats.BackgroundSharePercent = round1(float64(stats.BackgroundEntries) * 100 / float64(stats.TotalEntries))
	}

	stats.Trend.Delta = stats.Trend.Last7Days - stats.Trend.Prev7Days
	stats.Trend.AvgPerDay = round1(float64(stats.Trend.Last7Days) / 7.0)
	if stats.Trend.Prev7Days > 0 {
		pct := round1(float64(stats.Trend.Delta) * 100 / float64(stats.Trend.Prev7Days))
		stats.Trend.DeltaPercent = &pct
	}

	sort.Strings(extraKinds)
	stats.Kinds = make([]KindBreakdown, 0, len(canonicalMemoryKinds)+len(extraKinds))
	for _, k := range canonicalMemoryKinds {
		stats.Kinds = append(stats.Kinds, *kindMap[k])
	}
	for _, k := range extraKinds {
		stats.Kinds = append(stats.Kinds, *kindMap[k])
	}

	for _, g := range genMap {
		stats.TopGenerators = append(stats.TopGenerators, *g)
	}
	sort.Slice(stats.TopGenerators, func(i, j int) bool {
		a, b := stats.TopGenerators[i], stats.TopGenerators[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Chars != b.Chars {
			return a.Chars > b.Chars
		}
		if a.Trigger != b.Trigger {
			return a.Trigger < b.Trigger
		}
		if a.Title != b.Title {
			return a.Title < b.Title
		}
		return a.TaskID < b.TaskID
	})
	if len(stats.TopGenerators) > maxTopGenerators {
		stats.TopGenerators = stats.TopGenerators[:maxTopGenerators]
	}

	return stats
}

func computeProfileBudget(entries []domain.MemoryEntry, profile string, limit int) ProfileBudget {
	totalChars := utf8.RuneCountInString(profile)
	includedChars := min(totalChars, limit)
	truncatedChars := max(0, totalChars-limit)
	usagePercent := 0.0
	if limit > 0 {
		usagePercent = round1(float64(totalChars) * 100 / float64(limit))
	}
	status := "ok"
	if totalChars > limit {
		status = "over"
	} else if totalChars*100 >= limit*80 {
		status = "warning"
	}
	budget := ProfileBudget{
		Limit:          limit,
		TotalChars:     totalChars,
		IncludedChars:  includedChars,
		TruncatedChars: truncatedChars,
		UsagePercent:   usagePercent,
		Status:         status,
		TruncatedItems: []TruncatedProfileEntry{},
	}
	if profile == "" {
		return budget
	}

	// Match eligible entries in knowledge.Store.rebuildProfile's deterministic order:
	// profile entries before preference entries, newest UpdatedAt first, then ID.
	eligible := make([]domain.MemoryEntry, 0)
	for _, e := range entries {
		if e.SubjectUserID == 0 && (e.Kind == "profile" || e.Kind == "preference") {
			eligible = append(eligible, e)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		iProfile := eligible[i].Kind == "profile"
		jProfile := eligible[j].Kind == "profile"
		if iProfile != jProfile {
			return iProfile
		}
		if !eligible[i].UpdatedAt.Equal(eligible[j].UpdatedAt) {
			return eligible[i].UpdatedAt.After(eligible[j].UpdatedAt)
		}
		return eligible[i].ID < eligible[j].ID
	})

	byFirstLine := make(map[string][]int, len(eligible))
	for i, e := range eligible {
		line := "- " + e.Content
		firstLine := line
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			firstLine = line[:nl]
		}
		byFirstLine[firstLine] = append(byFirstLine[firstLine], i)
	}
	used := make([]bool, len(eligible))

	pos := 0
	runeOffset := 0
	const header = "# User profile\n\n"
	if strings.HasPrefix(profile, header) {
		pos = len(header)
		runeOffset = utf8.RuneCountInString(header)
	}
	for pos < len(profile) {
		rem := profile[pos:]
		firstLine := rem
		if nl := strings.IndexByte(rem, '\n'); nl >= 0 {
			firstLine = rem[:nl]
		}
		matchedIdx := -1
		matchedLen := 0
		hasTrailingNewline := false
		for _, idx := range byFirstLine[firstLine] {
			if used[idx] {
				continue
			}
			line := "- " + eligible[idx].Content
			if strings.HasPrefix(rem, line+"\n") {
				matchedIdx = idx
				matchedLen = len(line) + 1
				hasTrailingNewline = true
				break
			}
			if rem == line {
				matchedIdx = idx
				matchedLen = len(line)
				hasTrailingNewline = false
				break
			}
		}
		if matchedIdx >= 0 {
			used[matchedIdx] = true
			e := eligible[matchedIdx]
			segment := rem[:matchedLen]
			segmentRunes := utf8.RuneCountInString(segment)
			startRune := runeOffset
			endRune := runeOffset + segmentRunes
			contentEndRune := endRune
			if hasTrailingNewline {
				contentEndRune--
			}
			budget.TotalEntries++
			if contentEndRune <= limit {
				budget.IncludedEntries++
			} else {
				budget.TruncatedEntries++
				budget.TruncatedItems = append(budget.TruncatedItems, TruncatedProfileEntry{
					ID:        e.ID,
					Kind:      e.Kind,
					Content:   e.Content,
					Chars:     contentEndRune - startRune,
					StartChar: startRune,
					EndChar:   contentEndRune,
					Partial:   startRune < limit,
					UpdatedAt: e.UpdatedAt,
				})
			}
			pos += matchedLen
			runeOffset = endRune
			continue
		}
		if nl := strings.IndexByte(rem, '\n'); nl >= 0 {
			runeOffset += utf8.RuneCountInString(rem[:nl+1])
			pos += nl + 1
		} else {
			break
		}
	}
	return budget
}

type consolidationReceipt struct {
	CompletedAt        *time.Time             `json:"completed_at"`
	BeforeEntries      int                    `json:"before_entries"`
	AfterEntries       int                    `json:"after_entries"`
	BeforeProfileChars int                    `json:"before_profile_chars"`
	AfterProfileChars  int                    `json:"after_profile_chars"`
	ProfileCharsDelta  *int                   `json:"profile_chars_delta"`
	OperationCounts    *ConsolidationOpCounts `json:"operation_counts"`
	Operations         []struct {
		Op string `json:"op"`
	} `json:"operations"`
}

func parseConsolidationReceipt(id string, raw json.RawMessage) *ConsolidationSummary {
	if len(raw) == 0 {
		return nil
	}
	summary := &ConsolidationSummary{
		ID:  id,
		Log: append(json.RawMessage(nil), raw...),
	}
	var receipt consolidationReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return summary
	}
	if receipt.CompletedAt != nil && !receipt.CompletedAt.IsZero() {
		ts := receipt.CompletedAt.UTC()
		summary.CompletedAt = &ts
	}
	summary.BeforeEntries = receipt.BeforeEntries
	summary.AfterEntries = receipt.AfterEntries
	summary.BeforeProfileChars = receipt.BeforeProfileChars
	summary.AfterProfileChars = receipt.AfterProfileChars
	if receipt.ProfileCharsDelta != nil {
		summary.ProfileCharsDelta = *receipt.ProfileCharsDelta
	} else {
		summary.ProfileCharsDelta = receipt.AfterProfileChars - receipt.BeforeProfileChars
	}
	if receipt.OperationCounts != nil {
		summary.Operations = *receipt.OperationCounts
	} else {
		for _, item := range receipt.Operations {
			switch strings.ToLower(strings.TrimSpace(item.Op)) {
			case "merge", "merged":
				summary.Operations.Merged++
			case "reclassify", "reclassified":
				summary.Operations.Reclassified++
			case "delete", "deleted":
				summary.Operations.Deleted++
			case "keep", "kept":
				summary.Operations.Kept++
			case "update", "updated", "compact", "compacted":
				summary.Operations.Updated++
			case "skip", "skipped":
				summary.Operations.Skipped++
			}
		}
	}
	return summary
}

func consolidationSummaryFromReport(report domain.MemoryConsolidationReport) *ConsolidationSummary {
	summary := &ConsolidationSummary{
		ID:                 report.ID,
		BeforeEntries:      report.BeforeCount,
		AfterEntries:       report.AfterCount,
		BeforeProfileChars: report.ProfileCharsBefore,
		AfterProfileChars:  report.ProfileCharsAfter,
		ProfileCharsDelta:  report.ProfileCharsDelta,
	}
	if !report.CompletedAt.IsZero() {
		ts := report.CompletedAt.UTC()
		summary.CompletedAt = &ts
	} else if !report.StartedAt.IsZero() {
		ts := report.StartedAt.UTC()
		summary.CompletedAt = &ts
	}
	for _, item := range report.Operations {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "skipped":
			summary.Operations.Skipped++
			continue
		case "kept":
			summary.Operations.Kept++
			continue
		}
		switch strings.ToLower(strings.TrimSpace(item.Op)) {
		case "merge", "merged":
			summary.Operations.Merged++
		case "reclassify", "reclassified":
			summary.Operations.Reclassified++
		case "delete", "deleted":
			summary.Operations.Deleted++
		case "keep", "kept":
			summary.Operations.Kept++
		case "update", "updated", "compact", "compacted":
			summary.Operations.Updated++
		case "skip", "skipped":
			summary.Operations.Skipped++
		}
	}
	if raw, err := json.Marshal(report); err == nil {
		summary.Log = raw
	}
	return summary
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
