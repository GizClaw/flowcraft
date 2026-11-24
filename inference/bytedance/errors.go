package bytedance

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The Ark media surfaces report failures as a code and a message on a response
// that may still be HTTP 200, so the mapping reads those two fields. The code is
// the stable half and is tried first; the message is the fallback for codes the
// platform is still inventing.
//
// Matching is by phrase rather than by bare word, because the words are ordinary
// English: "generated" contains "rate", "unlimited" contains "limit", and
// "asterisk" contains "risk". Every term below is a phrase or a code name.
var (
	// The credential is wrong: another attempt cannot fix it, but a different
	// key can.
	arkAuthCodes = []string{"authenticationerror", "invalidapikey", "unauthorized", "authfailed"}
	arkAuthProse = []string{"api key", "credential"}

	// The account may not do this, permanently: nothing to wait out. This is
	// where the platform's quota refusals land, following the platform-wide rule
	// that billing and authorization refusals are permanent rather than
	// retryable (see core/errdefs).
	arkRefusedCodes = []string{
		"accessdenied", "accountoverdue", "arrearage", "insufficientbalance", "quotaexceeded",
	}
	arkRefusedProse = []string{
		"access denied", "insufficient balance", "arrearage", "account is overdue",
		"quota is exhausted",
	}

	// The request itself is wrong, or the platform refused its content: repeating
	// it anywhere produces the same refusal.
	arkInvalidCodes = []string{
		"invalidparameter", "notfound", "modelnotopen", "sensitivecontent", "filterfailed",
		"unsupportedmodel",
	}
	arkInvalidProse = []string{"invalid parameter", "not found", "sensitive content", "content filter"}

	// The far end is asking for a slower caller.
	arkThrottleCodes = []string{"ratelimitexceeded", "throttling", "toomanyrequests", "overloaded"}
	// tpm/rpm are the per-minute allowances the platform names in a refusal;
	// they are the words that tell a rate quota from an exhausted plan.
	arkThrottleProse = []string{"rate limit", "too many requests", "throttled", "overloaded", "tpm", "rpm"}
)

// classifyResponseError maps an Ark failure onto the contract's categories.
//
// The order is the order a caller acts on: a credential is replaced, a refusal is
// accepted, a bad request is fixed, a throttle is waited out. A quota code is the
// one that can read either way — an exhausted plan is permanent, a spent
// per-minute allowance is not — so a throttling phrase in its message decides,
// and the permanent reading is the default.
func classifyResponseError(code, message string) error {
	err := fmt.Errorf("bytedance: response failed: %s %s", code, message)
	if classified := arkCategory(code, message); classified != nil {
		return classified(err)
	}
	return errdefs.NotAvailable(err)
}

// arkCategory is the shared switch: nil when nothing matched, which leaves the
// failure as the far end being unwell.
func arkCategory(code, message string) func(error) error {
	name := normaliseCode(code)
	prose := strings.ToLower(message)
	named := func(terms []string) bool { return containsAny(name, terms) }
	said := func(terms []string) bool { return containsAny(prose, terms) }

	switch {
	case named(arkAuthCodes) || said(arkAuthProse):
		return errdefs.Unauthorized
	case named(arkThrottleCodes) || said(arkThrottleProse):
		return errdefs.RateLimit
	case named(arkRefusedCodes) || said(arkRefusedProse):
		return errdefs.Forbidden
	case named(arkInvalidCodes) || said(arkInvalidProse):
		return errdefs.Validation
	default:
		return nil
	}
}

// normaliseCode lower-cases a code and drops the separators vendors alternate
// between, so `RateLimitExceeded`, `rate_limit_exceeded` and `rateLimitExceeded`
// are one term.
func normaliseCode(code string) string {
	var builder strings.Builder
	builder.Grow(len(code))
	for _, r := range strings.ToLower(code) {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// containsAny reports whether s contains any of the terms. Each call passes a
// handful of literals, which is cheaper to read than a joined expression and
// cheaper to run than a regexp with the same alternatives.
func containsAny(s string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(s, term) {
			return true
		}
	}
	return false
}
