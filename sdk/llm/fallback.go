package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
)

const (
	defaultBreakerThreshold     = 3
	defaultBreakerCooldown      = 30 * time.Second
	defaultHalfOpenProbeTimeout = 60 * time.Second
)

// Sentinel errors for FallbackLLM failure modes. Both are *plain*
// stdlib errors so callers can do identity checks via
// errors.Is(err, ErrAllProvidersOpen) — per the errdefs convention,
// sentinels and behavioural classification are orthogonal and both
// must be preserved.
//
// At the call sites below the sentinels are wrapped with
// errdefs.NotAvailable so that the same value, once it crosses
// into pod / api land, also satisfies errdefs.IsNotAvailable and
// maps to HTTP 503 (a fallback chain that has run out is exactly the
// "service is temporarily unavailable, retry later" semantics, not a
// generic 500). The wrapping happens at the return sites — not here
// in the var block — because pre-wrapping would yield a single shared
// errNotAvailable value and break errors.Is identity for callers that
// kept the plain pointer reference. See allOpenError / allFailedError
// below for the helpers that perform the wrap.
var (
	ErrAllProvidersOpen   = errors.New("llm: all providers unavailable (circuit breaker open)")
	ErrAllProvidersFailed = errors.New("llm: all providers failed")
)

// allOpenError returns the canonical "every provider's breaker is
// open" terminal error: identity-equal to ErrAllProvidersOpen via
// errors.Is, and classified as NotAvailable so HTTPStatus emits 503
// instead of falling through to the default 500.
func allOpenError() error {
	return errdefs.NotAvailable(ErrAllProvidersOpen)
}

// allFailedError returns the "tried every provider, all failed"
// terminal error joined with the last underlying provider error.
// Identity-equal to ErrAllProvidersFailed AND to lastErr via
// errors.Is (the %w/%w wrapper keeps both unwrap branches), and
// classified as NotAvailable so the wire status is 503 even when
// lastErr itself carries a different errdefs class (e.g. a
// transient network error from one specific provider should not
// determine the chain-level wire shape — the chain decision is
// "service unavailable, please retry").
func allFailedError(lastErr error) error {
	return errdefs.NotAvailable(fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr))
}

// FallbackLLM decision policy.
//
// The HTTP-status / keyword / SDK-error → category dispatcher lives in
// sdk/errdefs/http.go because every external-provider boundary in the
// SDK reuses it. What FallbackLLM specifically needs on top is just
// three small policy decisions per category: should we move on to the
// next provider in the chain, how long should the breaker stay open
// after a trip, and what label to attach on per-category metrics.
// They are package-private helpers because nobody outside this file
// calls them — peers (sdkx/llm/*, sdkx/embedding/*, future
// sdkx/rerank, ...) consume the errdefs class directly via
// errdefs.IsXxx and don't need the LLM-fallback lens.

// shouldFallback reports whether a given category should try the next
// provider in the chain. Permanent and ContextOverflow stop the chain
// because every downstream provider will see the same input and fail
// the same way; everything else is worth trying again.
func shouldFallback(c errdefs.ProviderCategory) bool {
	switch c {
	case errdefs.ProviderPermanent, errdefs.ProviderContextOverflow:
		return false
	default:
		return true
	}
}

// cooldownMultiplier returns the per-category multiplier for the base
// breaker cooldown: Auth/Billing get a long penalty (the credentials
// won't fix themselves on the next call), RateLimit gets a moderate
// hold to let the upstream window roll over, everything else uses the
// configured base cooldown unchanged.
func cooldownMultiplier(c errdefs.ProviderCategory) int {
	switch c {
	case errdefs.ProviderAuth, errdefs.ProviderBilling:
		return 10
	case errdefs.ProviderRateLimit:
		return 3
	default:
		return 1
	}
}

// categoryLabel returns the stable short token attached to per-category
// metrics and log records ("rate_limit" / "auth" / "billing" /
// "context_overflow" / "permanent" / "transient"). Dashboards filter
// on these strings, so the set is part of FallbackLLM's observable
// contract — even though the function is unexported.
func categoryLabel(c errdefs.ProviderCategory) string {
	switch c {
	case errdefs.ProviderRateLimit:
		return "rate_limit"
	case errdefs.ProviderAuth:
		return "auth"
	case errdefs.ProviderBilling:
		return "billing"
	case errdefs.ProviderContextOverflow:
		return "context_overflow"
	case errdefs.ProviderPermanent:
		return "permanent"
	default:
		return "transient"
	}
}

