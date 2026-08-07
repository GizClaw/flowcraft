package route

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

func newCountingGenerateRuntime(
	t *testing.T,
	fail func(attempt int) error,
) *inference.Runtime {
	t.Helper()
	return newTwoModelGenerateRuntime(t, map[string]func(_ int) error{
		"model": fail,
	})
}

func newTwoModelGenerateRuntime(
	t *testing.T,
	behaviors map[string]func(_ int) error,
) *inference.Runtime {
	t.Helper()
	compile := func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[string], error) {
		active := request.ActiveFieldsFor(shape)
		decisions := make([]inference.Decision, len(active))
		for index, field := range active {
			decisions[index] = inference.Decision{
				Field: field, Disposition: inference.Native,
			}
		}
		return inference.Compiled[string]{
			Wire: "wire",
			Report: inference.CompileReport{
				Operation: inference.OperationGenerate, Decisions: decisions,
			},
		}, nil
	}
	decode := func(_ context.Context, _ string) (inference.GenerateResponse, error) {
		return validGenerateResponse(), nil
	}
	models := make([]inference.ModelImplementation, 0, len(behaviors))
	for name, fail := range behaviors {
		counter := 0
		transport := func(_ context.Context, _ string) (string, error) {
			counter++
			if err := fail(counter); err != nil {
				return "", err
			}
			return "ok", nil
		}
		driver, err := inference.BindGenerate(compile, transport, decode)
		if err != nil {
			t.Fatalf("BindGenerate (%s): %v", name, err)
		}
		models = append(models, inference.ModelImplementation{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: name},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context,
					inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: driver}, nil
				},
			},
		})
	}
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake", Models: models,
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func retryRouter(
	t *testing.T,
	runtime *inference.Runtime,
	model inference.ModelRef,
	fallback GenerateFallbackPolicy,
	options ...Option,
) *Router {
	t.Helper()
	selectors := Selectors{
		Generate: generateSelectorFunc(func(
			context.Context,
			inference.GenerateRequest,
		) (Decision, error) {
			return generateDecision(model), nil
		}),
		GenerateFallback: fallback,
	}
	router, err := New(runtime, selectors, options...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return router
}

func TestRetryThenSuccess(t *testing.T) {
	attempts := 0
	runtime := newCountingGenerateRuntime(t, func(attempt int) error {
		attempts = attempt
		if attempt <= 2 {
			return errdefs.RateLimit(errors.New("slow down"))
		}
		return nil
	})
	var delays []time.Duration
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{MaxAttempts: 3},
		}),
		withTestSleeper(func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		}),
	)

	response, trace, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if attempts != 3 || response.Metadata.Model.Name != "model" {
		t.Fatalf("attempts/response = %d/%+v", attempts, response)
	}
	if len(delays) != 2 {
		t.Fatalf("slept %d times, want 2", len(delays))
	}
	if delays[0] <= 0 || delays[1] <= 0 {
		t.Fatalf("delays = %v, want positive", delays)
	}
	retries := 0
	for _, attempt := range trace.Attempts {
		if attempt.Trigger == AttemptTriggerRetry {
			retries++
		}
	}
	if retries != 2 {
		t.Fatalf("retry triggers = %d, trace = %+v", retries, trace.Attempts)
	}
}

func TestRetryExhaustedReturnsLastError(t *testing.T) {
	runtime := newCountingGenerateRuntime(t, func(_ int) error {
		return errdefs.RateLimit(errors.New("slow down"))
	})
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{MaxAttempts: 2},
		}),
		withTestSleeper(func(_ context.Context, _ time.Duration) error { return nil }),
	)

	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
	if !errdefs.IsRateLimit(err) {
		t.Fatalf("Generate error = %v, want rate limit classification", err)
	}
	if len(trace.Attempts) != 4 {
		t.Fatalf("trace attempts = %+v, want preflight+execute per attempt", trace.Attempts)
	}
}

