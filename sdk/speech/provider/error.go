package provider

import "strings"

// ClassifiedError is implemented by errors that carry structured classification,
// allowing ClassifyError and IsRetryable to avoid fragile string matching.
type ClassifiedError interface {
	error
	ErrorCode() string
	IsRetryable() bool
	IsFallbackable() bool
}

// ProviderError is a structured error for provider operations.
type ProviderError struct {
	Code     string
	Op       string
	Message  string
	Cause    error
	Retry    bool
	Fallback bool
}

func (e *ProviderError) Error() string {
	s := e.Op + ": " + e.Message
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

func (e *ProviderError) Unwrap() error        { return e.Cause }
func (e *ProviderError) ErrorCode() string    { return e.Code }
func (e *ProviderError) IsRetryable() bool    { return e.Retry }
func (e *ProviderError) IsFallbackable() bool { return e.Fallback }

// ClassifyByMessage categorizes an error by substring matching on its message.
// Returns one of: "timeout", "transport_error", "provider_unavailable",
// "bad_audio", or "internal_error".
func ClassifyByMessage(msg string) string {
	msg = strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "connection"), strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "closed pipe"), strings.Contains(msg, "eof"),
		strings.Contains(msg, "transport"), strings.Contains(msg, "websocket"):
		return "transport_error"
	case strings.Contains(msg, "unavailable"), strings.Contains(msg, "503"),
		strings.Contains(msg, "502"), strings.Contains(msg, "429"), strings.Contains(msg, "rate limit"):
		return "provider_unavailable"
	case strings.Contains(msg, "audio"), strings.Contains(msg, "codec"), strings.Contains(msg, "sample rate"):
		return "bad_audio"
	default:
		return "internal_error"
	}
}