// FallbackLLM wraps a primary LLM with ordered fallbacks. When the primary
// fails, fallbacks are tried in sequence. A built-in circuit breaker
// disables a provider temporarily after consecutive failures.
type FallbackLLM struct {
	providers []namedLLM
	breaker   map[string]*circuitState
	mu        sync.Mutex

	threshold       int
	cooldown        time.Duration
	halfOpenTimeout time.Duration

	stateGauge       metric.Int64Gauge
	tripsCount       metric.Int64Counter
	errorsByCategory metric.Int64Counter
}

type namedLLM struct {
	name string
	llm  LLM
}

type circuitState struct {
	failures         int
	openUntil        time.Time
	halfOpen         bool
	halfOpenDeadline time.Time // auto-reset halfOpen if probe doesn't complete in time
}

// FallbackOption configures a FallbackLLM.
type FallbackOption func(*FallbackLLM)

// WithBreakerThreshold sets consecutive failures before the breaker opens.
func WithBreakerThreshold(n int) FallbackOption {
	return func(f *FallbackLLM) {
		if n > 0 {
			f.threshold = n
		}
	}
}

// WithBreakerCooldown sets the duration before a tripped breaker enters half-open state.
func WithBreakerCooldown(d time.Duration) FallbackOption {
	return func(f *FallbackLLM) {
		if d > 0 {
			f.cooldown = d
		}
	}
}

// WithHalfOpenTimeout sets how long a half-open probe is allowed to run
// before the breaker resets and allows a new probe attempt.
func WithHalfOpenTimeout(d time.Duration) FallbackOption {
	return func(f *FallbackLLM) {
		if d > 0 {
			f.halfOpenTimeout = d
		}
	}
}

// FallbackEntry pairs a name with an LLM for use in NewFallbackLLM.
type FallbackEntry struct {
	Name string
	LLM  LLM
}

// NewFallbackLLM creates a fallback-aware LLM from a primary and fallbacks.
func NewFallbackLLM(primary LLM, primaryName string, fallbacks []FallbackEntry, opts ...FallbackOption) *FallbackLLM {
	providers := []namedLLM{{name: primaryName, llm: primary}}
	for _, fb := range fallbacks {
		providers = append(providers, namedLLM{name: fb.Name, llm: fb.LLM})
	}
	f := &FallbackLLM{
		providers:       providers,
		breaker:         make(map[string]*circuitState, len(providers)),
		threshold:       defaultBreakerThreshold,
		cooldown:        defaultBreakerCooldown,
		halfOpenTimeout: defaultHalfOpenProbeTimeout,
	}
	for _, opt := range opts {
		opt(f)
	}
	for _, p := range providers {
		f.breaker[p.name] = &circuitState{}
	}
	f.initMetrics()
	return f
}

func (f *FallbackLLM) initMetrics() {
	meter := telemetry.MeterWithSuffix("llm")
	f.stateGauge, _ = meter.Int64Gauge("circuit_breaker.state",
		metric.WithDescription("Circuit breaker state per provider: 0=closed, 1=half-open, 2=open"))
	f.tripsCount, _ = meter.Int64Counter("circuit_breaker.trips.total",
		metric.WithDescription("Total number of circuit breaker trips per provider"))
	f.errorsByCategory, _ = meter.Int64Counter("errors.total",
		metric.WithDescription("Total LLM errors by provider and error category"))
}

func (f *FallbackLLM) recordStateMetric(ctx context.Context, name string, cs *circuitState) {
	if f.stateGauge == nil {
		return
	}
	var state int64
	switch {
	case cs.failures >= f.threshold && time.Now().Before(cs.openUntil):
		state = 2 // open
	case cs.halfOpen:
		state = 1 // half-open
	default:
		state = 0 // closed
	}
	f.stateGauge.Record(ctx, state,
		metric.WithAttributes(attribute.String("provider", name)))
}

