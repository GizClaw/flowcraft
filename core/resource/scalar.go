package resource

import (
	"encoding/json"
	"strconv"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Int is a settings scalar that accepts either a JSON number literal
// or a string. The string form is how an expanded ${env:NAME} value
// arrives: expansion replaces the reference with the environment
// variable's text, so a numeric field can consume env only through a
// type like this one. Literal JSON numbers keep their exact behavior.
type Int int

// UnmarshalJSON accepts a JSON number or a string, parsing the string
// with strconv.Atoi. A non-integer string is a validation error.
func (v *Int) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return errdefs.Validationf(
				"resource settings: int: %q: %v", s, err)
		}
		*v = Int(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = Int(n)
	return nil
}

// Bool is a settings scalar that accepts either a JSON boolean literal
// or a string (strconv.ParseBool: "true", "1", "false", "0", ...).
type Bool bool

// UnmarshalJSON accepts a JSON boolean or a string.
func (v *Bool) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return errdefs.Validationf(
				"resource settings: bool: %q: %v", s, err)
		}
		*v = Bool(b)
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	*v = Bool(b)
	return nil
}

// Float64 is a settings scalar that accepts either a JSON number
// literal or a string (strconv.ParseFloat).
type Float64 float64

// UnmarshalJSON accepts a JSON number or a string.
func (v *Float64) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return errdefs.Validationf(
				"resource settings: float: %q: %v", s, err)
		}
		*v = Float64(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	*v = Float64(f)
	return nil
}

// Int64 is a settings scalar that accepts either a JSON number literal
// or a string (strconv.ParseInt). Use it for 64-bit integer settings
// such as millisecond counters that must stay exact.
type Int64 int64

// UnmarshalJSON accepts a JSON number or a string.
func (v *Int64) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return errdefs.Validationf(
				"resource settings: int64: %q: %v", s, err)
		}
		*v = Int64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*v = Int64(n)
	return nil
}
