package model

import "fmt"

// ModelLimits declares numeric capacity limits of a model. A nil field
// means the limit is undeclared rather than zero: the provider catalog did
// not claim a value, so callers must not assume an upper bound.
type ModelLimits struct {
	// MaxInputTokens caps the tokens a request may feed to the model as
	// input context (prompt plus any prior turns). Nil when the provider
	// catalog does not declare a limit.
	MaxInputTokens *int `json:"max_input_tokens,omitempty"`
	// MaxOutputTokens caps the tokens a model may emit in a single response
	// (including reasoning tokens when the provider counts them against the
	// same budget). Nil when the provider catalog does not declare a limit.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
}

func (l ModelLimits) Clone() ModelLimits {
	return ModelLimits{
		MaxInputTokens:  ClonePointer(l.MaxInputTokens),
		MaxOutputTokens: ClonePointer(l.MaxOutputTokens),
	}
}

// WithMaxInputTokens returns limits declaring the input-context window.
// Values at or below zero leave the window undeclared (the conservative
// zero declaration), matching the built-in catalog convention.
func (l ModelLimits) WithMaxInputTokens(value int) ModelLimits {
	if value > 0 {
		l.MaxInputTokens = &value
	}
	return l
}

// WithMaxOutputTokens returns limits declaring the output window. Values at
// or below zero leave the window undeclared.
func (l ModelLimits) WithMaxOutputTokens(value int) ModelLimits {
	if value > 0 {
		l.MaxOutputTokens = &value
	}
	return l
}

// Values returns the declared windows as plain ints; undeclared windows
// read as zero. Resolved catalog entries and their tests use it to compare
// limits without dereferencing pointers.
func (l ModelLimits) Values() (maxInputTokens, maxOutputTokens int) {
	maxInputTokens, maxOutputTokens = 0, 0
	if l.MaxInputTokens != nil {
		maxInputTokens = *l.MaxInputTokens
	}
	if l.MaxOutputTokens != nil {
		maxOutputTokens = *l.MaxOutputTokens
	}
	return maxInputTokens, maxOutputTokens
}

func (l ModelLimits) Validate() error {
	if l.MaxInputTokens != nil && *l.MaxInputTokens <= 0 {
		return fmt.Errorf("max input tokens must be positive")
	}
	if l.MaxOutputTokens != nil && *l.MaxOutputTokens <= 0 {
		return fmt.Errorf("max output tokens must be positive")
	}
	return nil
}