func (f *FallbackLLM) Generate(ctx context.Context, messages []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	var lastErr error
	for _, p := range f.providers {
		allow, halfOpen := f.canAttempt(ctx, p.name)
		if !allow {
			continue
		}
		resp, usage, err := p.llm.Generate(ctx, messages, opts...)
		if err == nil {
			f.recordSuccess(ctx, p.name)
			return resp, usage, nil
		}
		lastErr = err
		cat := errdefs.ClassifyProvider(err)
		f.recordErrorMetric(ctx, p.name, cat)
		if !shouldFallback(cat) {
			telemetry.Warn(ctx, "llm permanent error, skipping fallback",
				otellog.String("provider", p.name),
				otellog.String("category", categoryLabel(cat)),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return Message{}, TokenUsage{}, err
		}
		f.recordFailureWithCategory(ctx, p.name, halfOpen, cat)
		telemetry.Warn(ctx, "llm transient failure, trying fallback",
			otellog.String("provider", p.name),
			otellog.String("category", categoryLabel(cat)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}
	if lastErr == nil {
		return Message{}, TokenUsage{}, allOpenError()
	}
	return Message{}, TokenUsage{}, allFailedError(lastErr)
}

func (f *FallbackLLM) GenerateStream(ctx context.Context, messages []Message, opts ...GenerateOption) (StreamMessage, error) {
	var lastErr error
	for _, p := range f.providers {
		allow, halfOpen := f.canAttempt(ctx, p.name)
		if !allow {
			continue
		}
		stream, err := p.llm.GenerateStream(ctx, messages, opts...)
		if err == nil {
			if stream == nil {
				return nil, fmt.Errorf("llm: %s returned nil stream", p.name)
			}
			return &trackedStream{
				StreamMessage: stream,
				fallback:      f,
				provider:      p.name,
				halfOpen:      halfOpen,
				ctx:           ctx,
			}, nil
		}
		lastErr = err
		cat := errdefs.ClassifyProvider(err)
		f.recordErrorMetric(ctx, p.name, cat)
		if !shouldFallback(cat) {
			telemetry.Warn(ctx, "llm stream permanent error, skipping fallback",
				otellog.String("provider", p.name),
				otellog.String("category", categoryLabel(cat)),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return nil, err
		}
		f.recordFailureWithCategory(ctx, p.name, halfOpen, cat)
		telemetry.Warn(ctx, "llm stream transient failure, trying fallback",
			otellog.String("provider", p.name),
			otellog.String("category", categoryLabel(cat)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}
	if lastErr == nil {
		return nil, allOpenError()
	}
	return nil, allFailedError(lastErr)
}

// trackedStream wraps a StreamMessage to defer circuit breaker recording
// until the stream is fully consumed or encounters an error.
type trackedStream struct {
	StreamMessage
	fallback *FallbackLLM
	provider string
	halfOpen bool
	ctx      context.Context
	once     sync.Once
}

func (t *trackedStream) Next() bool {
	if t.StreamMessage.Next() {
		return true
	}
	t.finish()
	return false
}

func (t *trackedStream) Close() error {
	err := t.StreamMessage.Close()
	t.finish()
	return err
}

func (t *trackedStream) finish() {
	t.once.Do(func() {
		if err := t.Err(); err != nil {
			cat := errdefs.ClassifyProvider(err)
			t.fallback.recordErrorMetric(t.ctx, t.provider, cat)
			if shouldFallback(cat) {
				t.fallback.recordFailureWithCategory(t.ctx, t.provider, t.halfOpen, cat)
			}
		} else {
			t.fallback.recordSuccess(t.ctx, t.provider)
		}
	})
}

func (f *FallbackLLM) canAttempt(ctx context.Context, name string) (allow, halfOpen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.breaker[name]
	if cs.failures < f.threshold {
		return true, false
	}
	if time.Now().Before(cs.openUntil) {
		return false, false
	}
	if cs.halfOpen {
		if time.Now().After(cs.halfOpenDeadline) {
			// Probe timed out (hung/cancelled without recording result).
			// Reset to allow a new probe.
			cs.halfOpen = false
		} else {
			return false, false
		}
	}
	cs.halfOpen = true
	cs.halfOpenDeadline = time.Now().Add(f.halfOpenTimeout)
	f.recordStateMetric(ctx, name, cs)
	return true, true
}

func (f *FallbackLLM) recordFailureWithCategory(ctx context.Context, name string, wasHalfOpen bool, cat errdefs.ProviderCategory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.breaker[name]
	cooldown := f.cooldown * time.Duration(cooldownMultiplier(cat))
	if wasHalfOpen {
		cs.halfOpen = false
		cs.openUntil = time.Now().Add(cooldown)
		f.recordStateMetric(ctx, name, cs)
		return
	}
	cs.failures++
	if cs.failures >= f.threshold {
		cs.openUntil = time.Now().Add(cooldown)
		if f.tripsCount != nil {
			f.tripsCount.Add(ctx, 1,
				metric.WithAttributes(attribute.String("provider", name)))
		}
		f.recordStateMetric(ctx, name, cs)
	}
}

func (f *FallbackLLM) recordErrorMetric(ctx context.Context, name string, cat errdefs.ProviderCategory) {
	if f.errorsByCategory == nil {
		return
	}
	f.errorsByCategory.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("provider", name),
			attribute.String("category", categoryLabel(cat)),
		))
}

func (f *FallbackLLM) recordSuccess(ctx context.Context, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.breaker[name]
	cs.failures = 0
	cs.openUntil = time.Time{}
	cs.halfOpen = false
	f.recordStateMetric(ctx, name, cs)
}
