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

type Circuit struct {
	mu     sync.Mutex
	states []circuitState
}

type circuitState struct {
	consecutiveFailures int
	openUntil           time.Time
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
	return !now.Before(c.states[index].openUntil)
}

func (c *Circuit) OnSuccess(index int) {
	if c == nil || index < 0 || index >= len(c.states) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states[index] = circuitState{}
}

func (c *Circuit) OnFailure(index int, now time.Time, policy FallbackPolicy, retryable bool) bool {
	if c == nil || index < 0 || index >= len(c.states) || !retryable {
		return false
	}
	policy = policy.Normalize()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := &c.states[index]
	state.consecutiveFailures++
	opened := false
	if policy.CircuitBreakAfter > 0 &&
		policy.CircuitOpen > 0 &&
		state.consecutiveFailures >= policy.CircuitBreakAfter {
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
