package inspector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"openalbert/apps/backend/internal/domain"
)

type memoryConversation struct {
	domain.Conversation
	MemoryCount   int        `json:"memory_count"`
	HasProfile    bool       `json:"has_profile,omitempty"`
	LastUpdatedAt *time.Time `json:"last_updated_at,omitempty"`
}

func isPersonalMemoryConversation(c domain.Conversation) bool {
	return strings.HasPrefix(c.ID, "personal:") || c.Private
}

func hasProfileContent(profile string) bool {
	trimmed := strings.TrimSpace(profile)
	return trimmed != "" && trimmed != "# User profile"
}

type ConversationStore interface {
	ListConversations(context.Context) ([]domain.Conversation, error)
	Conversation(context.Context, string) (domain.Conversation, bool, error)
	ListMemoryConsolidations(context.Context, string, int) ([]domain.MemoryConsolidationReport, error)
}

type MemoryStore interface {
	ListMemory(context.Context, string) ([]domain.MemoryEntry, error)
	Read(context.Context, string, string) (domain.MemoryEntry, error)
	Profile(context.Context, string) (string, error)
	Soul(context.Context) (string, error)
	GroupRules(context.Context, int64) (string, error)
}

// RegisterMemory exposes the knowledge store through the same read-only
// boundary as sessions. It deliberately does not use the agent's capped search.
func RegisterMemory(mux *http.ServeMux, conversations ConversationStore, memory MemoryStore) {
	mux.Handle("GET /api/inspect/memory/group", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := r.URL.Query().Get("conversation_id")
		if !memoryID(scope) {
			fail(w, 400)
			return
		}
		conv, found, err := conversations.Conversation(r.Context(), scope)
		if err != nil {
			fail(w, 500)
			return
		}
		if !found || conv.Private || conv.ChatID >= 0 {
			fail(w, 404)
			return
		}
		text, err := memory.GroupRules(r.Context(), conv.ChatID)
		if err != nil {
			fail(w, 500)
			return
		}
		write(w, map[string]any{"group_id": conv.ChatID, "group": text})
	})))
	mux.Handle("GET /api/inspect/memory/soul", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		text, err := memory.Soul(r.Context())
		if err != nil {
			fail(w, 500)
			return
		}
		write(w, map[string]string{"soul": text})
	})))
	mux.Handle("GET /api/inspect/memory/conversations", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows, err := conversations.ListConversations(r.Context())
		if err != nil {
			fail(w, 500)
			return
		}
		items := make([]memoryConversation, 0, len(rows))
		seen := make(map[string]bool, len(rows))
		codingOwners := make(map[int64]bool)
		appendScope := func(conv domain.Conversation, requireContent bool) bool {
			entries, err := memory.ListMemory(r.Context(), conv.ID)
			if err != nil {
				fail(w, 500)
				return false
			}
			profile, err := memory.Profile(r.Context(), conv.ID)
			if err != nil {
				fail(w, 500)
				return false
			}
			hasProfile := hasProfileContent(profile)
			if requireContent && len(entries) == 0 && !hasProfile {
				return true
			}
			item := memoryConversation{
				Conversation: conv,
				MemoryCount:  len(entries),
				HasProfile:   hasProfile,
			}
			var latest time.Time
			for _, e := range entries {
				if e.UpdatedAt.After(latest) {
					latest = e.UpdatedAt
				}
			}
			if !latest.IsZero() {
				utc := latest.UTC()
				item.LastUpdatedAt = &utc
			}
			items = append(items, item)
			seen[conv.ID] = true
			return true
		}
		for _, conv := range rows {
			if strings.HasPrefix(conv.ID, "coding:") {
				if conv.OwnerID > 0 {
					codingOwners[conv.OwnerID] = true
				}
				continue
			}
			if !appendScope(conv, false) {
				return
			}
		}
		for ownerID := range codingOwners {
			personalID := fmt.Sprintf("personal:%d", ownerID)
			if seen[personalID] {
				continue
			}
			if !appendScope(domain.Conversation{
				ID:      personalID,
				ChatID:  ownerID,
				OwnerID: ownerID,
				Private: true,
				Title:   "Личный разговор",
			}, true) {
				return
			}
		}
		sort.SliceStable(items, func(i, j int) bool {
			a, b := items[i], items[j]
			aPersonal := isPersonalMemoryConversation(a.Conversation)
			bPersonal := isPersonalMemoryConversation(b.Conversation)
			if aPersonal != bPersonal {
				return aPersonal
			}
			aHasMem := a.MemoryCount > 0 || a.HasProfile
			bHasMem := b.MemoryCount > 0 || b.HasProfile
			if aHasMem != bHasMem {
				return aHasMem
			}
			if a.MemoryCount != b.MemoryCount {
				return a.MemoryCount > b.MemoryCount
			}
			if (a.LastUpdatedAt != nil) != (b.LastUpdatedAt != nil) {
				return a.LastUpdatedAt != nil
			}
			if a.LastUpdatedAt != nil && !a.LastUpdatedAt.Equal(*b.LastUpdatedAt) {
				return a.LastUpdatedAt.After(*b.LastUpdatedAt)
			}
			return a.ID < b.ID
		})
		write(w, map[string]any{"conversations": items})
	})))
	mux.Handle("GET /api/inspect/memory", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := r.URL.Query().Get("conversation_id")
		if !memoryID(scope) {
			fail(w, 400)
			return
		}
		entries, err := memory.ListMemory(r.Context(), scope)
		if err != nil {
			fail(w, 500)
			return
		}
		profile, err := memory.Profile(r.Context(), scope)
		if err != nil {
			fail(w, 500)
			return
		}
		terms := strings.Fields(strings.ToLower(r.URL.Query().Get("q")))
		kind := r.URL.Query().Get("kind")
		filtered := []domain.MemoryEntry{}
		for _, e := range entries {
			if kind != "" && e.Kind != kind {
				continue
			}
			text := strings.ToLower(strings.Join([]string{e.ID, e.Content, e.Source, e.Kind, e.Evidence, strconv.FormatInt(e.SubjectUserID, 10)}, " "))
			match := true
			for _, term := range terms {
				if !strings.Contains(text, term) {
					match = false
					break
				}
			}
			if match {
				filtered = append(filtered, e)
			}
		}
		total := len(filtered)
		start := int(min(number(r, "after"), int64(total)))
		end := min(start+100, total)
		next := 0
		if end < total {
			next = end
		}
		write(w, map[string]any{"entries": filtered[start:end], "total": total, "next_after": next, "profile": profile})
	})))
	mux.Handle("GET /api/inspect/memory/stats", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := r.URL.Query().Get("conversation_id")
		if !memoryID(scope) {
			fail(w, 400)
			return
		}
		entries, err := memory.ListMemory(r.Context(), scope)
		if err != nil {
			fail(w, 500)
			return
		}
		profile, err := memory.Profile(r.Context(), scope)
		if err != nil {
			fail(w, 500)
			return
		}
		var origins map[string]SessionOrigin
		var consolidation *ConsolidationSummary
		if reports, err := conversations.ListMemoryConsolidations(r.Context(), scope, 1); err != nil {
			fail(w, 500)
			return
		} else if len(reports) > 0 {
			consolidation = consolidationSummaryFromReport(reports[0])
		}
		if meta, ok := conversations.(MemoryMetadataStore); ok {
			if sessionIDs := uniqueOriginSessionIDs(entries); len(sessionIDs) > 0 {
				origins, err = meta.InspectSessionOrigins(r.Context(), scope, sessionIDs)
				if err != nil {
					fail(w, 500)
					return
				}
			}
			if consolidation == nil {
				id, raw, found, err := meta.InspectLatestConsolidation(r.Context(), scope)
				if err != nil {
					fail(w, 500)
					return
				}
				if found {
					consolidation = parseConsolidationReceipt(id, raw)
				}
			}
		}
		write(w, computeMemoryStats(scope, entries, profile, origins, consolidation, time.Now().UTC()))
	})))
	mux.Handle("GET /api/inspect/memory/entry", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, id := r.URL.Query().Get("conversation_id"), r.URL.Query().Get("id")
		if !memoryID(scope) || !memoryID(id) {
			fail(w, 400)
			return
		}
		entry, err := memory.Read(r.Context(), scope, id)
		if errors.Is(err, fs.ErrNotExist) {
			fail(w, 404)
			return
		}
		if err != nil {
			fail(w, 500)
			return
		}
		write(w, entry)
	})))
	mux.Handle("GET /api/inspect/memory/consolidations", inspectorResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := r.URL.Query().Get("conversation_id")
		if !memoryID(scope) {
			fail(w, 400)
			return
		}
		rows, err := conversations.ListMemoryConsolidations(r.Context(), scope, 50)
		if err != nil {
			fail(w, 500)
			return
		}
		if rows == nil {
			rows = []domain.MemoryConsolidationReport{}
		}
		write(w, map[string]any{"consolidations": rows})
	})))
}

func memoryID(value string) bool { return strings.TrimSpace(value) != "" && len(value) <= 4096 }
