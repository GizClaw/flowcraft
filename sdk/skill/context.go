package skill

import "context"

type whitelistKey struct{}

// WithWhitelist stores a per-run skill allowlist in ctx. An empty
// allowlist means unrestricted discovery.
func WithWhitelist(ctx context.Context, names []string) context.Context {
	if len(names) == 0 {
		return context.WithValue(ctx, whitelistKey{}, []string(nil))
	}
	cp := make([]string, len(names))
	copy(cp, names)
	return context.WithValue(ctx, whitelistKey{}, cp)
}

// WhitelistFrom returns the per-run skill allowlist, if one was set.
func WhitelistFrom(ctx context.Context) []string {
	names, _ := ctx.Value(whitelistKey{}).([]string)
	if len(names) == 0 {
		return nil
	}
	cp := make([]string, len(names))
	copy(cp, names)
	return cp
}
