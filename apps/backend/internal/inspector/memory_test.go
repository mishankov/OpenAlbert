package inspector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/knowledge"
)

type memoryConversations struct {
	consolidations map[string][]domain.MemoryConsolidationReport
}

func (memoryConversations) ListConversations(context.Context) ([]domain.Conversation, error) {
	return []domain.Conversation{{ID: "personal:7", ChatID: 7, Private: true}, {ID: "group:8", ChatID: -8, Title: "Группа"}, {ID: "group:8:topic:9", ChatID: -8, ThreadID: 9}, {ID: "group:empty", ChatID: -9}}, nil
}

func (c memoryConversations) Conversation(ctx context.Context, id string) (domain.Conversation, bool, error) {
	rows, err := c.ListConversations(ctx)
	for _, row := range rows {
		if row.ID == id {
			return row, true, err
		}
	}
	return domain.Conversation{}, false, err
}

func (c memoryConversations) ListMemoryConsolidations(_ context.Context, scope string, _ int) ([]domain.MemoryConsolidationReport, error) {
	return c.consolidations[scope], nil
}

func TestMemoryInspectorReadsAllPagesAndKeepsScopes(t *testing.T) {
	ctx := context.Background()
	store, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	for i := range 105 {
		err = store.Update(ctx, domain.MemoryEntry{Scope: "personal:7", ID: fmt.Sprintf("fact-%03d", i), Kind: "fact", Content: "Москва Go", Source: "session-7", UpdatedAt: stamp})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Update(ctx, domain.MemoryEntry{Scope: "personal:7", ID: "profile", Kind: "profile", Content: "Живёт в Москве", Source: "session-7", UpdatedAt: stamp.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(ctx, domain.MemoryEntry{Scope: "group:8", ID: "fact-000", Kind: "fact", Content: "Отдельный групповой факт", Source: "group-session"}); err != nil {
		t.Fatal(err)
	}
	// Inspector enumeration must not expand the agent's search result limit.
	search, err := store.Search(ctx, "personal:7", "")
	if err != nil || len(search) != 50 {
		t.Fatalf("agent search: %d %v", len(search), err)
	}
	mux := http.NewServeMux()
	RegisterMemory(mux, memoryConversations{}, store)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	var page struct {
		Entries []domain.MemoryEntry `json:"entries"`
		Total   int                  `json:"total"`
		Next    int                  `json:"next_after"`
		Profile string               `json:"profile"`
	}
	base := "/api/inspect/memory?conversation_id=personal:7"
	if err = json.Unmarshal(get(base).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 100 || page.Total != 106 || page.Next != 100 || page.Entries[0].ID != "profile" || page.Entries[1].ID != "fact-000" || page.Profile == "" {
		t.Fatalf("first page: %+v", page)
	}
	_ = json.Unmarshal(get(base+"&after=100").Body.Bytes(), &page)
	if len(page.Entries) != 6 || page.Next != 0 || page.Entries[5].ID != "fact-104" {
		t.Fatalf("next page: %+v", page)
	}
	_ = json.Unmarshal(get(base+"&q="+url.QueryEscape("МОСКВА go")+"&kind=fact").Body.Bytes(), &page)
	if page.Total != 105 || len(page.Entries) != 100 {
		t.Fatalf("search/filter: %+v", page)
	}
	_ = json.Unmarshal(get(base+"&q=missing").Body.Bytes(), &page)
	if page.Total != 0 || page.Entries == nil || len(page.Entries) != 0 {
		t.Fatalf("empty search: %+v", page)
	}
	_ = json.Unmarshal(get(base+"&after=999999999999999999").Body.Bytes(), &page)
	if len(page.Entries) != 0 {
		t.Fatal("out-of-range pagination")
	}
	var entry domain.MemoryEntry
	_ = json.Unmarshal(get("/api/inspect/memory/entry?conversation_id=group:8&id=fact-000").Body.Bytes(), &entry)
	if entry.Scope != "group:8" || entry.Content != "Отдельный групповой факт" {
		t.Fatalf("scoped read: %+v", entry)
	}
	get("/api/inspect/memory/conversations")
	for _, tc := range []struct {
		method, path, host, origin string
		code                       int
	}{
		{"GET", "/api/inspect/memory", "localhost", "", 400},
		{"GET", "/api/inspect/memory/entry?conversation_id=personal:7", "localhost", "", 400},
		{"GET", "/api/inspect/memory/entry?conversation_id=other&id=fact-000", "localhost", "", 404},
		{"GET", "/api/inspect/memory/conversations", "inspector.lan", "", 200},
		{"GET", base, "localhost", "https://other.example", 200},
		{"GET", "/api/inspect/memory/entry?conversation_id=personal:7&id=fact-000", "localhost", "https://other.example", 200},
		{"POST", base, "localhost", "", 405},
		{"DELETE", "/api/inspect/memory/entry?conversation_id=personal:7&id=fact-000", "localhost", "", 405},
	} {
		t.Run(tc.method+tc.path+tc.host+tc.origin, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			r.Header.Set("Origin", tc.origin)
			mux.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSoulInspectorReadsConfiguredFileAndRemainsReadOnly(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprint(external), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "SOUL.md")
			soulFile := ""
			if external {
				path = filepath.Join(t.TempDir(), "owner.md")
				soulFile = path
			}
			content := "# OpenAlbert\n\nИмя и характер <script>текст</script>"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := knowledge.NewWithSoulFile(root, soulFile)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			RegisterMemory(mux, memoryConversations{}, store)
			read := func() {
				t.Helper()
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/soul", nil))
				var out struct {
					Soul string `json:"soul"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || out.Soul != content || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("soul: %d %q %v", w.Code, w.Body.String(), err)
				}
			}
			read()
			content = "# Новая личность"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			read()
			for _, tc := range []struct {
				method, host, origin string
				code                 int
			}{
				{"GET", "inspector.lan", "", 200},
				{"GET", "localhost", "https://other.example", 200},
				{"PUT", "localhost", "", 405},
				{"DELETE", "localhost", "", 405},
			} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/inspect/memory/soul", nil)
				r.Header.Set("Origin", tc.origin)
				mux.ServeHTTP(w, r)
				if w.Code != tc.code {
					t.Fatalf("%s %s: %d", tc.method, tc.host, w.Code)
				}
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/soul", nil))
			if w.Code != 500 {
				t.Fatalf("missing soul: %d", w.Code)
			}
		})
	}
}

func TestGroupInspectorReadsCurrentSharedFileAndRemainsReadOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := knowledge.New(root)
	if err != nil {
		t.Fatal(err)
	}
	content := "# Правила группы\nОтвечай по обращению. <script>текст</script>"
	if err := store.UpdateGroupRules(ctx, -8, "", content); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	RegisterMemory(mux, memoryConversations{}, store)
	read := func(scope, expected string) {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/group?conversation_id="+url.QueryEscape(scope), nil))
		var out struct {
			GroupID int64  `json:"group_id"`
			Group   string `json:"group"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || out.Group != expected || out.GroupID >= 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("group %s: %d %q %v", scope, w.Code, w.Body.String(), err)
		}
	}
	read("group:8", content)
	read("group:8:topic:9", content)
	read("group:empty", "")
	updated := "# Новые правила"
	if err := store.UpdateGroupRules(ctx, -8, content, updated); err != nil {
		t.Fatal(err)
	}
	read("group:8", updated)
	for _, tc := range []struct {
		method, scope string
		code          int
	}{
		{"GET", "", 400},
		{"GET", "personal:7", 404},
		{"GET", "group:unknown", 404},
		{"GET", "../../SOUL.md", 404},
		{"POST", "group:8", 405},
		{"PUT", "group:8", 405},
		{"DELETE", "group:8", 405},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, "http://localhost/api/inspect/memory/group?conversation_id="+url.QueryEscape(tc.scope), nil))
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.scope, w.Code)
		}
	}
	read("group:8", updated)
	if err := store.UpdateGroupRules(ctx, -8, updated, ""); err != nil {
		t.Fatal(err)
	}
	read("group:8:topic:9", "")
	path := filepath.Join(root, "groups", "-8", "GROUP.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	read("group:8", "")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/inspect/memory/group?conversation_id=group:8", 500}, {"/api/inspect/memory?conversation_id=group:8", 200}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+tc.path, nil))
		if w.Code != tc.code {
			t.Fatalf("GROUP.md read error affected %s: %d", tc.path, w.Code)
		}
	}
}

