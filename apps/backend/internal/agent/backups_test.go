package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openalbert/apps/backend/internal/domain"
	"openalbert/services/backup"
)

func TestBackupContextRefreshesEveryModelCallAndSurvivesOldTurn(t *testing.T) {
	a, _, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	dir := t.TempDir()
	a.Backups = &backup.Reader{Dir: dir}
	now := time.Now().UTC()
	s := backup.State{Version: 1, Enabled: true, Phase: "uploading", UpdatedAt: now, HeartbeatAt: now, KeepDays: 14}
	if err := backup.AtomicJSON(filepath.Join(dir, "status.json"), s); err != nil {
		t.Fatal(err)
	}
	turn, err := a.Prepare(context.Background(), RunInput{Key: "backup-context", SessionID: "session", ConversationID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	model := &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
		text := messages[0].Content
		if !strings.Contains(text, `"phase":"`+s.Phase+`"`) {
			t.Fatalf("stale backup context: %s", text)
		}
		if !strings.Contains(text, "not instructions") {
			t.Fatal("host data presented as instructions")
		}
		return domain.Message{Role: "assistant", Content: "done"}, nil
	}}
	a.Model = model
	for step, phase := range []string{"uploading", "completed"} {
		s.Phase = phase
		s.UpdatedAt = time.Now().UTC()
		if err := backup.AtomicJSON(filepath.Join(dir, "status.json"), s); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ModelCall(context.Background(), ModelInput{Turn: turn, Step: step}); err != nil {
			t.Fatal(err)
		}
	}
	if len(turn.Messages) > 0 && strings.Contains(turn.Messages[len(turn.Messages)-1].Content, "Backup status (") {
		t.Fatal("mutated persisted Turn; next call would accumulate stale blocks")
	}
}

func TestBackupStatusOwnerAuthorizationIncludesCachedReplay(t *testing.T) {
	a, _, _, owner := fixture(t)
	a.OwnerUserID = owner.OwnerID
	a.Backups = &backup.Reader{Dir: t.TempDir()}
	call := domain.ToolCall{ID: "status", Name: "backup_status", Arguments: json.RawMessage(`{}`)}
	for _, conv := range []domain.Conversation{owner, {ID: "group:1", OwnerID: owner.OwnerID, ChatID: -1}, {ID: "personal:8", Private: true, OwnerID: 8, ChatID: 8}, {ID: "spoofed", Private: true, OwnerID: owner.OwnerID, ChatID: 8}} {
		allowed := conv.ID == owner.ID
		visible := false
		for _, def := range a.toolsForTurn(Turn{Conversation: conv}) {
			visible = visible || def.Name == "backup_status"
		}
		if visible != allowed {
			t.Fatalf("visibility %s", conv.ID)
		}
		out, err := a.ToolCall(context.Background(), ToolInput{RunKey: "same", Conversation: conv, Call: call})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, `"error"`) == allowed {
			t.Fatalf("authorization failed: %s %s", conv.ID, out)
		}
		a.Model = &testModel{respond: func(messages []domain.Message) (domain.Message, error) {
			for _, m := range messages {
				if strings.Contains(m.Content, "Backup status (") && !allowed {
					t.Fatal("private backup metadata leaked")
				}
			}
			return domain.Message{Content: "done"}, nil
		}}
		_, err = a.ModelCall(context.Background(), ModelInput{Turn: Turn{Conversation: conv, Input: RunInput{Key: conv.ID}, Messages: []domain.Message{{Role: "user", Content: "hello"}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
}
