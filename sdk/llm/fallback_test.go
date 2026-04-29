package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ---------------------------------------------------------------------------
// Test mocks
// ---------------------------------------------------------------------------

type succeedLLM struct{ name string }

func (s *succeedLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return NewTextMessage(RoleAssistant, "from-"+s.name), TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, nil
}

func (s *succeedLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return &noopStream{}, nil
}

type failLLM struct {
	name string
	err  error
}

func (f *failLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	if f.err != nil {
		return Message{}, TokenUsage{}, f.err
	}
	return Message{}, TokenUsage{}, fmt.Errorf("provider %s down", f.name)
}

func (f *failLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return nil, fmt.Errorf("provider %s down", f.name)
}

type flakyLLM struct {
	name      string
	failUntil int
	callCount *int
}

func (f *flakyLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	*f.callCount++
	if *f.callCount <= f.failUntil {
		return Message{}, TokenUsage{}, fmt.Errorf("transient error %d", *f.callCount)
	}
	return NewTextMessage(RoleAssistant, "from-"+f.name), TokenUsage{}, nil
}

func (f *flakyLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return nil, fmt.Errorf("not implemented")
}

type streamSucceedLLM struct {
	name string
	text string
}

func (s *streamSucceedLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return NewTextMessage(RoleAssistant, s.text), TokenUsage{}, nil
}

func (s *streamSucceedLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return &fakeStream{text: s.text}, nil
}

type streamErrorLLM struct{ name string }

func (s *streamErrorLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return Message{}, TokenUsage{}, fmt.Errorf("fail")
}

func (s *streamErrorLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return &fakeStream{text: "partial", streamErr: fmt.Errorf("stream broke")}, nil
}

type nilStreamLLM struct{}

func (n *nilStreamLLM) Generate(_ context.Context, _ []Message, _ ...GenerateOption) (Message, TokenUsage, error) {
	return Message{}, TokenUsage{}, nil
}

func (n *nilStreamLLM) GenerateStream(_ context.Context, _ []Message, _ ...GenerateOption) (StreamMessage, error) {
	return nil, nil
}

type noopStream struct{}

func (n *noopStream) Next() bool           { return false }
func (n *noopStream) Current() StreamChunk { return StreamChunk{} }
func (n *noopStream) Err() error           { return nil }
func (n *noopStream) Close() error         { return nil }
func (n *noopStream) Message() Message     { return Message{} }
func (n *noopStream) Usage() Usage         { return Usage{} }

type fakeStream struct {
	text      string
	streamErr error
	done      bool
}

func (f *fakeStream) Next() bool {
	if f.done {
		return false
	}
	f.done = true
	return true
}

func (f *fakeStream) Current() StreamChunk {
	return StreamChunk{Role: RoleAssistant, Content: f.text, FinishReason: "stop"}
}

func (f *fakeStream) Err() error   { return f.streamErr }
func (f *fakeStream) Close() error { return nil }
func (f *fakeStream) Message() Message {
	return NewTextMessage(RoleAssistant, f.text)
}
func (f *fakeStream) Usage() Usage { return Usage{InputTokens: 5, OutputTokens: 3} }

// ---------------------------------------------------------------------------
// Generate — basic fallback
// ---------------------------------------------------------------------------

func TestFallbackLLM_PrimarySucceeds(t *testing.T) {
	fb := NewFallbackLLM(&succeedLLM{name: "primary"}, "primary", nil)

	msg, usage, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content() != "from-primary" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "from-primary")
	}
	if usage.TotalTokens != 15 {
		t.Fatalf("TotalTokens = %d, want 15", usage.TotalTokens)
	}
}

func TestFallbackLLM_FallsBackToSecondary(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "primary"}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}})

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content() != "from-secondary" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "from-secondary")
	}
}

func TestFallbackLLM_NoFallbacks_PrimarySucceeds(t *testing.T) {
	fb := NewFallbackLLM(&succeedLLM{name: "only"}, "only", nil)

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content() != "from-only" {
		t.Fatalf("got %q", msg.Content())
	}
}

