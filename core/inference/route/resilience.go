package route

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RetryPolicies holds one optional retry policy per operation. A nil entry
// disables retry for that operation (the pre-resilience behavior).
type RetryPolicies struct {
	Generate *RetryPolicy
	Embed    *RetryPolicy
}

func (p RetryPolicies) policyFor(operation inference.Operation) *RetryPolicy {
	switch operation {
	case inference.OperationGenerate:
		return p.Generate
	case inference.OperationEmbed:
		return p.Embed
	default:
		return nil
	}
}

// RetryDecision is the observable input to a Retryable predicate.
type RetryDecision struct {
	Operation        inference.Operation
	Phase            AttemptPhase
	Trigger          AttemptTrigger
	ErrorKind        inference.ErrorKind
	Err              error
	Attempt          int
	ObservableOutput bool
}

// RetryPolicy controls same-target retry for one operation.
type RetryPolicy struct {
	// MaxAttempts is the total logical attempts including the first.
	// Values below 2 disable retry.
	MaxAttempts int

	// Backoff controls the delay between attempts. The zero value selects
	// exponential backoff: 100ms seed, 2s cap, full jitter.
	Backoff Backoff

	// Retryable decides whether one failure may be retried on the same
	// target. Nil selects DefaultRetryable. Safety invariants (no retry
	// after observable output, no retry for compiler/validation/cancel
	// kinds) are enforced before this predicate is consulted.
	Retryable func(context.Context, RetryDecision) bool

	// FallbackOnRetryExhausted allows a transient provider failure to move
	// to the next target once its retry budget is exhausted. Default false
	// keeps the pre-resilience fallback boundary (compile rejections only).
	FallbackOnRetryExhausted bool

	// MaxTotalAttempts caps logical attempts across all targets for one
	// request. Zero disables the global cap; per-target MaxAttempts still
	// applies.
	MaxTotalAttempts int
}

func (p *RetryPolicy) effective() RetryPolicy {
	if p == nil {
		return RetryPolicy{MaxAttempts: 1}
	}
	effective := *p
	if effective.MaxAttempts < 1 {
		effective.MaxAttempts = 1
	}
	if effective.MaxTotalAttempts < 0 {
		effective.MaxTotalAttempts = 0
	}
	effective.Backoff = effective.Backoff.normalized()
	return effective
}

func (p *RetryPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.MaxAttempts < 0 {
		return fmt.Errorf("retry max attempts must not be negative")
	}
	if p.MaxTotalAttempts < 0 {
		return fmt.Errorf("retry max total attempts must not be negative")
	}
	return p.Backoff.validate()
}

// DefaultRetryable is the conservative same-target retry predicate: only
// ProviderFailure classified as rate limit, timeout, or not available, and
// never after observable output.
var DefaultRetryable = func(_ context.Context, decision RetryDecision) bool {
	if decision.ObservableOutput || decision.ErrorKind != inference.ProviderFailure {
		return false
	}
	return transientFailure(decision.Err)
}

func transientFailure(err error) bool {
	return errdefs.IsRateLimit(err) ||
		errdefs.IsTimeout(err) ||
		errdefs.IsNotAvailable(err)
}

func transientProviderFailure(err error) bool {
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) ||
		inferenceErr.Kind != inference.ProviderFailure {
		return false
	}
	return transientFailure(err)
}

func nonRetryableKind(kind inference.ErrorKind) bool {
	switch kind {
	case inference.InvalidRequest,
		inference.UnsupportedOperation,
		inference.UnsupportedFeature,
		inference.InvalidExtension,
		inference.UnknownProvider,
		inference.UnknownModel,
		inference.UnknownProfile,
		inference.PolicyDenied,
		inference.OperationInterrupted,
		inference.CompilerContractViolation,
		inference.InvalidProviderResponse:
		return true
	default:
		return false
	}
}