func TestNoRetryOnOperationInterrupted(t *testing.T) {
	runtime := newCountingGenerateRuntime(t, func(_ int) error {
		return context.Canceled
	})
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{MaxAttempts: 3},
		}),
	)

	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !errdefs.IsAborted(err) {
		t.Fatalf("Generate error = %v, want aborted classification", err)
	}
	for _, attempt := range trace.Attempts {
		if attempt.Trigger == AttemptTriggerRetry {
			t.Fatalf("unexpected retry attempt: %+v", attempt)
		}
	}
}

func TestNoRetryOnCompilerRejection(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"model": {reject: inference.UnsupportedFeature},
	})
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{
				MaxAttempts: 3,
				Retryable: func(context.Context, RetryDecision) bool {
					return true
				},
			},
		}),
	)

	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !inference.IsKind(err, inference.UnsupportedFeature) {
		t.Fatalf("Generate error = %v, want unsupported feature", err)
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %+v, want a single rejection", trace.Attempts)
	}
}

func TestFallbackOnRetryExhausted(t *testing.T) {
	first := generateModel("first")
	second := generateModel("second")
	firstAttempts := 0
	runtime := newTwoModelGenerateRuntime(t,
		map[string]func(_ int) error{
			"first": func(_ int) error {
				firstAttempts++
				return errdefs.RateLimit(errors.New("slow down"))
			},
			"second": func(_ int) error { return nil },
		},
	)
	router := retryRouter(t, runtime, first, generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		return second, true, nil
	}), WithRetryPolicies(RetryPolicies{
		Generate: &RetryPolicy{
			MaxAttempts:              2,
			FallbackOnRetryExhausted: true,
		},
	}), withTestSleeper(func(_ context.Context, _ time.Duration) error { return nil }))

	response, trace, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Metadata.Model.Name != "second" {
		t.Fatalf("executed = %+v, want second", response.Metadata.Model)
	}
	if firstAttempts != 2 {
		t.Fatalf("primary attempts = %d, want 2", firstAttempts)
	}
	if len(trace.Fallbacks) != 1 {
		t.Fatalf("fallbacks = %+v, want 1", trace.Fallbacks)
	}
}

func TestRetryAfterOverridesBackoff(t *testing.T) {
	runtime := newCountingGenerateRuntime(t, func(_ int) error {
		return errdefs.WithRetryAfter(
			errdefs.RateLimit(errors.New("slow down")),
			7*time.Second,
		)
	})
	var delays []time.Duration
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{
				MaxAttempts: 2,
				Backoff:     Backoff{Kind: BackoffFixed, Initial: time.Second},
			},
		}),
		withTestSleeper(func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		}),
	)

	_, _, err := router.Generate(context.Background(), generateRequest("hello"))
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
	if len(delays) != 1 || delays[0] != 7*time.Second {
		t.Fatalf("delays = %v, want [7s]", delays)
	}
}

