package skill

import (
	"context"
	"fmt"
)

// checkWhitelist validates if a skill name is allowed.
// Note: whitelist should already be resolved (either from field or context) before calling.
// Returns nil if allowed, error if not allowed or not in whitelist.
func checkWhitelist(_ context.Context, whitelist []string, skillName string) error {
	if len(whitelist) == 0 {
		return nil // no whitelist configured, allow all
	}
	for _, allowed := range whitelist {
		if allowed == skillName {
			return nil
		}
	}
	return fmt.Errorf("skill %q not in whitelist", skillName)
}

type ctxKey int

const ctxKeySkillWhitelist ctxKey = iota

// WithSkillWhitelist injects a per-app skill whitelist into the context.
func WithSkillWhitelist(ctx context.Context, whitelist []string) context.Context {
	return context.WithValue(ctx, ctxKeySkillWhitelist, whitelist)
}

// SkillWhitelistFrom extracts the skill whitelist from the context.
func SkillWhitelistFrom(ctx context.Context) []string {
	wl, _ := ctx.Value(ctxKeySkillWhitelist).([]string)
	return wl
}