func TestFallbackLLM_NoFallbacks_PrimaryFails(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "only"}, "only", nil)

	_, _, err := fb.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFallbackLLM_AllFail(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "primary"}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &failLLM{name: "secondary"}}})

	_, _, err := fb.Generate(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error when all providers fail")
	}
	if !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("expected ErrAllProvidersFailed, got: %v", err)
	}
}

// TestFallbackLLM_TerminalErrors_AreNotAvailable pins the dual
// contract on the chain-terminal errors:
//
//   - identity:     errors.Is(err, ErrAllProvidersOpen|Failed) — so
//     callers and tests can branch on which exhaustion mode hit.
//   - last-err id:  errors.Is(err, lastErr) — so the underlying
//     provider error survives the wrap (the %w/%w in allFailedError
//     keeps both unwrap branches reachable).
//   - classification: errdefs.IsNotAvailable(err) — so HTTPStatus
//     emits 503 instead of falling through to the default 500. A
//     fallback chain that has run out is the textbook "service
//     unavailable, retry later" semantics, not an internal bug.
//
// A regression in any one of these three would silently degrade
// either observability (sentinel branching) or wire status (pod /
// api would surface a 500 to clients).
func TestFallbackLLM_TerminalErrors_AreNotAvailable(t *testing.T) {
	t.Run("all_open", func(t *testing.T) {
		fb := NewFallbackLLM(&failLLM{name: "p1"}, "p1",
			[]FallbackEntry{{Name: "p2", LLM: &failLLM{name: "p2"}}},
			WithBreakerThreshold(1), WithBreakerCooldown(10*time.Second))

		_, _, _ = fb.Generate(context.Background(), nil)
		_, _, _ = fb.Generate(context.Background(), nil)

		_, _, err := fb.Generate(context.Background(), nil)
		if !errors.Is(err, ErrAllProvidersOpen) {
			t.Fatalf("identity lost: want ErrAllProvidersOpen, got: %v", err)
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("classification lost: want IsNotAvailable, got: %v", err)
		}
		if got := errdefs.HTTPStatus(err); got != http.StatusServiceUnavailable {
			t.Fatalf("HTTPStatus = %d, want 503", got)
		}
	})

	t.Run("all_failed_preserves_last_err_identity", func(t *testing.T) {
		sentinel := errors.New("upstream-specific failure")
		fb := NewFallbackLLM(
			&failLLM{name: "primary", err: sentinel}, "primary",
			[]FallbackEntry{{Name: "secondary", LLM: &failLLM{name: "secondary", err: sentinel}}})

		_, _, err := fb.Generate(context.Background(), nil)
		if !errors.Is(err, ErrAllProvidersFailed) {
			t.Fatalf("chain identity lost: want ErrAllProvidersFailed, got: %v", err)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("provider identity lost: want sentinel reachable via Is, got: %v", err)
		}
		if !errdefs.IsNotAvailable(err) {
			t.Fatalf("classification lost: want IsNotAvailable, got: %v", err)
		}
		if got := errdefs.HTTPStatus(err); got != http.StatusServiceUnavailable {
			t.Fatalf("HTTPStatus = %d, want 503", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Generate — error classification & fallback behavior
// ---------------------------------------------------------------------------

func TestFallbackLLM_PermanentError_NoFallback(t *testing.T) {
	permErr := fmt.Errorf("http 400: invalid request parameters")
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: permErr}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}})

	_, _, err := fb.Generate(context.Background(), nil)
	if err != permErr {
		t.Fatalf("expected original permanent error, got: %v", err)
	}

	fb.mu.Lock()
	failures := fb.breaker["primary"].failures
	fb.mu.Unlock()
	if failures != 0 {
		t.Fatalf("permanent error should not increment breaker, got %d", failures)
	}
}

func TestFallbackLLM_TransientError_FallsBack(t *testing.T) {
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: fmt.Errorf("http 502: bad gateway")}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}})

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content() != "from-secondary" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "from-secondary")
	}
}

func TestFallbackLLM_ContextOverflow_NoFallback(t *testing.T) {
	overflowErr := fmt.Errorf("maximum context length exceeded")
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: overflowErr}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}})

	_, _, err := fb.Generate(context.Background(), nil)
	if err != overflowErr {
		t.Fatalf("expected original overflow error, got: %v", err)
	}
}