// retryEligible applies the shared safety invariants before consulting the
// policy's Retryable predicate.
func retryEligible(
	ctx context.Context,
	policy *RetryPolicy,
	decision RetryDecision,
) bool {
	if policy == nil ||
		policy.MaxAttempts < 2 ||
		decision.Attempt >= policy.MaxAttempts ||
		decision.ObservableOutput ||
		decision.Phase == AttemptPhasePreflight ||
		nonRetryableKind(decision.ErrorKind) {
		return false
	}
	retryable := policy.Retryable
	if retryable == nil {
		retryable = DefaultRetryable
	}
	return retryable(ctx, decision)
}

// BackoffKind selects the delay growth curve.
type BackoffKind string

const (
	BackoffFixed       BackoffKind = "fixed"
	BackoffExponential BackoffKind = "exponential"
)

// JitterKind selects how delay is randomized.
type JitterKind string

const (
	JitterNone  JitterKind = "none"
	JitterFull  JitterKind = "full"
	JitterEqual JitterKind = "equal"
)

// Backoff is the retry delay policy. Zero values select the defaults when
// used through RetryPolicy.
type Backoff struct {
	Kind       BackoffKind
	Initial    time.Duration
	Max        time.Duration
	Multiplier float64
	Jitter     JitterKind
}

func (b Backoff) normalized() Backoff {
	if b.Kind == "" {
		b.Kind = BackoffExponential
	}
	if b.Initial <= 0 {
		b.Initial = 100 * time.Millisecond
	}
	if b.Max <= 0 {
		b.Max = 2 * time.Second
	}
	if b.Multiplier <= 0 {
		b.Multiplier = 2
	}
	if b.Jitter == "" {
		b.Jitter = JitterFull
	}
	return b
}

func (b Backoff) validate() error {
	switch b.Kind {
	case "", BackoffFixed, BackoffExponential:
	default:
		return fmt.Errorf("unknown backoff kind %q", b.Kind)
	}
	switch b.Jitter {
	case "", JitterNone, JitterFull, JitterEqual:
	default:
		return fmt.Errorf("unknown backoff jitter %q", b.Jitter)
	}
	if b.Initial < 0 {
		return fmt.Errorf("backoff initial must not be negative")
	}
	if b.Max < 0 {
		return fmt.Errorf("backoff max must not be negative")
	}
	if b.Multiplier < 0 {
		return fmt.Errorf("backoff multiplier must not be negative")
	}
	return nil
}

// retryDelay returns the sleep before retry attempt number attempt (1 is the
// first retry after the initial attempt). A Retry-After hint on err overrides
// the policy backoff and may exceed Max.
func retryDelay(policy Backoff, attempt int, err error) time.Duration {
	if d, ok := errdefs.RetryAfter(err); ok {
		return d
	}
	backoff := policy.normalized()
	var delay time.Duration
	switch backoff.Kind {
	case BackoffFixed:
		delay = backoff.Initial
	default:
		growth := math.Pow(backoff.Multiplier, float64(attempt-1))
		delay = time.Duration(float64(backoff.Initial) * growth)
	}
	if delay > backoff.Max {
		delay = backoff.Max
	}
	if delay <= 0 {
		return 0
	}
	switch backoff.Jitter {
	case JitterNone:
		return delay
	case JitterEqual:
		half := delay / 2
		return half + time.Duration(rand.Int63n(int64(half)+1))
	default: // JitterFull
		return time.Duration(rand.Int63n(int64(delay) + 1))
	}
}

// UnmarshalJSON decodes the JSON-only wire form of Backoff. Durations use
// time.ParseDuration strings ("100ms", "2s").
func (b *Backoff) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw struct {
		Kind       BackoffKind `json:"kind"`
		Initial    string      `json:"initial"`
		Max        string      `json:"max"`
		Multiplier float64     `json:"multiplier"`
		Jitter     JitterKind  `json:"jitter"`
	}
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode backoff: %w", err)
	}
	var initial, max time.Duration
	var err error
	if raw.Initial != "" {
		initial, err = time.ParseDuration(raw.Initial)
		if err != nil {
			return fmt.Errorf("backoff initial: %w", err)
		}
	}
	if raw.Max != "" {
		max, err = time.ParseDuration(raw.Max)
		if err != nil {
			return fmt.Errorf("backoff max: %w", err)
		}
	}
	*b = Backoff{
		Kind:       raw.Kind,
		Initial:    initial,
		Max:        max,
		Multiplier: raw.Multiplier,
		Jitter:     raw.Jitter,
	}
	return nil
}

