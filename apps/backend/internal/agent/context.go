package agent

import (
	"context"
	"encoding/json"
	"errors"
	"openalbert/apps/backend/internal/domain"
)

const MaxContextMessageRunes = 32000

var ErrAnalysisContextLimit = errors.New("latest analysis entry exceeds the profile context budget")

// CompactContext bounds Temporal payloads by removing complete historical turns.
// The caller supplies the current turn key; old prepared payloads without that
// marker are ambiguous and must remain intact or fail with an overflow marker.
func CompactContext(messages []domain.Message, turnID string) []domain.Message {
	out := append([]domain.Message(nil), messages...)
	protected := 0
	if turnID != "" {
		protected = domain.ModelTurnStart(out, turnID)
	}
	// Drop over-budget historical image turns, including their text and replies,
	// before general eviction. Stripping just their images would misrepresent the
	// evidence and hide them from a text-only profile's compatibility check.
	imageBudget := 400000
	for _, m := range out[protected:] {
		for _, image := range m.Images {
			imageBudget -= len(image)
		}
	}
	type span struct{ start, end int }
	var turns []span
	for offset := 0; offset < protected; {
		start, end := domain.OldestModelTurn(out[offset:], protected-offset)
		if start == end {
			break
		}
		turns = append(turns, span{offset + start, offset + end})
		offset += end
	}
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		size := 0
		for _, m := range out[turn.start:turn.end] {
			for _, image := range m.Images {
				size += len(image)
			}
		}
		if size > 0 && size > imageBudget {
			out = append(out[:turn.start], out[turn.end:]...)
			protected -= turn.end - turn.start
		} else {
			imageBudget -= size
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
		return []domain.Message{{TurnID: turnID, ContextOverflow: true}}
	}
	return out
}

type analysisContext struct {
	SessionTrigger        string               `json:"session_trigger"`
	ProfileAndPreferences []domain.MemoryEntry `json:"profile_and_preferences,omitempty"`
	Existing              []domain.MemoryEntry `json:"existing_memories"`
	Transcript            []domain.Message     `json:"transcript"`
}

func (a *Activities) fitAnalysisContext(ctx context.Context, instructions string, payload analysisContext) ([]domain.Message, analysisContext, error) {
	ctx = domain.WithModelRole(ctx, domain.RoleAnalysis)
	messages := []domain.Message{{Role: "system", Content: instructions}, {Role: "user"}}
	sizer, profileAware := a.Model.(domain.ModelContext)
	transcript := payload.Transcript
	start := 0
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
			// A reduced memory snapshot may leave room for transcript records
			// discarded earlier. Restore the largest complete newest suffix.
			for start > 0 {
				candidate := payload
				candidate.Transcript = transcript[start-1:]
				raw, err := json.Marshal(candidate)
				if err != nil {
					return nil, payload, err
				}
				probe := []domain.Message{messages[0], {Role: "user", Content: string(raw)}}
				fits, err := sizer.FitsContext(ctx, probe, nil)
				if err != nil {
					return nil, payload, err
				}
				if !fits {
					break
				}
				start--
				payload, messages = candidate, probe
			}
			return messages, payload, nil
		}
		// Retain a suffix of complete transcript records; never clip through JSON
		// or discard the newest entry. Never drop profile/preference rules: fail
		// with ErrAnalysisContextLimit if the minimum transcript cannot fit with them.
		if len(payload.Transcript) > 1 {
			start++
			payload.Transcript = transcript[start:]
		} else if len(payload.Existing) > 0 {
			payload.Existing = payload.Existing[:len(payload.Existing)-1]
		} else {
			return nil, payload, ErrAnalysisContextLimit
		}
	}
}