func TestBackoffStopsOnContextCancel(t *testing.T) {
	runtime := newCountingGenerateRuntime(t, func(_ int) error {
		return errdefs.RateLimit(errors.New("slow down"))
	})
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{MaxAttempts: 3},
		}),
		withTestSleeper(func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := router.Generate(ctx, generateRequest("hello"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate error = %v, want context canceled", err)
	}
}

func TestBreakerOpensSkipsAndRecovers(t *testing.T) {
	failMode := true
	runtime := newCountingGenerateRuntime(t, func(_ int) error {
		if failMode {
			return errdefs.RateLimit(errors.New("slow down"))
		}
		return nil
	})
	now := time.Now()
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold:  2,
			RecoveryWindow:    time.Hour,
			HalfOpenMaxProbes: 1,
		}),
		withTestClock(func() time.Time { return now }),
		withTestSleeper(func(_ context.Context, _ time.Duration) error { return nil }),
	)

	// Two transient failures open the circuit; the second carries the
	// closed → open transition in its trace.
	var openTrace Trace
	for index := range 2 {
		_, trace, err := router.Generate(
			context.Background(),
			generateRequest("hello"),
		)
		if !inference.IsKind(err, inference.ProviderFailure) {
			t.Fatalf("Generate error = %v, want provider failure", err)
		}
		if index == 1 {
			openTrace = trace
		}
	}
	if transition := openTrace.Attempts[len(openTrace.Attempts)-1].CircuitTransition; transition != "open" {
		t.Fatalf("opening attempt transition = %q, want open", transition)
	}
	// Third call is skipped: no fallback policy, so CircuitOpen.
	_, trace, err := router.Generate(context.Background(), generateRequest("hello"))
	if !IsKind(err, CircuitOpen) {
		t.Fatalf("Generate error = %v, want circuit open", err)
	}
	if trace.Attempts[len(trace.Attempts)-1].Outcome != AttemptOutcomeSkipped ||
		trace.Attempts[len(trace.Attempts)-1].Circuit != "open" {
		t.Fatalf("last attempt = %+v, want open skip", trace.Attempts[len(trace.Attempts)-1])
	}

	// Recovery window elapses and the half-open probe succeeds, closing it.
	failMode = false
	now = now.Add(2 * time.Hour)
	_, probeTrace, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("half-open probe: %v", err)
	}
	if transition := probeTrace.Attempts[len(probeTrace.Attempts)-1].CircuitTransition; transition != "closed" {
		t.Fatalf("probe transition = %q, want closed", transition)
	}
	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); err != nil {
		t.Fatalf("closed circuit call: %v", err)
	}

	// A new transient failure starts the counter again.
	failMode = true
	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
}

func TestResetCircuitBreaker(t *testing.T) {
	runtime := newCountingGenerateRuntime(t, func(int) error {
		return errdefs.RateLimit(errors.New("slow down"))
	})
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold:  1,
			RecoveryWindow:    time.Hour,
			HalfOpenMaxProbes: 1,
		}),
		withTestClock(func() time.Time { return time.Now() }),
		withTestSleeper(func(context.Context, time.Duration) error { return nil }),
	)

	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); !IsKind(err, CircuitOpen) {
		t.Fatalf("Generate error = %v, want circuit open", err)
	}

	router.ResetCircuitBreaker()
	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate after reset = %v, want provider failure, not circuit open", err)
	}
}

func TestGlobalAttemptBudgetStopsRetriesAndFallback(t *testing.T) {
	runtime := newTwoModelGenerateRuntime(t, map[string]func(int) error{
		"first":  func(int) error { return errdefs.RateLimit(errors.New("slow")) },
		"second": func(int) error { return errdefs.RateLimit(errors.New("slow")) },
	})
	router := retryRouter(t, runtime, generateModel("first"), generateFallbackFunc(func(
		context.Context,
		inference.GenerateRequest,
		Attempt,
	) (inference.ModelRef, bool, error) {
		return generateModel("second"), true, nil
	}), WithRetryPolicies(RetryPolicies{
		Generate: &RetryPolicy{
			MaxAttempts:              3,
			MaxTotalAttempts:         2,
			FallbackOnRetryExhausted: true,
		},
	}), withTestSleeper(func(context.Context, time.Duration) error { return nil }))

	_, trace, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	)
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
	if len(trace.Fallbacks) != 0 {
		t.Fatalf("fallbacks = %+v, want none under global budget", trace.Fallbacks)
	}
	// Two logical attempts (preflight + execute each) exhausted the budget.
	if len(trace.Attempts) != 4 {
		t.Fatalf("trace attempts = %+v, want 4 records for 2 logical attempts", trace.Attempts)
	}
}