// CircuitBreakerConfig controls the per-target circuit breaker.
type CircuitBreakerConfig struct {
	// FailureThreshold is consecutive transient failures that open the
	// circuit. Zero selects the default of 5.
	FailureThreshold int

	// RecoveryWindow is how long an open circuit stays open before one
	// half-open probe is allowed. Zero selects the default of 30s.
	RecoveryWindow time.Duration

	// HalfOpenMaxProbes bounds concurrent probes while half-open. Zero
	// selects the default of 1.
	HalfOpenMaxProbes int
}

func (c CircuitBreakerConfig) normalized() CircuitBreakerConfig {
	if c.FailureThreshold < 1 {
		c.FailureThreshold = 5
	}
	if c.RecoveryWindow <= 0 {
		c.RecoveryWindow = 30 * time.Second
	}
	if c.HalfOpenMaxProbes < 1 {
		c.HalfOpenMaxProbes = 1
	}
	return c
}

func (c CircuitBreakerConfig) validate() error {
	if c.FailureThreshold < 0 {
		return fmt.Errorf("circuit breaker failure threshold must not be negative")
	}
	if c.RecoveryWindow < 0 {
		return fmt.Errorf("circuit breaker recovery window must not be negative")
	}
	if c.HalfOpenMaxProbes < 0 {
		return fmt.Errorf("circuit breaker half-open max probes must not be negative")
	}
	return nil
}

// UnmarshalJSON decodes the JSON-only wire form of CircuitBreakerConfig.
// RecoveryWindow uses a time.ParseDuration string.
func (c *CircuitBreakerConfig) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw struct {
		FailureThreshold  int    `json:"failure_threshold"`
		RecoveryWindow    string `json:"recovery_window"`
		HalfOpenMaxProbes int    `json:"half_open_max_probes"`
	}
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode circuit breaker: %w", err)
	}
	var recovery time.Duration
	if raw.RecoveryWindow != "" {
		var err error
		recovery, err = time.ParseDuration(raw.RecoveryWindow)
		if err != nil {
			return fmt.Errorf("circuit breaker recovery_window: %w", err)
		}
	}
	*c = CircuitBreakerConfig{
		FailureThreshold:  raw.FailureThreshold,
		RecoveryWindow:    recovery,
		HalfOpenMaxProbes: raw.HalfOpenMaxProbes,
	}
	return nil
}

// Option configures a Router at construction time.
type Option func(*routerOptions) error

type routerOptions struct {
	retry   RetryPolicies
	breaker *CircuitBreakerConfig
	clock   func() time.Time
	sleeper func(context.Context, time.Duration) error
}

// WithRetryPolicies enables retry/backoff for the operations covered by the
// supplied policies. A policy without its operation selector is rejected by
// New.
func WithRetryPolicies(policies RetryPolicies) Option {
	return func(configured *routerOptions) error {
		configured.retry = policies
		return nil
	}
}

// WithCircuitBreaker enables the per-target circuit breaker. Zero values in
// the config select the documented defaults.
func WithCircuitBreaker(config CircuitBreakerConfig) Option {
	return func(configured *routerOptions) error {
		if err := config.validate(); err != nil {
			return err
		}
		normalized := config.normalized()
		configured.breaker = &normalized
		return nil
	}
}

func withTestClock(clock func() time.Time) Option {
	return func(configured *routerOptions) error {
		configured.clock = clock
		return nil
	}
}

func withTestSleeper(sleeper func(context.Context, time.Duration) error) Option {
	return func(configured *routerOptions) error {
		configured.sleeper = sleeper
		return nil
	}
}