func TestFallbackLLM_AuthError_FallsBackWithLongCooldown(t *testing.T) {
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: fmt.Errorf("invalid api key provided")}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}},
		WithBreakerThreshold(1), WithBreakerCooldown(100*time.Millisecond))

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected fallback, got error: %v", err)
	}
	if msg.Content() != "from-secondary" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "from-secondary")
	}

	fb.mu.Lock()
	cooldownEnd := fb.breaker["primary"].openUntil
	fb.mu.Unlock()

	minEnd := time.Now().Add(100*time.Millisecond*10 - 200*time.Millisecond)
	if cooldownEnd.Before(minEnd) {
		t.Fatalf("auth error cooldown too short: openUntil=%v", cooldownEnd)
	}
}

func TestFallbackLLM_RateLimitError_FallsBackWithMediumCooldown(t *testing.T) {
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: fmt.Errorf("http 429: too many requests")}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}},
		WithBreakerThreshold(1), WithBreakerCooldown(100*time.Millisecond))

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected fallback, got error: %v", err)
	}
	if msg.Content() != "from-secondary" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "from-secondary")
	}

	fb.mu.Lock()
	cooldownEnd := fb.breaker["primary"].openUntil
	fb.mu.Unlock()

	minEnd := time.Now().Add(100*time.Millisecond*3 - 100*time.Millisecond)
	if cooldownEnd.Before(minEnd) {
		t.Fatalf("rate limit cooldown too short: openUntil=%v", cooldownEnd)
	}
}

// ---------------------------------------------------------------------------
// Circuit breaker
// ---------------------------------------------------------------------------

func TestFallbackLLM_CircuitBreaker(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "primary"}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}},
		WithBreakerThreshold(2), WithBreakerCooldown(50*time.Millisecond))

	for i := 0; i < 3; i++ {
		_, _, _ = fb.Generate(context.Background(), nil)
	}

	fb.mu.Lock()
	cs := fb.breaker["primary"]
	isOpen := cs.failures >= 2 && time.Now().Before(cs.openUntil)
	fb.mu.Unlock()
	if !isOpen {
		t.Fatal("circuit breaker should be open after threshold failures")
	}

	time.Sleep(60 * time.Millisecond)

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error after cooldown: %v", err)
	}
	if msg.Content() != "from-secondary" {
		t.Fatalf("Content() = %q, want %q", msg.Content(), "from-secondary")
	}
}

func TestFallbackLLM_AllBreakersOpen(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "p1"}, "p1",
		[]FallbackEntry{{Name: "p2", LLM: &failLLM{name: "p2"}}},
		WithBreakerThreshold(1), WithBreakerCooldown(10*time.Second))

	_, _, _ = fb.Generate(context.Background(), nil)
	_, _, _ = fb.Generate(context.Background(), nil)

	_, _, err := fb.Generate(context.Background(), nil)
	if !errors.Is(err, ErrAllProvidersOpen) {
		t.Fatalf("expected ErrAllProvidersOpen, got: %v", err)
	}
}

func TestFallbackLLM_CircuitBreaker_HalfOpen_Success(t *testing.T) {
	callCount := 0
	fb := NewFallbackLLM(
		&flakyLLM{name: "flaky", failUntil: 2, callCount: &callCount}, "flaky", nil,
		WithBreakerThreshold(2), WithBreakerCooldown(50*time.Millisecond))

	_, _, _ = fb.Generate(context.Background(), nil)
	_, _, _ = fb.Generate(context.Background(), nil)

	fb.mu.Lock()
	isOpen := fb.breaker["flaky"].failures >= 2
	fb.mu.Unlock()
	if !isOpen {
		t.Fatal("breaker should be open")
	}

	time.Sleep(60 * time.Millisecond)

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("half-open probe should succeed: %v", err)
	}
	if msg.Content() != "from-flaky" {
		t.Fatalf("expected from-flaky, got %q", msg.Content())
	}

	fb.mu.Lock()
	if fb.breaker["flaky"].failures != 0 {
		t.Fatalf("failures should be reset, got %d", fb.breaker["flaky"].failures)
	}
	fb.mu.Unlock()
}

