package provider

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"
)

type FallbackPolicy struct {
	MaxAttempts       int
	RetryBackoff      time.Duration
	CircuitBreakAfter int
	CircuitOpen       time.Duration
	ShouldRetry       func(error) bool
	ShouldFallback    func(error) bool
}

type Attempt struct {
	Provider      string `json:"provider"`
	Attempt       int    `json:"attempt"`
	Outcome       string `json:"outcome"`
	Retryable     bool   `json:"retryable,omitempty"`
	Fallbackable  bool   `json:"fallbackable,omitempty"`
	CircuitOpened bool   `json:"circuit_opened,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Report struct {
	Operation        string    `json:"operation"`
	SelectedProvider string    `json:"selected_provider,omitempty"`
	FallbackUsed     bool      `json:"fallback_used,omitempty"`
	Attempts         []Attempt `json:"attempts,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type Observer interface {
	OnProviderReport(Report)
}

type ObserverFunc func(Report)

func (f ObserverFunc) OnProviderReport(r Report) { f(r) }

type Recorder struct {
	mu      sync.Mutex
	reports map[string]Report
}

func NewRecorder() *Recorder {
	return &Recorder{reports: make(map[string]Report)}
}

func (r *Recorder) OnProviderReport(report Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reports == nil {
		r.reports = make(map[string]Report)
	}
	r.reports[report.Operation] = report
}

func (r *Recorder) Last(operation string) (Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	report, ok := r.reports[operation]
	return report, ok
}

type observerContextKey struct{}

func WithObserver(ctx context.Context, observer Observer) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	return context.WithValue(ctx, observerContextKey{}, observer)
}

func ObserverFromContext(ctx context.Context) Observer {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(observerContextKey{}).(Observer)
	return observer
}

func DefaultFallbackPolicy() FallbackPolicy {
	return FallbackPolicy{
		MaxAttempts:       1,
		CircuitBreakAfter: 2,
		CircuitOpen:       5 * time.Second,
		ShouldRetry:       IsRetryable,
		ShouldFallback:    CanFallback,
	}
}

func (p FallbackPolicy) Normalize() FallbackPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	if p.CircuitBreakAfter < 0 {
		p.CircuitBreakAfter = 0
	}
	if p.ShouldRetry == nil {
		p.ShouldRetry = IsRetryable
	}
	if p.ShouldFallback == nil {
		p.ShouldFallback = CanFallback
	}
	return p
}

func CanFallback(err error) bool {
	if err == nil {
		return false
	}
	var ce ClassifiedError
	if errors.As(err, &ce) {
		return ce.IsFallbackable()
	}
	return !errors.Is(err, context.Canceled)
}

func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ce ClassifiedError
	if errors.As(err, &ce) {
		return ce.IsRetryable()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, context.DeadlineExceeded):
		return true
	}
	code := ClassifyByMessage(err.Error())
	return code == "timeout" || code == "transport_error" || code == "provider_unavailable"
}

// IsProviderFault reports whether err should count against a provider's circuit
// breaker. Failures attributable to the caller/client — context cancellation and
// bad input (bad audio/codec/sample-rate) — must NOT penalize provider health;
// otherwise a client repeatedly sending bad input would trip a healthy
// provider's breaker (and cascade to fallbacks). Everything else (timeout,
// transport, provider_unavailable, internal_error) is treated as a
// provider-attributable fault.
func IsProviderFault(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ce ClassifiedError
	if errors.As(err, &ce) {
		return ce.ErrorCode() != "bad_audio"
	}
	return ClassifyByMessage(err.Error()) != "bad_audio"
}

type Circuit struct {
	mu     sync.Mutex
	states []circuitState
}

type circuitState struct {
	consecutiveFailures int
	openUntil           time.Time
	// halfOpen is set when the open window has elapsed and a single probe has
	// been allowed. While it is set, Allow holds every other caller so only one
	// probe is in flight at a time. A probe success (OnSuccess) fully closes the
	// circuit, while a probe failure (OnFailure) re-opens it immediately from a
	// clean failure count so recovery is gradual and the counter stays bounded.
	halfOpen bool
}

func NewCircuit(size int) *Circuit {
	if size < 0 {
		size = 0
	}
	return &Circuit{states: make([]circuitState, size)}
}

func (c *Circuit) Allow(index int, now time.Time) bool {
	if c == nil || index < 0 || index >= len(c.states) {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := &c.states[index]
	if state.openUntil.IsZero() {
		// A zero open window means the circuit is either fully closed or a
		// half-open probe is already in flight. When a probe is in flight, hold
		// every other caller (return false) until OnSuccess/OnFailure resolves
		// it, so exactly one probe reaches a provider that may still be
		// unhealthy instead of a post-cooldown burst.
		return !state.halfOpen
	}
	if now.Before(state.openUntil) {
		// Open: still within the cool-down window.
		return false
	}
	// Open window elapsed: transition to half-open and allow one probe. Decay
	// the failure count to zero so a probe failure re-opens from a clean slate
	// (bounding the counter) rather than compounding the previous streak.
	state.openUntil = time.Time{}
	state.consecutiveFailures = 0
	state.halfOpen = true
	return true
}

func (c *Circuit) OnSuccess(index int) {
	if c == nil || index < 0 || index >= len(c.states) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[index] = circuitState{}
}

// OnFailure records a provider failure and reports whether the circuit opened.
//
// attributable gates breaker accounting: only provider-attributable faults
// (see IsProviderFault) count toward consecutiveFailures. Retryability does NOT
// gate accounting — a persistently failing provider must eventually trip its
// breaker even when it returns hard (non-retryable) errors — but client-fault
// errors (bad input, caller cancellation) are excluded so they never open a
// healthy provider.
func (c *Circuit) OnFailure(index int, now time.Time, policy FallbackPolicy, attributable bool) bool {
	if c == nil || index < 0 || index >= len(c.states) {
		return false
	}
	if !attributable {
		return false
	}
	policy = policy.Normalize()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := &c.states[index]
	state.consecutiveFailures++

	canOpen := policy.CircuitBreakAfter > 0 && policy.CircuitOpen > 0
	opened := false
	switch {
	case state.halfOpen:
		// A half-open probe failed: re-open immediately from the clean count so
		// recovery restarts a fresh cool-down window instead of an ever-growing
		// streak.
		state.halfOpen = false
		if canOpen {
			state.openUntil = now.Add(policy.CircuitOpen)
			opened = true
		}
	case canOpen && state.consecutiveFailures >= policy.CircuitBreakAfter:
		state.openUntil = now.Add(policy.CircuitOpen)
		opened = true
	}
	return opened
}

func ProviderName(v any) string {
	if v == nil {
		return "unknown"
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() == "" {
		return t.Name()
	}
	if t.Name() == "" {
		return t.String()
	}
	return t.PkgPath() + "." + t.Name()
}