type memoryMetadataConversations struct {
	memoryConversations
	origins          map[string]SessionOrigin
	originsCalls     int
	lastOriginScope  string
	lastOriginIDs    []string
	originsErr       error
	consolidationID  string
	consolidationRaw json.RawMessage
	consolidationErr error
}

func (m *memoryMetadataConversations) InspectSessionOrigins(_ context.Context, scope string, sessionIDs []string) (map[string]SessionOrigin, error) {
	m.originsCalls++
	m.lastOriginScope = scope
	m.lastOriginIDs = append([]string(nil), sessionIDs...)
	if m.originsErr != nil {
		return nil, m.originsErr
	}
	out := make(map[string]SessionOrigin, len(sessionIDs))
	for _, id := range sessionIDs {
		if o, ok := m.origins[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (m *memoryMetadataConversations) InspectLatestConsolidation(_ context.Context, _ string) (string, json.RawMessage, bool, error) {
	if m.consolidationErr != nil {
		return "", nil, false, m.consolidationErr
	}
	if len(m.consolidationRaw) == 0 {
		return "", nil, false, nil
	}
	return m.consolidationID, m.consolidationRaw, true, nil
}

func TestMemoryStatsEndpointAggregatesAndRemainsReadOnly(t *testing.T) {
	ctx := context.Background()
	store, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	day := func(offset int) time.Time { return now.AddDate(0, 0, offset) }

	// Populate profile + preference entries exceeding 8,000 Cyrillic runes to verify
	// exact truncation metrics matching clip(profile, 8000) under profile-first,
	// newest-first USER.md ordering.
	entries := []domain.MemoryEntry{
		{Scope: "personal:7", ID: "01-prof-a", Kind: "profile", Content: strings.Repeat("А", 4200), Source: "sess-msg", UpdatedAt: day(-10)},
		{Scope: "personal:7", ID: "02-pref-b", Kind: "preference", Content: strings.Repeat("Б", 3900), Source: "session:sess-task-1; sess-later", UpdatedAt: day(-1)},
		{Scope: "personal:7", ID: "03-pref-c", Kind: "preference", Content: strings.Repeat("В", 200), Source: "sess-init", UpdatedAt: day(-2)},
		{Scope: "personal:7", ID: "04-fact-1", Kind: "fact", Content: "Задача мониторит погоду", Source: "sess-task-1", UpdatedAt: day(-1)},
		{Scope: "personal:7", ID: "05-ep-1", Kind: "episode", Content: "Сводка инициативы", Source: "sess-init", UpdatedAt: day(0)},
		{Scope: "personal:7", ID: "06-hyp-1", Kind: "hypothesis", Content: "Может предпочитать вечер", Source: "sess-missing", UpdatedAt: day(-8)},
		{Scope: "group:8", ID: "other-scope", Kind: "fact", Content: "Другой разговор", Source: "sess-group", UpdatedAt: day(0)},
	}
	for _, e := range entries {
		if err := store.Update(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	meta := &memoryMetadataConversations{
		origins: map[string]SessionOrigin{
			"sess-msg":    {SessionID: "sess-msg", Trigger: "message"},
			"sess-task-1": {SessionID: "sess-task-1", Trigger: "task", TaskID: "task-weather", TaskTitle: "Прогноз погоды"},
			"sess-init":   {SessionID: "sess-init", Trigger: "initiative"},
		},
		consolidationID: "memory-consolidation/personal:7/2026-10-23",
		consolidationRaw: json.RawMessage(`{
			"scope": "personal:7",
			"completed_at": "2026-10-23T08:30:00Z",
			"before_entries": 24,
			"after_entries": 18,
			"before_profile_chars": 9200,
			"after_profile_chars": 6400,
			"operations": [
				{"op": "merge", "ids": ["a","b"], "reason": "duplicate"},
				{"op": "delete", "ids": ["c"], "reason": "expired"},
				{"op": "reclassify", "ids": ["d"], "reason": "one-off"},
				{"op": "keep", "ids": ["e"], "reason": "valid"}
			]
		}`),
	}

	mux := http.NewServeMux()
	RegisterMemory(mux, meta, store)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/stats?conversation_id=personal:7", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("stats status/headers: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if meta.originsCalls != 1 || meta.lastOriginScope != "personal:7" {
		t.Fatalf("expected single batched origin lookup for personal:7, got calls=%d scope=%q", meta.originsCalls, meta.lastOriginScope)
	}
	var got MemoryStats
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.TotalEntries != 6 || got.SpanDays != 11 {
		t.Fatalf("totals/span: %+v", got)
	}
	if got.Triggers.Message != 1 || got.Triggers.Task != 2 || got.Triggers.Initiative != 2 || got.Triggers.Unknown != 1 {
		t.Fatalf("triggers: %+v", got.Triggers)
	}
	if got.BackgroundEntries != 4 || got.BackgroundSharePercent != 66.7 {
		t.Fatalf("background share: entries=%d share=%v", got.BackgroundEntries, got.BackgroundSharePercent)
	}
	if got.Trend.Last7Days != 4 || got.Trend.Prev7Days != 2 || got.Trend.Delta != 2 || got.Trend.DeltaPercent == nil || *got.Trend.DeltaPercent != 100 {
		t.Fatalf("trend: %+v", got.Trend)
	}
	if len(got.DailyGrowth) != 11 {
		t.Fatalf("expected 11 contiguous daily buckets, got %d", len(got.DailyGrowth))
	}
	var dailySum int
	for _, pt := range got.DailyGrowth {
		dailySum += pt.Total
	}
	if dailySum != 6 {
		t.Fatalf("daily sum mismatch: %d", dailySum)
	}

	profile, err := store.Profile(ctx, "personal:7")
	if err != nil {
		t.Fatal(err)
	}
	wantRunes := utf8.RuneCountInString(profile)
	if got.ProfileBudget.Limit != UserProfileCharLimit || got.ProfileBudget.TotalChars != wantRunes || got.ProfileBudget.Status != "over" {
		t.Fatalf("profile budget: %+v (want runes=%d)", got.ProfileBudget, wantRunes)
	}
	if got.ProfileBudget.IncludedChars != 8000 || got.ProfileBudget.TruncatedChars != wantRunes-8000 {
		t.Fatalf("profile clip counts: %+v", got.ProfileBudget)
	}
	if got.ProfileBudget.TotalEntries != 3 || got.ProfileBudget.IncludedEntries != 1 || got.ProfileBudget.TruncatedEntries != 2 {
		t.Fatalf("profile entry counts: %+v", got.ProfileBudget)
	}
	if len(got.ProfileBudget.TruncatedItems) != 2 ||
		got.ProfileBudget.TruncatedItems[0].ID != "02-pref-b" || !got.ProfileBudget.TruncatedItems[0].Partial ||
		got.ProfileBudget.TruncatedItems[1].ID != "03-pref-c" || got.ProfileBudget.TruncatedItems[1].Partial {
		t.Fatalf("truncated items: %+v", got.ProfileBudget.TruncatedItems)
	}

	if len(got.Kinds) != 5 {
		t.Fatalf("expected 5 canonical kinds, got %d", len(got.Kinds))
	}
	kindByName := map[string]KindBreakdown{}
	for _, k := range got.Kinds {
		kindByName[k.Kind] = k
	}
	if kindByName["preference"].Total != 2 || kindByName["preference"].Task != 1 || kindByName["preference"].Initiative != 1 {
		t.Fatalf("preference breakdown: %+v", kindByName["preference"])
	}

	if len(got.TopGenerators) != 2 || got.TopGenerators[0].Trigger != "task" || got.TopGenerators[0].TaskID != "task-weather" || got.TopGenerators[0].Title != "Прогноз погоды" || got.TopGenerators[0].Count != 2 {
		t.Fatalf("top generators: %+v", got.TopGenerators)
	}
	if got.TopGenerators[1].Trigger != "initiative" || got.TopGenerators[1].Count != 2 {
		t.Fatalf("second generator: %+v", got.TopGenerators[1])
	}

	if got.LatestConsolidation == nil ||
		got.LatestConsolidation.BeforeEntries != 24 ||
		got.LatestConsolidation.AfterEntries != 18 ||
		got.LatestConsolidation.ProfileCharsDelta != -2800 ||
		got.LatestConsolidation.Operations.Merged != 1 ||
		got.LatestConsolidation.Operations.Deleted != 1 ||
		got.LatestConsolidation.Operations.Reclassified != 1 ||
		got.LatestConsolidation.Operations.Kept != 1 {
		t.Fatalf("consolidation summary: %+v", got.LatestConsolidation)
	}

	// Read-only and validation checks + error handling.
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/inspect/memory/stats", 400},
		{"POST", "/api/inspect/memory/stats?conversation_id=personal:7", 405},
		{"DELETE", "/api/inspect/memory/stats?conversation_id=personal:7", 405},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, "http://localhost"+tc.path, nil))
		if rec.Code != tc.code {
			t.Fatalf("%s %s: got %d, want %d", tc.method, tc.path, rec.Code, tc.code)
		}
	}

	meta.originsErr = errors.New("db unavailable")
	errRec := httptest.NewRecorder()
	mux.ServeHTTP(errRec, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/stats?conversation_id=personal:7", nil))
	if errRec.Code != 500 {
		t.Fatalf("origin lookup error should return 500, got %d", errRec.Code)
	}
}

func TestComputeProfileBudgetMatchesCustomOrderingAndNearLimit(t *testing.T) {
	// Even if USER.md orders profile entries ahead of preference entries (as in #54),
	// budget attribution must follow the rendered USER.md offsets rather than ID order.
	entries := []domain.MemoryEntry{
		{ID: "a-pref", Kind: "preference", Content: strings.Repeat("П", 1500)},
		{ID: "z-prof", Kind: "profile", Content: strings.Repeat("Р", 6600)},
	}
	profile := "# User profile\n\n- " + entries[1].Content + "\n- " + entries[0].Content + "\n"
	got := computeProfileBudget(entries, profile, UserProfileCharLimit)
	if got.IncludedEntries != 1 || got.TruncatedEntries != 1 || len(got.TruncatedItems) != 1 || got.TruncatedItems[0].ID != "a-pref" || !got.TruncatedItems[0].Partial {
		t.Fatalf("custom order truncation mismatch: %+v", got)
	}

	near := computeProfileBudget(entries[:1], "# User profile\n\n- "+strings.Repeat("П", 6500)+"\n", UserProfileCharLimit)
	if near.Status != "warning" || near.TruncatedEntries != 0 {
		t.Fatalf("near limit status mismatch: %+v", near)
	}

	// Deterministic ID-order matching when multiline content overlaps earlier single-line entries.
	ambiguous := []domain.MemoryEntry{
		{ID: "01-a", Kind: "profile", Content: "A"},
		{ID: "02-b", Kind: "profile", Content: "B"},
		{ID: "03-ab", Kind: "profile", Content: "A\n- B"},
	}
	ambProfile := "# User profile\n\n- A\n- B\n- A\n- B\n"
	gotAmb := computeProfileBudget(ambiguous, ambProfile, 26)
	if len(gotAmb.TruncatedItems) != 1 || gotAmb.TruncatedItems[0].ID != "03-ab" {
		t.Fatalf("ambiguous multiline match should attribute truncation to 03-ab, got %+v", gotAmb.TruncatedItems)
	}
}

func TestMemoryStatsUsesOriginCreatedAtForCorrectedEntriesAndCapsDailyWindow(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	day := func(offset int) time.Time { return now.AddDate(0, 0, offset) }

	entries := []domain.MemoryEntry{
		// Created 10 days ago in sess-old, then corrected today in sess-new: must count in prev_7_days, not last_7_days.
		{Scope: "personal:7", ID: "corrected", Kind: "fact", Content: "Обновлённый факт", Source: "sess-old; sess-new", UpdatedAt: day(0)},
		// Created today in sess-new.
		{Scope: "personal:7", ID: "added-today", Kind: "fact", Content: "Новый факт", Source: "sess-new", UpdatedAt: day(0)},
		// Legacy entry without resolved session falls back to UpdatedAt 120 days ago.
		{Scope: "personal:7", ID: "legacy", Kind: "fact", Content: "Старый факт", Source: "sess-missing", UpdatedAt: day(-120)},
	}
	origins := map[string]SessionOrigin{
		"sess-old": {SessionID: "sess-old", Trigger: "message", CreatedAt: day(-10)},
		"sess-new": {SessionID: "sess-new", Trigger: "message", CreatedAt: day(0)},
	}

	got := computeMemoryStats("personal:7", entries, "", origins, nil, now)
	if got.Trend.Last7Days != 1 || got.Trend.Prev7Days != 1 || got.Trend.Delta != 0 {
		t.Fatalf("corrected entry shifted growth window: %+v", got.Trend)
	}
	if got.FallbackDateEntries != 1 {
		t.Fatalf("expected 1 fallback date entry, got %d", got.FallbackDateEntries)
	}
	if got.SpanDays != 121 || got.DailyGrowthWindowDays != maxDailyWindowDays || len(got.DailyGrowth) != maxDailyWindowDays {
		t.Fatalf("expected contiguous 90-day window, got span=%d window=%d len=%d", got.SpanDays, got.DailyGrowthWindowDays, len(got.DailyGrowth))
	}
	// Verify the corrected entry sits on day(-10), not day(0).
	oldDate := day(-10).Format("2006-01-02")
	todayDate := day(0).Format("2006-01-02")
	counts := map[string]int{}
	for _, pt := range got.DailyGrowth {
		counts[pt.Date] = pt.Total
	}
	if counts[oldDate] != 1 || counts[todayDate] != 1 {
		t.Fatalf("expected 1 on %s and 1 on %s, got %d and %d", oldDate, todayDate, counts[oldDate], counts[todayDate])
	}
}

func TestMemoryStatsPreservesOriginalSessionAndDateOnToolOverwrite(t *testing.T) {
	ctx := context.Background()
	store, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	day := func(offset int) time.Time { return now.AddDate(0, 0, offset) }

	// Session 1 (message) creates a fact 10 days ago.
	if err := store.Update(ctx, domain.MemoryEntry{
		Scope: "personal:7", ID: "fav-lang", Kind: "fact",
		Content: "Пишет на Python", Source: "sess-msg-old", UpdatedAt: day(-10),
	}); err != nil {
		t.Fatal(err)
	}
	// Session 2 (task) overwrites the same ID today via memory_update (which replaces Source).
	if err := store.Update(ctx, domain.MemoryEntry{
		Scope: "personal:7", ID: "fav-lang", Kind: "fact",
		Content: "Пишет на Go", Source: "sess-task-new", UpdatedAt: day(0),
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListMemory(ctx, "personal:7")
	if err != nil {
		t.Fatal(err)
	}
	origins := map[string]SessionOrigin{
		"sess-msg-old":  {SessionID: "sess-msg-old", Trigger: "message", CreatedAt: day(-10)},
		"sess-task-new": {SessionID: "sess-task-new", Trigger: "task", TaskID: "t1", TaskTitle: "Task", CreatedAt: day(0)},
	}
	got := computeMemoryStats("personal:7", entries, "", origins, nil, now)
	if got.Triggers.Message != 1 || got.Triggers.Task != 0 {
		t.Fatalf("overwrite shifted originating trigger: %+v", got.Triggers)
	}
	if got.Trend.Prev7Days != 1 || got.Trend.Last7Days != 0 {
		t.Fatalf("overwrite counted as new addition in current week: %+v", got.Trend)
	}
	if got.DailyGrowth[0].Date != day(-10).Format("2006-01-02") || got.DailyGrowth[0].Message != 1 {
		t.Fatalf("overwrite shifted daily bucket: %+v", got.DailyGrowth)
	}

	// Legacy entry (no created_at on disk) originally created 10d ago, corrected 2d ago,
	// then edited again today via tool overwrite: must stay on -10d from origin session
	// and never promote the -2d UpdatedAt into CreatedAt or serialize 0001-01-01.
	legacyRoot := t.TempDir()
	legacyStore, err := knowledge.New(legacyRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a legacy entry, then strip created_at from its frontmatter file to simulate pre-change disk state.
	if err := legacyStore.Update(ctx, domain.MemoryEntry{
		Scope: "personal:7", ID: "legacy-fact", Kind: "fact",
		Content: "Исправленный 2 дня назад факт", Source: "sess-msg-old; sess-2d", UpdatedAt: day(-2),
	}); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(legacyRoot, "scopes", "*", "memories", "*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("legacy file glob: %v %v", matches, err)
	}
	rawFile, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(rawFile), "\n---\n", 2)
	var metaMap map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(parts[0], "---\n")), &metaMap); err != nil {
		t.Fatal(err)
	}
	delete(metaMap, "created_at")
	strippedMeta, _ := json.Marshal(metaMap)
	if err := os.WriteFile(matches[0], []byte("---\n"+string(strippedMeta)+"\n---\n"+parts[1]), 0600); err != nil {
		t.Fatal(err)
	}
	// Edit the legacy entry again today from a task session.
	if err := legacyStore.Update(ctx, domain.MemoryEntry{
		Scope: "personal:7", ID: "legacy-fact", Kind: "fact",
		Content: "Отредактирован сегодня", Source: "sess-task-new", UpdatedAt: day(0),
	}); err != nil {
		t.Fatal(err)
	}
	legacyEntries, err := legacyStore.ListMemory(ctx, "personal:7")
	if err != nil || len(legacyEntries) != 1 {
		t.Fatalf("legacy list: %v %+v", err, legacyEntries)
	}
	if !legacyEntries[0].CreatedAt.IsZero() {
		t.Fatalf("legacy edit must not promote UpdatedAt into CreatedAt: %+v", legacyEntries[0])
	}
	encoded, err := json.Marshal(legacyEntries[0])
	if err != nil || strings.Contains(string(encoded), "0001-01-01") {
		t.Fatalf("zero CreatedAt serialized: %s %v", encoded, err)
	}
	legacyStats := computeMemoryStats("personal:7", legacyEntries, "", origins, nil, now)
	if legacyStats.Triggers.Message != 1 || legacyStats.Trend.Prev7Days != 1 || legacyStats.Trend.Last7Days != 0 {
		t.Fatalf("legacy overwrite shifted trigger or growth week: triggers=%+v trend=%+v", legacyStats.Triggers, legacyStats.Trend)
	}
}

func TestMemoryConsolidationInspectorEndpointAndStatsIntegration(t *testing.T) {
	store, err := knowledge.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	report := domain.MemoryConsolidationReport{
		ID:                 "run-1",
		Scope:              "personal:7",
		Trigger:            "scheduled",
		StartedAt:          time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
		CompletedAt:        time.Date(2026, 11, 25, 10, 1, 0, 0, time.UTC),
		Status:             "completed",
		BeforeCount:        12,
		AfterCount:         8,
		ProfileCharsBefore: 4200,
		ProfileCharsAfter:  2100,
		ProfileCharsDelta:  -2100,
		Operations: []domain.MemoryConsolidationOperation{
			{Phase: "profile", Op: "merge", IDs: []string{"a", "b"}, ResultID: "a", Kind: "preference", Reason: "duplicate rule", Status: "applied"},
			{Phase: "profile", Op: "compact", IDs: []string{"e"}, ResultID: "e", Kind: "preference", Reason: "tighten verbose rule", Status: "applied"},
			{Phase: "profile", Op: "reclassify", IDs: []string{"d"}, ResultID: "d", Kind: "profile", Reason: "already profile", Status: "kept"},
			{Phase: "hygiene", Op: "delete", IDs: []string{"c"}, Reason: "stale", Status: "skipped", SkipReason: "concurrent_modification"},
		},
	}
	mux := http.NewServeMux()
	RegisterMemory(mux, memoryConversations{consolidations: map[string][]domain.MemoryConsolidationReport{"personal:7": {report}}}, store)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/consolidations?conversation_id=personal:7", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("consolidations: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Consolidations []domain.MemoryConsolidationReport `json:"consolidations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Consolidations) != 1 || out.Consolidations[0].ProfileCharsDelta != -2100 {
		t.Fatalf("unexpected consolidations payload: %+v %v", out, err)
	}

	// Verify /api/inspect/memory/stats surfaces the latest consolidation report from ListMemoryConsolidations.
	wStats := httptest.NewRecorder()
	mux.ServeHTTP(wStats, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/stats?conversation_id=personal:7", nil))
	if wStats.Code != 200 {
		t.Fatalf("stats with consolidation report: %d %s", wStats.Code, wStats.Body.String())
	}
	var stats MemoryStats
	if err := json.Unmarshal(wStats.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.LatestConsolidation == nil ||
		stats.LatestConsolidation.ID != "run-1" ||
		stats.LatestConsolidation.BeforeEntries != 12 ||
		stats.LatestConsolidation.AfterEntries != 8 ||
		stats.LatestConsolidation.ProfileCharsDelta != -2100 ||
		stats.LatestConsolidation.Operations.Merged != 1 ||
		stats.LatestConsolidation.Operations.Updated != 1 ||
		stats.LatestConsolidation.Operations.Kept != 1 ||
		stats.LatestConsolidation.Operations.Reclassified != 0 ||
		stats.LatestConsolidation.Operations.Skipped != 1 ||
		stats.LatestConsolidation.Operations.Deleted != 0 ||
		len(stats.LatestConsolidation.Log) == 0 {
		t.Fatalf("unexpected stats latest consolidation: %+v", stats.LatestConsolidation)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/inspect/memory/consolidations?conversation_id=group:empty", nil))
	if w.Code != 200 || w.Body.String() != "{\"consolidations\":[]}\n" {
		t.Fatalf("empty consolidations: %d %q", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/inspect/memory/consolidations", 400},
		{"POST", "/api/inspect/memory/consolidations?conversation_id=personal:7", 405},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, "http://localhost"+tc.path, nil))
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}
