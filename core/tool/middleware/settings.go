package middleware

import (
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Settings declares the built-in middleware chain. Each entry is
// optional; absent entries are skipped, with one exception: non-text
// result parts are bounded even when ResultLimit is absent (see
// ResultPartBudgetBytes).
type Settings struct {
	Recover     *RecoverSettings     `json:"recover,omitempty"`
	Timeout     *TimeoutSettings     `json:"timeout,omitempty"`
	Concurrency *ConcurrencySettings `json:"concurrency,omitempty"`
	Telemetry   *TelemetrySettings   `json:"telemetry,omitempty"`
	ResultLimit *ResultLimitSettings `json:"result_limit,omitempty"`
	// ResultPartBudgetBytes caps the encoded size of one result's non-text
	// parts when ResultLimit declares no text budget. Non-text parts are
	// bounded by default ([DefaultResultPartBudget]) because a tool result is
	// replayed in every later turn's context and media has no natural size;
	// this field moves that bound, and 0 lifts it. It is ignored when
	// ResultLimit is set, which carries its own part_budget_bytes.
	ResultPartBudgetBytes *int `json:"result_part_budget_bytes,omitempty"`
}

type RecoverSettings struct {
	Enabled bool `json:"enabled"`
}

type TimeoutSettings struct {
	// Default is the per-call deadline as a Go duration string
	// ("30s", "2m"). Calls that already carry a deadline pass through.
	Default string `json:"default,omitempty"`
}

type ConcurrencySettings struct {
	Limit int `json:"limit"`
}

// TelemetrySettings enables the standard per-call observability
// middleware: one span per execution plus executions/duration/error
// metrics and a warning log for failed calls.
type TelemetrySettings struct {
	Enabled bool `json:"enabled"`
}

// ResultLimitSettings caps how large a tool result may be, the
// settings form of [ResultLimiter]. Text is metered in runes; non-text
// parts (images, audio, video, file references, structured data) are
// metered by encoded size against a byte budget. Whatever does not fit
// is dropped and the truncation marker is appended, so the model learns
// the result was shortened.
type ResultLimitSettings struct {
	// Max is the rune budget for the text parts of one result. It is
	// required and must be positive.
	Max int `json:"max"`
	// Marker replaces the default truncation marker. An empty marker
	// falls back to DefaultResultMarker.
	Marker string `json:"marker,omitempty"`
	// PartBudgetBytes is the byte budget shared by the non-text parts of
	// one result. Absent means DefaultResultPartBudget; 0 lifts the cap.
	PartBudgetBytes *int `json:"part_budget_bytes,omitempty"`
}

// FromSettings builds the middleware chain declared by s, outermost
// first.
func FromSettings(s Settings) ([]tool.Middleware, error) {
	var mws []tool.Middleware
	if s.Recover != nil && s.Recover.Enabled {
		mws = append(mws, Recover())
	}
	if s.Telemetry != nil && s.Telemetry.Enabled {
		mws = append(mws, Telemetry())
	}
	if s.ResultLimit != nil {
		// Inside Recover and Telemetry, outside Timeout: the timeout's own
		// error result has to be bounded like any other result.
		if s.ResultLimit.Max <= 0 {
			return nil, errdefs.Validationf(
				"tool middleware: result_limit.max must be positive, got %d",
				s.ResultLimit.Max)
		}
		opts := make([]ResultLimitOption, 0, 2)
		if s.ResultLimit.Marker != "" {
			opts = append(opts, WithResultMarker(s.ResultLimit.Marker))
		}
		if budget := s.ResultLimit.PartBudgetBytes; budget != nil {
			opts = append(opts, WithResultPartBudget(*budget))
		}
		mws = append(mws, ResultLimiter(s.ResultLimit.Max, opts...))
	} else if budget := partBudget(s.ResultPartBudgetBytes); budget > 0 {
		// Text is unbounded unless the deployment asks for a rune budget, but
		// media is bounded by default so a tool cannot hand the model an
		// unbounded context item by returning one.
		mws = append(mws, ResultPartLimiter(budget))
	}
	if s.Timeout != nil && s.Timeout.Default != "" {
		d, err := time.ParseDuration(s.Timeout.Default)
		if err != nil {
			return nil, errdefs.Validationf(
				"tool middleware: timeout.default: %v", err)
		}
		mws = append(mws, Timeout(d, nil))
	}
	if s.Concurrency != nil && s.Concurrency.Limit > 0 {
		mws = append(mws, Concurrency(s.Concurrency.Limit))
	}
	return mws, nil
}

// partBudget resolves the non-text byte budget: an explicit value wins, and
// an absent one keeps the default.
func partBudget(configured *int) int {
	if configured == nil {
		return DefaultResultPartBudget
	}
	return *configured
}