func TestFallbackLLM_CircuitBreaker_HalfOpen_SecondFailure(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "primary"}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}},
		WithBreakerThreshold(2), WithBreakerCooldown(50*time.Millisecond))

	for i := 0; i < 3; i++ {
		_, _, _ = fb.Generate(context.Background(), nil)
	}
	time.Sleep(60 * time.Millisecond)

	msg, _, err := fb.Generate(context.Background(), nil)
	if err != nil {
		t.Fatalf("should fallback to secondary: %v", err)
	}
	if msg.Content() != "from-secondary" {
		t.Fatalf("expected secondary after half-open failure, got %q", msg.Content())
	}
}

func TestWithHalfOpenTimeout(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "p"}, "p", nil,
		WithBreakerThreshold(1),
		WithBreakerCooldown(10*time.Millisecond),
		WithHalfOpenTimeout(30*time.Millisecond))

	if fb.halfOpenTimeout != 30*time.Millisecond {
		t.Fatalf("halfOpenTimeout = %v, want 30ms", fb.halfOpenTimeout)
	}

	_, _, _ = fb.Generate(context.Background(), nil)
	time.Sleep(15 * time.Millisecond)

	// First call enters half-open probe.
	_, _, _ = fb.Generate(context.Background(), nil)

	// Wait for halfOpenTimeout to expire.
	time.Sleep(35 * time.Millisecond)

	allow, ho := fb.canAttempt(context.Background(), "p")
	if !allow || !ho {
		t.Fatalf("expected new probe after halfOpenTimeout expired, allow=%v halfOpen=%v", allow, ho)
	}
}

// ---------------------------------------------------------------------------
// GenerateStream
// ---------------------------------------------------------------------------

func TestFallbackLLM_AllBreakersOpen_Stream(t *testing.T) {
	fb := NewFallbackLLM(&failLLM{name: "p1"}, "p1",
		[]FallbackEntry{{Name: "p2", LLM: &failLLM{name: "p2"}}},
		WithBreakerThreshold(1), WithBreakerCooldown(10*time.Second))

	_, _ = fb.GenerateStream(context.Background(), nil)
	_, _ = fb.GenerateStream(context.Background(), nil)

	_, err := fb.GenerateStream(context.Background(), nil)
	if !errors.Is(err, ErrAllProvidersOpen) {
		t.Fatalf("expected ErrAllProvidersOpen, got: %v", err)
	}
}

func TestFallbackLLM_PermanentError_Stream_NoFallback(t *testing.T) {
	permErr := fmt.Errorf("http 400: invalid request parameters")
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: permErr}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}})

	_, err := fb.GenerateStream(context.Background(), nil)
	if err != permErr {
		t.Fatalf("permanent error should skip fallback for stream, got: %v", err)
	}
}

func TestFallbackLLM_AuthError_Stream_FallsBack(t *testing.T) {
	fb := NewFallbackLLM(
		&failLLM{name: "primary", err: fmt.Errorf("http 401: invalid api key")}, "primary",
		[]FallbackEntry{{Name: "secondary", LLM: &succeedLLM{name: "secondary"}}},
		WithBreakerThreshold(1), WithBreakerCooldown(100*time.Millisecond))

	_, err := fb.GenerateStream(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected fallback to secondary stream, got error: %v", err)
	}

	fb.mu.Lock()
	cooldownEnd := fb.breaker["primary"].openUntil
	fb.mu.Unlock()

	minEnd := time.Now().Add(100*time.Millisecond*10 - 200*time.Millisecond)
	if cooldownEnd.Before(minEnd) {
		t.Fatalf("auth error cooldown too short for stream: openUntil=%v", cooldownEnd)
	}
}

// ---------------------------------------------------------------------------
// trackedStream
// ---------------------------------------------------------------------------

func TestTrackedStream_Success(t *testing.T) {
	fb := NewFallbackLLM(&streamSucceedLLM{name: "primary", text: "hello world"}, "primary", nil)

	stream, err := fb.GenerateStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var content string
	for stream.Next() {
		content += stream.Current().Content
	}
	if content != "hello world" {
		t.Fatalf("content = %q, want %q", content, "hello world")
	}

	fb.mu.Lock()
	failures := fb.breaker["primary"].failures
	fb.mu.Unlock()
	if failures != 0 {
		t.Fatalf("successful stream should reset failures, got %d", failures)
	}
}

