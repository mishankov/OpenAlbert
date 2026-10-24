package agent

import (
	"context"
	"testing"

	"openalbert/internal/domain"
)

type sessionModel struct{ sessions []string }

func (m *sessionModel) Complete(ctx context.Context, messages []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	m.sessions = append(m.sessions, domain.ModelSession(ctx))
	text := "ok"
	if len(tools) == 0 {
		text = `{"summary":"done","memories":[]}`
	}
	return domain.Completion{Message: domain.Message{Role: "assistant", Content: text}}, nil
}
func TestModelAndAnalysisShareOpaqueConversationSession(t *testing.T) {
	a, repo, _, conv := fixture(t)
	ctx := context.Background()
	model := &sessionModel{}
	a.Model = model
	for i, key := range []string{"first", "second"} {
		_, err := a.ModelCall(ctx, ModelInput{Turn: Turn{Input: RunInput{Key: key, ConversationID: conv.ID}}, Step: i})
		if err != nil {
			t.Fatal(err)
		}
	}
	session := domain.Session{ID: "analysis-session", ConversationID: conv.ID, Status: "active"}
	if err := repo.StartSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := repo.EndSession(ctx, session.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Analyze(ctx, session); err != nil {
		t.Fatal(err)
	}
	if len(model.sessions) != 3 {
		t.Fatalf("missing model requests: %v", model.sessions)
	}
	for _, got := range model.sessions {
		if got == "" || got == conv.ID || got != ID(conv.ID) {
			t.Fatalf("missing or identifying session: %q", got)
		}
	}
	other, err := repo.AcceptIncoming(ctx, domain.Incoming{ID: "other", Conversation: domain.Conversation{ID: "group:other"}}, 2)
	if err != nil || !other {
		t.Fatal(err)
	}
	_, err = a.ModelCall(ctx, ModelInput{Turn: Turn{Input: RunInput{Key: "other", ConversationID: "group:other"}}})
	if err != nil {
		t.Fatal(err)
	}
	if model.sessions[3] == model.sessions[0] {
		t.Fatal("different conversations share a session")
	}
}
