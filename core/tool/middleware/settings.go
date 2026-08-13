package middleware

import (
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/tool"
)

// Settings declares the built-in middleware chain. Each entry is
// optional; absent entries are skipped.
type Settings struct {
	Recover     *RecoverSettings     `json:"recover,omitempty"`
	Timeout     *TimeoutSettings     `json:"timeout,omitempty"`
	Concurrency *ConcurrencySettings `json:"concurrency,omitempty"`
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

// FromSettings builds the middleware chain declared by s, outermost
// first.
func FromSettings(s Settings) ([]tool.Middleware, error) {
	var mws []tool.Middleware
	if s.Recover != nil && s.Recover.Enabled {
		mws = append(mws, Recover())
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