func defaultSleeper(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type circuitKey struct {
	operation inference.Operation
	model     inference.ModelRef
}

type circuitState string

const (
	circuitClosed   circuitState = "closed"
	circuitOpen     circuitState = "open"
	circuitHalfOpen circuitState = "half_open"
)

type circuitEntry struct {
	state    circuitState
	failures int
	openedAt time.Time
	probes   int
}

type circuitBreaker struct {
	config  CircuitBreakerConfig
	clock   func() time.Time
	mu      sync.Mutex
	entries map[circuitKey]*circuitEntry
}

func newCircuitBreaker(
	config CircuitBreakerConfig,
	clock func() time.Time,
) *circuitBreaker {
	if clock == nil {
		clock = time.Now
	}
	return &circuitBreaker{
		config:  config.normalized(),
		clock:   clock,
		entries: make(map[circuitKey]*circuitEntry),
	}
}

type breakerDecision struct {
	state   circuitState
	allowed bool
}

func (b *circuitBreaker) begin(
	ctx context.Context,
	key circuitKey,
) breakerDecision {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[key]
	if entry == nil {
		entry = &circuitEntry{state: circuitClosed}
		b.entries[key] = entry
	}
	now := b.clock()
	switch entry.state {
	case circuitClosed:
		return breakerDecision{state: circuitClosed, allowed: true}
	case circuitOpen:
		if now.Sub(entry.openedAt) >= b.config.RecoveryWindow {
			entry.state = circuitHalfOpen
			entry.probes = 1
			recordCircuitMetric(ctx, routeCircuitProbes, key)
			return breakerDecision{state: circuitHalfOpen, allowed: true}
		}
		recordCircuitMetric(ctx, routeCircuitSkips, key)
		return breakerDecision{state: circuitOpen, allowed: false}
	case circuitHalfOpen:
		if entry.probes < b.config.HalfOpenMaxProbes {
			entry.probes++
			recordCircuitMetric(ctx, routeCircuitProbes, key)
			return breakerDecision{state: circuitHalfOpen, allowed: true}
		}
		recordCircuitMetric(ctx, routeCircuitSkips, key)
		return breakerDecision{state: circuitHalfOpen, allowed: false}
	default:
		recordCircuitMetric(ctx, routeCircuitSkips, key)
		return breakerDecision{state: entry.state, allowed: false}
	}
}

// finish records the outcome of an allowed attempt. counted marks failures
// that should feed the breaker (transient provider failures only). A nil err
// closes the circuit and resets the failure count. The returned string is a
// circuit transition ("open" or "closed") when this outcome changed the
// circuit state, otherwise empty.
func (b *circuitBreaker) finish(
	ctx context.Context,
	key circuitKey,
	err error,
	counted bool,
) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[key]
	if entry == nil {
		return ""
	}
	now := b.clock()
	switch entry.state {
	case circuitHalfOpen:
		if entry.probes > 0 {
			entry.probes--
		}
		if err == nil {
			entry.state = circuitClosed
			entry.failures = 0
			return "closed"
		}
		if counted {
			entry.state = circuitOpen
			entry.openedAt = now
			recordCircuitMetric(ctx, routeCircuitOpens, key)
			return "open"
		}
	case circuitClosed:
		if err == nil {
			entry.failures = 0
			return ""
		}
		if counted {
			entry.failures++
			if entry.failures >= b.config.FailureThreshold {
				entry.state = circuitOpen
				entry.openedAt = now
				recordCircuitMetric(ctx, routeCircuitOpens, key)
				return "open"
			}
		}
	}
	return ""
}

func (b *circuitBreaker) isOpen(key circuitKey) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.entries[key]
	if entry == nil || entry.state != circuitOpen {
		return false
	}
	return b.clock().Sub(entry.openedAt) < b.config.RecoveryWindow
}

func (b *circuitBreaker) reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.entries)
}

func recordCircuitMetric(
	ctx context.Context,
	counter metric.Int64Counter,
	key circuitKey,
) {
	counter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("inference.operation", string(key.operation)),
		attribute.String(telemetry.AttrLLMProvider, key.model.ID.Provider),
		attribute.String(telemetry.AttrLLMModel, key.model.ID.Name),
	))
}
