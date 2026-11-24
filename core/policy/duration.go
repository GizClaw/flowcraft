package policy

import (
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Duration is a time.Duration that reads and writes as a Go duration string.
//
// A Grant is the deployment document itself, so the document has to be readable:
// time.Duration marshals as nanoseconds ("10000000000" for ten seconds), which is
// both unreadable and impossible to hand-edit without counting zeros. Duration
// keeps the standard library type for arithmetic and stores "10s" on the wire.
//
// The zero value is "unset" and is omitted from a document, which is what makes
// "zero means the default" expressible on both sides.
type Duration time.Duration

// Std returns the standard library value.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the duration in the form the document uses.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalText renders the duration for a YAML document.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses a duration such as "10s" or "1m30s".
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return errdefs.Validationf("policy: duration %q: %w", text, err)
	}
	if parsed < 0 {
		return errdefs.Validationf("policy: duration %q must not be negative", text)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON renders the duration as a string, for the JSON-semantics loader.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON parses a duration from a JSON document.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errdefs.Validationf("policy: duration must be a string: %w", err)
	}
	return d.UnmarshalText([]byte(text))
}
