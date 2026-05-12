package openai

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"

	oai "github.com/openai/openai-go"
)

// classifyAPIError gives openai-go's *oai.Error (and its alias used by all
// OpenAI-compatible providers — azure, deepseek, qwen-chat, minimax-chat —
// which delegate to this package) a status-code-aware classification path.
//
// Why this lives here and not in sdk/errdefs:
//
//   - sdk/errdefs is provider-agnostic; importing openai-go there would
//     drag the SDK into every binary that just wants the errdefs sentinels.
//   - The generic [errdefs.ClassifyProviderError] does keyword scanning and
//     a regex over the error string, but openai-go's Error.Error() formats
//     as `<METHOD> "<URL>": <code> <status> <body>` — the "https://" prefix
//     puts an 's' between "http" and the 3-digit status code, so the
//     `\b(?:http|status)\s*(\d{3})\b` heuristic in errdefs misses the
//     code and falls through to the ProviderTransient default. The result
//     was that 400 / 404 / 422 errors got bucketed as NotAvailable
//     (retryable) instead of Validation (do-not-retry), which became a
//     real problem once the locomo runner started retrying NotAvailable
//     on its own.
//
// Mapping (StatusCode → errdefs class):
//
//	401, 403         → Unauthorized
//	402              → Forbidden  (billing-permanent, separate from 401)
//	429              → RateLimit
//	400, 405, 422    → Validation
//	404              → split by error body:
//	                     - empty Code  → NotAvailable (Azure MaaS capacity blip)
//	                     - non-empty   → Validation   (DeploymentNotFound etc.)
//	408, 409, >=500  → NotAvailable
//
// Falls back to [errdefs.ClassifyProviderError] for anything that isn't an
// *oai.Error (network errors, ctx errors that already round-tripped through
// errdefs.FromContext, etc.) so the existing keyword/regex fallbacks still
// catch their cases.
// classifyAPIError is the legacy package-level entry point. It delegates to
// classifyAPIErrorWithProvider using the bare "openai" tag and exists only
// for tests and code paths that don't have an *LLM in scope; production
// call sites should prefer (*LLM).classifyAPIError so sub-providers
// (azure / deepseek / qwen) get their real name on the wrapped error.
func classifyAPIError(err error) error {
	return classifyAPIErrorWithProvider("openai", err)
}

// classifyAPIError is the LLM-method variant that picks up the per-instance
// provider name set by WithProviderName, so sub-providers see their own tag
// in the fallback path.
func (c *LLM) classifyAPIError(err error) error {
	return classifyAPIErrorWithProvider(c.Provider(), err)
}

func classifyAPIErrorWithProvider(provider string, err error) error {
	if err == nil {
		return nil
	}
	if errdefs.HasClassification(err) {
		return err
	}
	var ae *oai.Error
	if !errors.As(err, &ae) {
		return errdefs.ClassifyProviderError(provider, err)
	}
	switch ae.StatusCode {
	case 401, 403:
		return errdefs.Unauthorized(err)
	case 402:
		return errdefs.Forbidden(err)
	case 429:
		return errdefs.RateLimit(err)
	case 400, 405, 422:
		return errdefs.Validation(err)
	case 404:
		// Two flavours of 404 in the wild, indistinguishable by status
		// code alone:
		//
		//  1. Azure AI Foundry / MaaS capacity blip — the Front Door
		//     layer answers with HTTP 404 and *no body* (or a generic
		//     "Resource not found" with no `error.code` field) while
		//     the deployment pod is cold-starting or scaling. These
		//     ARE transient — the very next request usually works,
		//     and the locomo runner's retry-once recovers them.
		//
		//  2. DeploymentNotFound / wrong path / model retired — the
		//     OpenAI service answers with a structured error body
		//     carrying `error.code` ("DeploymentNotFound",
		//     "model_not_found", ...). These are permanent: retrying
		//     just burns a second API call.
		//
		// `ae.Code` is the parsed `error.code` field; it's empty for
		// flavour (1) and populated for (2). Splitting here keeps the
		// Azure capacity case retryable without re-introducing the
		// "wrong deployment name silently looks like a flaky network"
		// failure mode.
		if ae.Code == "" {
			return errdefs.NotAvailable(err)
		}
		return errdefs.Validation(err)
	case 408, 409:
		return errdefs.NotAvailable(err)
	}
	if ae.StatusCode >= 500 {
		return errdefs.NotAvailable(err)
	}
	// Unknown 2xx/3xx status reaching this branch is a contract violation
	// of openai-go (it only constructs *oai.Error for non-2xx), but rather
	// than panicking we treat it as a generic provider error.
	return errdefs.ClassifyProviderError(provider, err)
}