func TestBreakerHalfOpenConcurrentProbes(t *testing.T) {
	failMode := true
	var transportCalls atomic.Int64
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeOnce sync.Once
	runtime := newTwoModelGenerateRuntime(t, map[string]func(int) error{
		"model": func(int) error {
			transportCalls.Add(1)
			if failMode {
				return errdefs.RateLimit(errors.New("slow down"))
			}
			probeOnce.Do(func() { close(probeStarted) })
			<-releaseProbe
			return nil
		},
	})
	now := time.Now()
	router := retryRouter(t, runtime, generateModel("model"), nil,
		WithCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold:  1,
			RecoveryWindow:    time.Hour,
			HalfOpenMaxProbes: 1,
		}),
		withTestClock(func() time.Time { return now }),
		withTestSleeper(func(context.Context, time.Duration) error { return nil }),
	)

	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("Generate error = %v, want provider failure", err)
	}
	failMode = false
	now = now.Add(2 * time.Hour)

	const workers = 8
	results := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _, err := router.Generate(
				context.Background(),
				generateRequest("hello"),
			)
			results <- err
		}()
	}

	// The single half-open probe is now blocked in transport. Every other
	// worker must be denied until it finishes.
	<-probeStarted
	circuitOpens := 0
	for circuitOpens < workers-1 {
		err := <-results
		if !IsKind(err, CircuitOpen) {
			t.Fatalf("unexpected result before probe released: %v", err)
		}
		circuitOpens++
	}
	close(releaseProbe)
	if err := <-results; err != nil {
		t.Fatalf("probe result = %v, want success", err)
	}
	group.Wait()
	close(results)

	if got := transportCalls.Load(); got != 2 {
		t.Fatalf("transport calls = %d, want 2 (open + single probe)", got)
	}
	if _, _, err := router.Generate(
		context.Background(),
		generateRequest("hello"),
	); err != nil {
		t.Fatalf("Generate after probe = %v, want success on closed circuit", err)
	}
}

func TestPolicyJSONRetryAndCircuitBreaker(t *testing.T) {
	data := `{
		"generate": [{
			"tier": "primary",
			"targets": [{"model": {"id": {"provider": "fake", "name": "model"}}}]
		}],
		"retry": {
			"generate": {
				"max_attempts": 2,
				"max_total_attempts": 4,
				"backoff": {"kind": "fixed", "initial": "250ms"},
				"retryable": ["rate_limit"],
				"fallback_on_retry_exhausted": true
			}
		},
		"circuit_breaker": {
			"failure_threshold": 3,
			"recovery_window": "45s",
			"half_open_max_probes": 1
		}
	}`
	var policy Policy
	if err := json.Unmarshal([]byte(data), &policy); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	options, err := policy.Options()
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("options = %d, want 2", len(options))
	}
}

func TestPolicyJSONRejectsUnknownRetryFields(t *testing.T) {
	data := `{
		"generate": [{
			"tier": "primary",
			"targets": [{"model": {"id": {"provider": "fake", "name": "model"}}}]
		}],
		"retry": {
			"generate": {"max_attempts": 2, "bogus": true}
		}
	}`
	var policy Policy
	if err := json.Unmarshal([]byte(data), &policy); err == nil {
		t.Fatal("Unmarshal accepted an unknown retry field")
	}
}

func TestRetryPolicyRequiresSelector(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"model": {},
	})
	_, err := New(runtime, Selectors{
		Generate: generateSelectorFunc(func(
			context.Context,
			inference.GenerateRequest,
		) (Decision, error) {
			return generateDecision(generateModel("model")), nil
		}),
	}, WithRetryPolicies(RetryPolicies{
		Embed: &RetryPolicy{MaxAttempts: 2},
	}))
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("New error = %v, want validation failure", err)
	}
}

func TestRetryPolicyConfigRequiresPools(t *testing.T) {
	policy := Policy{
		Generate: []Pool{{
			Tier:    "primary",
			Targets: []Target{{Model: generateModel("model")}},
		}},
		Retry: &RetryConfig{
			Embed: &RetryPolicyConfig{MaxAttempts: 2},
		},
	}
	if err := policy.Validate(); err == nil ||
		!strings.Contains(err.Error(), "embed retry policy") {
		t.Fatal("Validate accepted retry policy without pools")
	}
}

func TestTraceCloneOwnsNewAttemptFields(t *testing.T) {
	target := generateModel("model")
	trace := Trace{
		Attempts: []Attempt{{
			Target: target, Phase: AttemptPhaseExecute,
			Trigger: AttemptTriggerRetry, Outcome: AttemptOutcomeFailed,
			Number: 2, BackoffMillis: 5, Circuit: "half_open",
		}},
	}
	clone := trace.Clone()
	clone.Attempts[0].Number = 9
	if trace.Attempts[0].Number != 2 {
		t.Fatal("trace clone shares attempt number")
	}
}

