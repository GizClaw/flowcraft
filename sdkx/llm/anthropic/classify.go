package anthropic

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	anth "github.com/anthropics/anthropic-sdk-go"
)

// classifyAPIError gives anthropic-sdk-go's *anth.Error a status-code-aware
// classification path that the generic [errdefs.ClassifyProviderError]
// misses.
//
// The bug it fixes is the same one that bites openai-go (see the sibling
// sdkx/llm/openai/classify.go for the full story): the SDK's Error.Error()
// formats as `<METHOD> "<URL>": <code> <status> <body>`, the URL prefix
// contains "https" which traps the `\b(?:http|status)\s*(\d{3})\b` regex,
// the keyword scan misses generic 400 / 404 bodies, and everything falls
// through to ProviderTransient → NotAvailable. With the locomo runner's
// new retry-once-on-NotAvailable that misclassification turned a real
// 400 misconfig into "silently retry then drop"; this routes the codes
// through their proper buckets.
//
// Mapping (StatusCode → errdefs class):
//
//	401, 403         → Unauthorized
//	402              → Forbidden
//	429              → RateLimit
//	400, 404, 405, 422 → Validation
//	408, 409, >=500  → NotAvailable
//
// Unlike openai-go's Error, *anth.Error has no `Code` field — Anthropic's
// own backend doesn't have the Azure-MaaS-style cold-start 404 problem,
// so all 4xx (incl. 404) are treated as permanent client errors.
func classifyAPIError(err error) error {
	if err == nil {
		return nil
	}
	if errdefs.HasClassification(err) {
		return err
	}
	var ae *anth.Error
	if !errors.As(err, &ae) {
		return errdefs.ClassifyProviderError("anthropic", err)
	}
	switch ae.StatusCode {
	case 401, 403:
		return errdefs.Unauthorized(err)
	case 402:
		return errdefs.Forbidden(err)
	case 429:
		return errdefs.RateLimit(err)
	case 400, 404, 405, 422:
		return errdefs.Validation(err)
	case 408, 409:
		return errdefs.NotAvailable(err)
	}
	if ae.StatusCode >= 500 {
		return errdefs.NotAvailable(err)
	}
	return errdefs.ClassifyProviderError("anthropic", err)
}
