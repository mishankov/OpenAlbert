package domain

import "context"

type modelSessionKey struct{}

// WithModelSession associates model calls, including auxiliary analysis, with an
// opaque conversation identifier. It never contains user names or access tokens.
func WithModelSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, modelSessionKey{}, id)
}
func ModelSession(ctx context.Context) string {
	id, _ := ctx.Value(modelSessionKey{}).(string)
	return id
}