func TestPolicyJSONUnknownCircuitFieldRejected(t *testing.T) {
	data := `{
		"generate": [{
			"tier": "primary",
			"targets": [{"model": {"id": {"provider": "fake", "name": "model"}}}]
		}],
		"circuit_breaker": {"failure_threshold": 3, "bogus": true}
	}`
	var policy Policy
	if err := json.Unmarshal([]byte(data), &policy); err == nil {
		t.Fatal("Unmarshal accepted an unknown circuit breaker field")
	}
}

func TestGenerateStreamRetriesOpenFailure(t *testing.T) {
	first := generateModel("stream")
	opens := 0
	// Build a dedicated runtime whose stream opener fails once with a
	// transient error and then succeeds.
	compile := func(
		_ context.Context,
		_ inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[string], error) {
		active := request.ActiveFieldsFor(shape)
		decisions := make([]inference.Decision, len(active))
		for index, field := range active {
			decisions[index] = inference.Decision{
				Field: field, Disposition: inference.Native,
			}
		}
		return inference.Compiled[string]{
			Wire: "wire",
			Report: inference.CompileReport{
				Operation: inference.OperationGenerate, Decisions: decisions,
			},
		}, nil
	}
	operations, err := inference.BindGenerateOperations(
		compile,
		func(_ context.Context, _ string) (string, error) { return "ok", nil },
		func(_ context.Context, _ string) (inference.GenerateResponse, error) {
			return validGenerateResponse(), nil
		},
		func(_ context.Context, _ string) (inference.ProviderStream[streamRaw], error) {
			opens++
			if opens == 1 {
				return nil, errdefs.RateLimit(errors.New("slow down"))
			}
			return &routeProviderStream{events: []streamRaw{{finish: true}}}, nil
		},
		func(_ context.Context, raw streamRaw) (inference.GenerateStreamEvent, error) {
			if raw.finish {
				return inference.GenerateStreamEvent{FinishReason: inference.FinishCompleted}, nil
			}
			return inference.GenerateStreamEvent{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindGenerateOperations: %v", err)
	}
	streamRuntime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: "fake",
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{
				ID: inference.ModelID{Provider: "fake", Name: "stream"},
			},
			Openers: inference.Openers{
				Generate: func(
					context.Context,
					inference.ModelRef,
				) (inference.GenerateOperations, error) {
					return operations, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	router := retryRouter(t, streamRuntime, first, nil,
		WithRetryPolicies(RetryPolicies{
			Generate: &RetryPolicy{MaxAttempts: 2},
		}),
		withTestSleeper(func(_ context.Context, _ time.Duration) error { return nil }),
	)
	stream, trace, err := router.GenerateStream(
		context.Background(),
		generateRequest("hello"),
	)
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if opens != 2 {
		t.Fatalf("stream opens = %d, want 2", opens)
	}
	retries := 0
	for _, attempt := range trace.Attempts {
		if attempt.Trigger == AttemptTriggerRetry {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry triggers = %d, trace = %+v", retries, trace.Attempts)
	}
	if trace.Executed != first ||
		trace.Attempts[len(trace.Attempts)-1].Outcome != AttemptOutcomeOpened {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestAttemptJSONUsesJSONWireField(t *testing.T) {
	data, err := json.Marshal(Attempt{
		Target: generateModel("model"), Phase: AttemptPhaseExecute,
		Trigger: AttemptTriggerRetry, Outcome: AttemptOutcomeFailed,
		Number: 2, BackoffMillis: 25,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if raw["attempt"] != float64(2) || raw["backoff_ms"] != float64(25) {
		t.Fatalf("wire fields = %+v", raw)
	}
	if _, ok := raw["number"]; ok {
		t.Fatalf("unexpected Go field name on wire: %+v", raw)
	}
}