func TestTrackedStream_Close(t *testing.T) {
	fb := NewFallbackLLM(&streamSucceedLLM{name: "primary", text: "data"}, "primary", nil)

	stream, err := fb.GenerateStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	fb.mu.Lock()
	failures := fb.breaker["primary"].failures
	fb.mu.Unlock()
	if failures != 0 {
		t.Fatalf("closed stream should record success, got %d failures", failures)
	}
}

func TestTrackedStream_Error(t *testing.T) {
	fb := NewFallbackLLM(&streamErrorLLM{name: "primary"}, "primary", nil,
		WithBreakerThreshold(1))

	stream, err := fb.GenerateStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for stream.Next() {
	}

	fb.mu.Lock()
	failures := fb.breaker["primary"].failures
	fb.mu.Unlock()
	if failures != 1 {
		t.Fatalf("errored stream should increment failures, got %d", failures)
	}
}

func TestTrackedStream_NilStream(t *testing.T) {
	fb := NewFallbackLLM(&nilStreamLLM{}, "primary", nil)

	_, err := fb.GenerateStream(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil stream")
	}
	if !strings.Contains(err.Error(), "nil stream") {
		t.Fatalf("expected nil stream error, got: %v", err)
	}
}

// TestCategoryLabel pins the metric / log label tokens emitted by
// FallbackLLM. Dashboards filter on these strings, so the set is part
// of the package's observable contract even though categoryLabel is
// unexported.
func TestCategoryLabel(t *testing.T) {
	cases := []struct {
		cat  errdefs.ProviderCategory
		want string
	}{
		{errdefs.ProviderTransient, "transient"},
		{errdefs.ProviderRateLimit, "rate_limit"},
		{errdefs.ProviderAuth, "auth"},
		{errdefs.ProviderBilling, "billing"},
		{errdefs.ProviderContextOverflow, "context_overflow"},
		{errdefs.ProviderPermanent, "permanent"},
	}
	for _, tc := range cases {
		if got := categoryLabel(tc.cat); got != tc.want {
			t.Errorf("categoryLabel(%d) = %q, want %q", tc.cat, got, tc.want)
		}
	}
}

// TestShouldFallback pins the chain-stop policy: ContextOverflow and
// Permanent are the only categories that must NOT advance to the next
// provider in the FallbackLLM chain (downstream sees the same input
// and fails the same way). A regression here would either degrade UX
// (give up too early) or burn quota (retry on a definite no).
func TestShouldFallback(t *testing.T) {
	cases := []struct {
		cat    errdefs.ProviderCategory
		expect bool
	}{
		{errdefs.ProviderTransient, true},
		{errdefs.ProviderRateLimit, true},
		{errdefs.ProviderAuth, true},
		{errdefs.ProviderBilling, true},
		{errdefs.ProviderContextOverflow, false},
		{errdefs.ProviderPermanent, false},
	}
	for _, tc := range cases {
		t.Run(categoryLabel(tc.cat), func(t *testing.T) {
			if got := shouldFallback(tc.cat); got != tc.expect {
				t.Errorf("shouldFallback(%v) = %v, want %v", tc.cat, got, tc.expect)
			}
		})
	}
}

// TestCooldownMultiplier pins the per-category breaker hold:
// Auth/Billing get the long penalty (credentials don't fix themselves),
// RateLimit gets a moderate hold to let the upstream window roll over.
func TestCooldownMultiplier(t *testing.T) {
	cases := []struct {
		cat  errdefs.ProviderCategory
		want int
	}{
		{errdefs.ProviderTransient, 1},
		{errdefs.ProviderRateLimit, 3},
		{errdefs.ProviderAuth, 10},
		{errdefs.ProviderBilling, 10},
		{errdefs.ProviderContextOverflow, 1},
		{errdefs.ProviderPermanent, 1},
	}
	for _, tc := range cases {
		t.Run(categoryLabel(tc.cat), func(t *testing.T) {
			if got := cooldownMultiplier(tc.cat); got != tc.want {
				t.Errorf("cooldownMultiplier(%v) = %d, want %d", tc.cat, got, tc.want)
			}
		})
	}
}
