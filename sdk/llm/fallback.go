package llm

import (
	"context"
	"fmt"
	"sync"
	"time"

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
		cat := ClassifyError(err)
		f.recordErrorMetric(ctx, p.name, cat)
		if !ShouldFallback(cat) {
			telemetry.Warn(ctx, "llm permanent error, skipping fallback",
				otellog.String("provider", p.name),
				otellog.String("category", CategoryString(cat)),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return Message{}, TokenUsage{}, err
		}
		f.recordFailureWithCategory(ctx, p.name, halfOpen, cat)
		telemetry.Warn(ctx, "llm transient failure, trying fallback",
			otellog.String("provider", p.name),
			otellog.String("category", CategoryString(cat)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}
	if lastErr == nil {
		return Message{}, TokenUsage{}, ErrAllProvidersOpen
	}
	return Message{}, TokenUsage{}, fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr)
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
		cat := ClassifyError(err)
		f.recordErrorMetric(ctx, p.name, cat)
		if !ShouldFallback(cat) {
			telemetry.Warn(ctx, "llm stream permanent error, skipping fallback",
				otellog.String("provider", p.name),
				otellog.String("category", CategoryString(cat)),
				otellog.String(telemetry.AttrErrorMessage, err.Error()))
			return nil, err
		}
		f.recordFailureWithCategory(ctx, p.name, halfOpen, cat)
		telemetry.Warn(ctx, "llm stream transient failure, trying fallback",
			otellog.String("provider", p.name),
			otellog.String("category", CategoryString(cat)),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
	}
	if lastErr == nil {
		return nil, ErrAllProvidersOpen
	}
	return nil, fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr)
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
			cat := ClassifyError(err)
			t.fallback.recordErrorMetric(t.ctx, t.provider, cat)
			if ShouldFallback(cat) {
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

func (f *FallbackLLM) recordFailureWithCategory(ctx context.Context, name string, wasHalfOpen bool, cat ErrorCategory) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs := f.breaker[name]
	cooldown := f.cooldown * time.Duration(CooldownMultiplier(cat))
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

func (f *FallbackLLM) recordErrorMetric(ctx context.Context, name string, cat ErrorCategory) {
	if f.errorsByCategory == nil {
		return
	}
	f.errorsByCategory.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("provider", name),
			attribute.String("category", CategoryString(cat)),
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
