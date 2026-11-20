package domain

import "context"

type modelSessionKey struct{}
type modelRoleKey struct{}
type modelTurnKey struct{}

// WithModelTurn identifies the current agent turn so context fitting preserves
// its original request and all subsequent tool results and artifact views.
func WithModelTurn(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, modelTurnKey{}, id)
}

func ModelTurn(ctx context.Context) string {
	id, _ := ctx.Value(modelTurnKey{}).(string)
	return id
}

func WithModelRole(ctx context.Context, role ModelRole) context.Context {
	return context.WithValue(ctx, modelRoleKey{}, role)
}

func ModelRoleFromContext(ctx context.Context) ModelRole {
	role, _ := ctx.Value(modelRoleKey{}).(ModelRole)
	return role
}

// WithModelSession associates model calls, including auxiliary analysis, with an
// opaque conversation identifier. It never contains user names or access tokens.
func WithModelSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, modelSessionKey{}, id)
}
func ModelSession(ctx context.Context) string {
	id, _ := ctx.Value(modelSessionKey{}).(string)
	return id
}

// ModelTurnStart protects the entire current turn, including batched user
// messages and synthetic artifact views. A missing explicit marker is ambiguous.
func ModelTurnStart(messages []Message, turn string) int {
	if turn != "" {
		for i, m := range messages {
			if m.TurnID == turn {
				return i
			}
		}
		return 0
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return 0
}

// OldestModelTurn returns one removable historical turn, never an instruction.
// Unmarked user messages cannot identify a new turn: they may be a batch part or
// artifact view. Treat an unmarked historical block as one ambiguous unit.
func OldestModelTurn(messages []Message, protected int) (start, end int) {
	for start = 0; start < protected; start++ {
		role := messages[start].Role
		if role == "user" || role == "assistant" || role == "tool" {
			break
		}
	}
	if start == protected {
		return start, start
	}
	turn := messages[start].TurnID
	for end = start + 1; end < protected; end++ {
		m := messages[end]
		if m.Role != "user" && m.Role != "assistant" && m.Role != "tool" {
			break
		}
		if m.TurnID != "" && m.TurnID != turn {
			break
		}
	}
	return start, end
}
