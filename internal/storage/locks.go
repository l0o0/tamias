package storage

import "context"

type lockTokensContextKey struct{}

// WithLockTokens attaches WebDAV lock tokens to a Core operation context. Core
// write paths use these tokens while checking the shared DAV lock manager.
func WithLockTokens(ctx context.Context, tokens []string) context.Context {
	copyOfTokens := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		copyOfTokens = append(copyOfTokens, token)
	}
	return context.WithValue(ctx, lockTokensContextKey{}, copyOfTokens)
}

// LockTokens returns the immutable token list attached to a Core operation.
func LockTokens(ctx context.Context) []string {
	tokens, _ := ctx.Value(lockTokensContextKey{}).([]string)
	return append([]string(nil), tokens...)
}
