package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jarvis/internal/domain"
	"jarvis/internal/knowledge"
)

type memoryConversations struct{}

func (memoryConversations) ListConversations(context.Context) ([]domain.Conversation, error) {
	return []domain.Conversation{{ID: "personal:7", Private: true}, {ID: "group:8", Title: "Группа"}}, nil
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
		{"GET", "/api/inspect/memory/conversations", "evil.example", "", 403},
		{"GET", base, "localhost", "https://evil.example", 403},
		{"GET", "/api/inspect/memory/entry?conversation_id=personal:7&id=fact-000", "localhost", "https://evil.example", 403},
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

func TestSoulInspectorReadsConfiguredFileAndRemainsLocalReadOnly(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprint(external), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "SOUL.md")
			soulFile := ""
			if external {
				path = filepath.Join(t.TempDir(), "owner.md")
				soulFile = path
			}
			content := "# Jarvis\n\nИмя и характер <script>текст</script>"
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
				{"GET", "evil.example", "", 403},
				{"GET", "localhost", "https://evil.example", 403},
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
