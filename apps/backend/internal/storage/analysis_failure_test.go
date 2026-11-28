package storage

import (
	"context"
	"encoding/json"
	"testing"

	"openalbert/apps/backend/internal/domain"
)

// This is part of the shared repository contract, including real PostgreSQL.
func analysisFailureContract(t *testing.T, repo domain.Repository, nonce, scope string) {
	t.Helper()
	ctx := context.Background()
	session := domain.Session{ID: nonce + ":analysis-failure", ConversationID: scope}
	if err := repo.StartSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := repo.EndSession(ctx, session.ID, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repo.MarkAnalysisFailed(ctx, session.ID, "context_limit"); err != nil {
			t.Fatal(err)
		}
	}
	// Neither retries nor a stale successful completion may relabel failure as
	// analyzed or overwrite the original failure outcome.
	if err := repo.MarkAnalysisFailed(ctx, session.ID, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkAnalyzed(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := repo.PendingAnalysis(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range pending {
		if item.ID == session.ID {
			t.Fatal("terminal failure remains pending")
		}
	}
	var stored domain.Session
	var analyzed bool
	switch r := repo.(type) {
	case *Memory:
		r.mu.Lock()
		stored, analyzed = r.sessions[session.ID], r.analyzed[session.ID]
		r.mu.Unlock()
	case *Store:
		var raw string
		var analyzedInt int
		if err := r.queryRow(ctx, "SELECT data, analyzed FROM openalbert_sessions WHERE id = ?1", session.ID).Scan(&raw, &analyzedInt); err != nil {
			t.Fatal(err)
		}
		analyzed = analyzedInt != 0
		if err := json.Unmarshal([]byte(raw), &stored); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported repository %T", repo)
	}
	if stored.AnalysisError != "context_limit" || analyzed || stored.Summary != "" || stored.Status != "closed" {
		t.Fatalf("failure outcome lost: session=%+v analyzed=%v", stored, analyzed)
	}
}
