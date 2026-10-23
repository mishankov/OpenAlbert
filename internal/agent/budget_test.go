package agent

import (
	"context"
	"testing"

	"jarvis/internal/domain"
)

type budgetModelSpy struct{ tools [][]domain.Tool }

func (m *budgetModelSpy) Complete(_ context.Context, _ []domain.Message, tools []domain.Tool) (domain.Completion, error) {
	m.tools = append(m.tools, tools)
	return domain.Completion{Message: domain.Message{Role: "assistant", Content: "internal"}}, nil
}

func TestFinalizationOnlyAdvertisesDeliveryAndHasIndependentCheckpoint(t *testing.T) {
	a, _, _, conv := fixture(t)
	model := &budgetModelSpy{}
	a.Model = model
	in := ModelInput{Turn: Turn{Conversation: conv, Input: RunInput{Key: "budget", SessionID: "session", ConversationID: conv.ID}}}
	if _, err := a.ModelCall(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	in.Finalize = true
	for i := 0; i < 2; i++ {
		if _, err := a.ModelCall(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if len(model.tools) != 2 || len(model.tools[0]) <= 1 || len(model.tools[1]) != 1 || model.tools[1][0].Name != "send_message" {
		t.Fatalf("finalization reused research cache or tools: %+v", model.tools)
	}
}
