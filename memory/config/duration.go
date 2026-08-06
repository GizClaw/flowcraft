package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a duration string such as "30s" or "2m". Unitless numbers
// are rejected on purpose: units belong in the config file.
type Duration time.Duration

// UnmarshalJSON rejects unitless numbers and parses Go duration strings.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\"")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}
