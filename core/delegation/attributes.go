package delegation

import (
	"context"
	"maps"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// runAttributesContextKey carries caller-declared run attributes from the
// tool middleware that stamps them to the tool that builds a Request.
type runAttributesContextKey struct{}

// WithRunAttributes returns a context carrying attributes that the
// delegation tool copies onto every Request it builds. Tool middleware is
// the intended producer: it is the last seam that still sees the caller's
// context before the service is called, so a deployment can stamp its
// execution policy (plus any other run-scoped metadata) without forking the
// delegate tool.
//
// The map is copied, so later mutation by the caller does not affect the
// context. A nil or empty map clears any value stamped further out.
func WithRunAttributes(ctx context.Context, attributes map[string]string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runAttributesContextKey{}, maps.Clone(attributes))
}

// RunAttributesFromContext returns a copy of the attributes stamped by
// [WithRunAttributes]. ok is false when nothing was stamped.
func RunAttributesFromContext(ctx context.Context) (map[string]string, bool) {
	if ctx == nil {
		return nil, false
	}
	attributes, ok := ctx.Value(runAttributesContextKey{}).(map[string]string)
	if !ok || len(attributes) == 0 {
		return nil, false
	}
	return maps.Clone(attributes), true
}

// mergeRunAttributes layers caller-declared attributes under the
// service-owned lineage keys. Lineage describes the delegation structure
// itself, so a declared attribute cannot override it.
func mergeRunAttributes(lineage, declared map[string]string) map[string]string {
	if len(lineage) == 0 && len(declared) == 0 {
		return nil
	}
	merged := make(map[string]string, len(lineage)+len(declared))
	maps.Copy(merged, declared)
	maps.Copy(merged, lineage)
	return merged
}

// lineageOwnedAttributes lists the run attribute keys the service derives
// from the caller's ambient execution context. They identify one specific
// tool call, so resume identity has to ignore them.
var lineageOwnedAttributes = []string{telemetry.AttrToolCallID}

// withoutLineageAttributes drops the service-owned lineage keys, leaving
// only the caller-declared policy surface. Nil when nothing remains.
func withoutLineageAttributes(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	declared := maps.Clone(attributes)
	for _, key := range lineageOwnedAttributes {
		delete(declared, key)
	}
	if len(declared) == 0 {
		return nil
	}
	return declared
}

// validateAttributes rejects keys and values that would change meaning
// once trimmed: attribute maps are compared verbatim for idempotency and
// resume identity, so "policy" and " policy " must not both be accepted.
// An empty value is allowed and is distinct from an absent key.
func validateAttributes(attributes map[string]string) error {
	for key, value := range attributes {
		if strings.TrimSpace(key) == "" {
			return errdefs.Validationf("delegation: attribute key must not be blank")
		}
		if key != strings.TrimSpace(key) {
			return errdefs.Validationf(
				"delegation: attribute key %q has surrounding whitespace", key)
		}
		if value != strings.TrimSpace(value) {
			return errdefs.Validationf(
				"delegation: attribute %q value has surrounding whitespace", key)
		}
	}
	return nil
}
