package agent

import (
	"context"
	"encoding/json"
	"errors"
	"openalbert/apps/backend/internal/domain"
)

const MaxContextMessageRunes = 32000

// CompactContext bounds Temporal Activity payloads while retaining the latest user
// request and complete tool exchanges. Older details remain in persisted history.
func CompactContext(messages []domain.Message) []domain.Message {
	out := append([]domain.Message(nil), messages...)
	// Artifact views are user messages too. Use the explicit turn marker rather
	// than the last user role to retain the original request and all its evidence.
	var turnID string
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].TurnID != "" {
			turnID = out[i].TurnID
			break
		}
	}
	protected := domain.ModelTurnStart(out, turnID)
	imageBudget := 400000
	for i := len(out) - 1; i >= 0; i-- {
		kept := []string{}
		for _, image := range out[i].Images {
			if i >= protected || len(image) <= imageBudget {
				kept = append(kept, image)
				imageBudget -= len(image)
			}
		}
		out[i].Images = kept
	}
	for i := range out {
		if i < protected && out[i].Role != "system" {
			out[i].Content = clip(out[i].Content, MaxContextMessageRunes)
		}
	}
	for len(out) > 2 {
		raw, _ := json.Marshal(out)
		if len(raw) <= 750000 {
			break
		}
		start, end := domain.OldestModelTurn(out, protected)
		if start == end {
			break
		}
		out = append(out[:start], out[end:]...)
		protected -= end - start
	}
	if raw, _ := json.Marshal(out); len(raw) > 750000 {
		// Full messages remain in persisted history. Carry a small explicit
		// failure across Temporal instead of exceeding its transport limit or
		// silently discarding evidence from the current agent turn.
		return []domain.Message{{TurnID: turnID, ContextOverflow: true}}
	}
	return out
}

type analysisContext struct {
	Existing   []domain.MemoryEntry `json:"existing_memories"`
	Transcript []domain.Message     `json:"transcript"`
}

func (a *Activities) fitAnalysisContext(ctx context.Context, instructions string, payload analysisContext) ([]domain.Message, analysisContext, error) {
	ctx = domain.WithModelRole(ctx, domain.RoleAnalysis)
	messages := []domain.Message{{Role: "system", Content: instructions}, {Role: "user"}}
	sizer, profileAware := a.Model.(domain.ModelContext)
	for {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, payload, err
		}
		messages[1].Content = string(raw)
		if !profileAware {
			return messages, payload, nil
		}
		fits, err := sizer.FitsContext(ctx, messages, nil)
		if err != nil {
			return nil, payload, err
		}
		if fits {
			return messages, payload, nil
		}
		// Retain a suffix of complete transcript records; never clip through JSON
		// or discard the newest entry. Deletion checks use the original transcript.
		if len(payload.Transcript) > 1 {
			payload.Transcript = payload.Transcript[1:]
		} else if len(payload.Existing) > 0 {
			payload.Existing = payload.Existing[:len(payload.Existing)-1]
		} else {
			return nil, payload, errors.New("latest analysis entry exceeds the profile context budget")
		}
	}
}
