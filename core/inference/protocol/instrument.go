package protocol

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
	"github.com/GizClaw/flowcraft/core/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// This file instruments the execution funnel every driver passes through. It
// lives on the pipeline handle rather than on Bind* because the pipeline is the
// only place that sees the whole round trip — compile, transport, decode — for
// every seam, so one implementation covers generate, embed and decide, unary and
// stream, and every driver that will ever be written.
//
// What the span describes: one provider round trip. Not the caller's turn, not
// the routing decision (that has its own span under route), and not the assembly
// of a response from many events (that is the stream's own terminal event).
//
// What it never carries: prompts, completions, tool arguments, or a wrapped
// cause. inference.Error.Kind and Error.Detail are stable, redacted labels; the
// message text belongs in a log record, which is why the error path sets a status
// description instead of calling span.RecordError with the cause attached.

// Status values for the metrics' `status` dimension.
const (
	statusSuccess = "success"
	statusError   = "error"
)

// observation carries the seam-specific accessors the instrumentation needs.
//
// The pipeline is generic over its request and response, so reading a model, a
// usage envelope or a finish reason from an arbitrary Res is only possible
// through a function the seam supplies when it binds. That is also what keeps
// this file free of a per-seam type switch.
type observation[Req, Res any] struct {
	// spanName is the fixed span name ("llm.generate", ...). Empty disables
	// instrumentation, which only happens for a pipeline built outside the
	// package's Bind* functions.
	spanName string
	// operation maps the request onto a gen_ai.operation.name value.
	operation func(Req) string
	// model is the requested model, for gen_ai.request.model.
	model func(Req) model.ModelRef
	// usageOf, metadataOf and finishOf read the response envelope.
	usageOf  func(Res) usage.Usage
	metaOf   func(Res) inference.Metadata
	finishOf func(Res) []string
}

func (o observation[Req, Res]) enabled() bool { return o.spanName != "" }

// generateObservation describes the generation seam. It serves both the unary
// and the streaming pipeline: they produce the same response type, so the only
// difference is where the response comes from.
func generateObservation() observation[inference.GenerateRequest, inference.GenerateResponse] {
	return observation[inference.GenerateRequest, inference.GenerateResponse]{
		spanName: "llm.generate",
		operation: func(request inference.GenerateRequest) string {
			// The seam is the same for every modality, but the operation is
			// not: an image or speech request is generate_content, and
			// calling it chat would describe the wire format instead of what
			// the caller asked for.
			if request.Output.Kind() != inference.OutputText {
				return telemetry.GenAIOperationGenerateContent
			}
			return telemetry.GenAIOperationChat
		},
		model: func(request inference.GenerateRequest) model.ModelRef {
			return request.Model
		},
		usageOf: func(response inference.GenerateResponse) usage.Usage {
			return response.Usage
		},
		metaOf: func(response inference.GenerateResponse) inference.Metadata {
			return response.Metadata
		},
		finishOf: func(response inference.GenerateResponse) []string {
			if response.FinishReason == "" {
				return nil
			}
			return []string{string(response.FinishReason)}
		},
	}
}

// embedObservation describes the embedding seam.
func embedObservation() observation[inference.EmbedRequest, inference.EmbedResponse] {
	return observation[inference.EmbedRequest, inference.EmbedResponse]{
		spanName: "llm.embed",
		operation: func(inference.EmbedRequest) string {
			return telemetry.GenAIOperationEmbeddings
		},
		model: func(request inference.EmbedRequest) model.ModelRef {
			return request.Model
		},
		usageOf: func(response inference.EmbedResponse) usage.Usage {
			return response.Usage
		},
		metaOf: func(response inference.EmbedResponse) inference.Metadata {
			return response.Metadata
		},
		finishOf: func(inference.EmbedResponse) []string {
			return nil
		},
	}
}

// decideObservation describes the judgment seam.
func decideObservation() observation[inference.DecisionRequest, inference.DecisionResponse] {
	return observation[inference.DecisionRequest, inference.DecisionResponse]{
		spanName: "llm.decide",
		operation: func(inference.DecisionRequest) string {
			return telemetry.GenAIOperationDecide
		},
		model: func(request inference.DecisionRequest) model.ModelRef {
			return request.Model
		},
		usageOf: func(response inference.DecisionResponse) usage.Usage {
			return response.Usage
		},
		metaOf: func(response inference.DecisionResponse) inference.Metadata {
			return response.Metadata
		},
		finishOf: func(inference.DecisionResponse) []string {
			return nil
		},
	}
}

// call is one instrumented provider round trip.
type call[Res any] struct {
	ctx       context.Context
	span      trace.Span
	start     time.Time
	operation string
	provider  string
	model     string
	stream    bool
	attempted bool

	usageOf  func(Res) usage.Usage
	metaOf   func(Res) inference.Metadata
	finishOf func(Res) []string

	ttft time.Duration
	// sawFirstToken is the "already recorded" flag, not ttft: a platform whose
	// clock resolution can return a zero interval would otherwise read a real
	// first delta as "not yet", emit the event again and drop the metric.
	sawFirstToken bool
}

// startCall opens the span for one call. A nil *call is returned when the
// pipeline carries no observation, and every method tolerates that, so the
// un-instrumented path is the same code with no branching.
func startCall[Req, Res any](
	ctx context.Context,
	obs observation[Req, Res],
	providerID string,
	request Req,
	shape Shape,
) (context.Context, *call[Res]) {
	if !obs.enabled() {
		return ctx, nil
	}
	operation := obs.operation(request)
	ref := obs.model(request)
	ctx, span := telemetry.TracerWithSuffix("inference").Start(ctx, obs.spanName,
		trace.WithAttributes(
			attribute.String(telemetry.AttrGenAIOperationName, operation),
			attribute.String(telemetry.AttrGenAIProviderName, providerID),
			attribute.String(telemetry.AttrGenAIRequestModel, ref.ID.Name),
			attribute.String(telemetry.AttrInferenceShape, string(shape)),
		))
	// The runtime identity the host attached. The span is opened here because
	// this is the one place every driver passes through.
	span.SetAttributes(runctx.Attrs(ctx)...)
	return ctx, &call[Res]{
		ctx:       ctx,
		span:      span,
		start:     time.Now(),
		operation: operation,
		provider:  providerID,
		model:     ref.ID.Name,
		stream:    shape == ShapeStream,
		usageOf:   obs.usageOf,
		metaOf:    obs.metaOf,
		finishOf:  obs.finishOf,
	}
}

// firstToken records the delay to the first visible text delta, once. It is
// called from the stream relay, where the arrival time is known; the metric is
// emitted at finish so a caller that never reaches first text contributes
// nothing rather than a zero.
func (c *call[Res]) firstToken() {
	if c == nil || c.sawFirstToken {
		return
	}
	c.sawFirstToken = true
	c.ttft = time.Since(c.start)
	c.span.AddEvent("llm.first_token", trace.WithAttributes(
		attribute.Int64(telemetry.AttrLLMTimeToFirstTokenMs, c.ttft.Milliseconds()),
	))
}

// logOpen records a stream that crossed the transport boundary. A compile
// failure never reaches the provider, so it is not logged as an opened stream.
func (c *call[Res]) logOpen() {
	if c == nil || !c.attempted {
		return
	}
	inference.LogProviderStream(c.ctx, c.provider, c.operation, c.model, nil, "")
}

// logOutcome records one provider round trip at the boundary the driver would
// otherwise own. Doing it here covers every driver and every modality without a
// per-driver copy: unary calls use the call record, streams use the opener and
// terminal records.
func (c *call[Res]) logOutcome(response Res, err error) {
	if c == nil || !c.attempted {
		return
	}
	if c.stream {
		if err != nil {
			inference.LogProviderStream(c.ctx, c.provider, c.operation, c.model, err, "")
			return
		}
		metadata := c.metaOf(response)
		if metadata.ResponseID != "" {
			inference.LogProviderStreamEnd(c.ctx, c.provider, c.operation, metadata.ResponseID)
		}
		return
	}
	var requestID, responseID string
	if err == nil {
		metadata := c.metaOf(response)
		requestID = metadata.RequestID
		responseID = metadata.ResponseID
	}
	inference.LogProviderCall(
		c.ctx, c.provider, c.operation, c.model, err, requestID, responseID,
	)
}

// finish closes the call: response envelope, usage, error classification, metrics
// and the span itself. It is safe to call on a nil call and more than once (the
// stream relay guards its own double-finish with sync.Once).
func (c *call[Res]) finish(response Res, err error) {
	if c == nil {
		return
	}
	c.logOutcome(response, err)
	duration := time.Since(c.start)

	var tokens usage.Usage
	status, kind := statusSuccess, ""
	if err != nil {
		status = statusError
		kind = c.recordError(err)
	} else {
		tokens = c.recordResponse(response)
		c.span.SetStatus(codes.Ok, "")
	}
	c.recordMetrics(duration, status, kind, tokens)
	c.span.End()
}

// recordResponse mirrors the provider identifiers and the usage envelope onto the
// span, and returns the usage for the metric side.
func (c *call[Res]) recordResponse(response Res) usage.Usage {
	metadata := c.metaOf(response)
	tokens := c.usageOf(response)

	attrs := make([]attribute.KeyValue, 0, 8)
	if metadata.Model.Name != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrGenAIResponseModel, metadata.Model.Name))
	}
	if metadata.RequestID != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrLLMRequestID, metadata.RequestID))
	}
	if metadata.ResponseID != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrGenAIResponseID, metadata.ResponseID))
	}
	if reasons := c.finishOf(response); len(reasons) > 0 {
		attrs = append(attrs,
			attribute.StringSlice(telemetry.AttrGenAIResponseFinishReasons, reasons))
	}
	attrs = append(attrs, usageAttrs(tokens)...)
	c.span.SetAttributes(attrs...)
	return tokens
}

// recordError classifies the failure and returns the kind for the metric
// dimension. The span status carries the kind, never the message: the cause may
// wrap a provider payload, and an exception event would copy it verbatim.
func (c *call[Res]) recordError(err error) string {
	kind, detail, retryAfter, requestID := classifyFailure(err)
	attrs := []attribute.KeyValue{
		attribute.String(telemetry.AttrErrorKind, kind),
	}
	if detail != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrInferenceErrorDetail, detail))
	}
	if retryAfter > 0 {
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrErrorRetryAfterMs, retryAfter.Milliseconds()))
	}
	if requestID != "" {
		attrs = append(attrs,
			attribute.String(telemetry.AttrLLMRequestID, requestID))
	}
	c.span.SetAttributes(attrs...)
	c.span.SetStatus(codes.Error, kind)
	return kind
}

// recordMetrics emits the execution, duration, token and first-token series.
//
// Instruments are resolved per call instead of once at package init: the global
// meter provider is swapped in tests, and an instrument created before the swap
// keeps delegating to the provider that existed then. The SDK caches meters by
// scope and instruments by identity, so a per-call resolution is a couple of map
// lookups, not an allocation per call.
func (c *call[Res]) recordMetrics(
	duration time.Duration,
	status string,
	kind string,
	tokens usage.Usage,
) {
	meter := telemetry.MeterWithSuffix("inference")

	if counter, err := meter.Int64Counter("executions.total",
		metric.WithDescription("Inference calls by outcome")); err == nil {
		counter.Add(c.ctx, 1,
			c.metricAttrs(attribute.String("status", status)))
	}
	if histogram, err := meter.Float64Histogram("duration.seconds",
		metric.WithDescription("Inference call duration")); err == nil {
		histogram.Record(c.ctx, duration.Seconds(), c.metricAttrs())
	}
	if err := status == statusError; err {
		if counter, err := meter.Int64Counter("errors.total",
			metric.WithDescription("Inference failures by classification")); err == nil {
			counter.Add(c.ctx, 1,
				c.metricAttrs(attribute.String(telemetry.AttrErrorKind, kind)))
		}
	}
	recordTokens(c.ctx, meter, c.metricAttrs(), tokens)
	if c.sawFirstToken {
		if histogram, err := meter.Float64Histogram("time_to_first_token.seconds",
			metric.WithDescription("Delay until the first visible text delta")); err == nil {
			histogram.Record(c.ctx, c.ttft.Seconds(), c.metricAttrs())
		}
	}
}

// metricAttrs is the low-cardinality identity set shared by every series: the
// seam, the provider and the requested model are all bounded by the deployment's
// catalog. Run ids and call ids are deliberately absent — they belong on spans.
func (c *call[Res]) metricAttrs(extra ...attribute.KeyValue) metric.MeasurementOption {
	attrs := make([]attribute.KeyValue, 0, 3+len(extra))
	attrs = append(attrs,
		attribute.String(telemetry.AttrGenAIOperationName, c.operation),
		attribute.String(telemetry.AttrGenAIProviderName, c.provider),
		attribute.String(telemetry.AttrGenAIRequestModel, c.model),
	)
	return metric.WithAttributes(append(attrs, extra...)...)
}

// usageAttrs converts a usage envelope into span attributes. Zero values are
// omitted, so "the provider reported zero" and "the provider reported nothing"
// stay distinguishable in the same way usage.Usage's pointer fields keep them
// distinguishable in the API.
func usageAttrs(tokens usage.Usage) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	if tokens.InputTokens > 0 {
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrGenAIUsageInputTokens, tokens.InputTokens))
	}
	if tokens.OutputTokens > 0 {
		attrs = append(attrs,
			attribute.Int64(telemetry.AttrGenAIUsageOutputTokens, tokens.OutputTokens))
	}
	if tokens.Input.CacheReadTokens != nil && *tokens.Input.CacheReadTokens > 0 {
		attrs = append(attrs, attribute.Int64(
			telemetry.AttrGenAIUsageCacheReadInputTokens, *tokens.Input.CacheReadTokens))
	}
	if tokens.Billing != nil && tokens.Billing.Cost != nil {
		if micros, ok := costMicros(*tokens.Billing.Cost); ok {
			attrs = append(attrs,
				attribute.Int64(telemetry.AttrLLMCostMicros, micros))
		}
	}
	return attrs
}

// recordTokens emits the token counters. Only non-zero counters move: an absent
// cache read is not a cache hit rate of zero.
func recordTokens(
	ctx context.Context,
	meter metric.Meter,
	opts metric.MeasurementOption,
	tokens usage.Usage,
) {
	if tokens.InputTokens > 0 {
		if counter, err := meter.Int64Counter("tokens.input",
			metric.WithDescription("Input tokens consumed")); err == nil {
			counter.Add(ctx, tokens.InputTokens, opts)
		}
	}
	if tokens.OutputTokens > 0 {
		if counter, err := meter.Int64Counter("tokens.output",
			metric.WithDescription("Output tokens produced")); err == nil {
			counter.Add(ctx, tokens.OutputTokens, opts)
		}
	}
	if tokens.Input.CacheReadTokens != nil && *tokens.Input.CacheReadTokens > 0 {
		if counter, err := meter.Int64Counter("tokens.input.cached",
			metric.WithDescription("Input tokens served from provider caches")); err == nil {
			counter.Add(ctx, *tokens.Input.CacheReadTokens, opts)
		}
	}
}

// classifyFailure extracts the redacted classification of a failure. The
// provider request id is read from the classified error and, failing that, from
// the errdefs chain a driver may have wrapped it with.
func classifyFailure(err error) (kind, detail string, retryAfter time.Duration, requestID string) {
	kind = "unclassified"
	var inferenceErr *inference.Error
	if errors.As(err, &inferenceErr) {
		kind = string(inferenceErr.Kind)
		detail = inferenceErr.Detail
		retryAfter = inferenceErr.RetryAfter
		requestID = inferenceErr.RequestID
	}
	if requestID == "" {
		if id, ok := errdefs.RequestID(err); ok {
			requestID = id
		}
	}
	return kind, detail, retryAfter, requestID
}

// costMicros converts a Money (Units / 10^Scale) into the integer micro-unit
// representation the llm.cost.micros attribute uses. It reports ok=false when the
// conversion would overflow or when a sub-micro remainder floors to zero, so a
// span never claims a cost of zero for a call that did cost something.
func costMicros(money usage.Money) (int64, bool) {
	if money.Scale > 18 {
		return 0, false
	}
	if money.Scale > 6 {
		micros := money.Units / pow10(money.Scale-6)
		return micros, micros > 0
	}
	factor := pow10(6 - money.Scale)
	if money.Units > math.MaxInt64/factor {
		return 0, false
	}
	return money.Units * factor, true
}

// pow10 returns exactly 10^n for n in [0, 18], matching Money.Scale's validated
// range.
func pow10(n uint8) int64 {
	var value int64 = 1
	for i := uint8(0); i < n; i++ {
		value *= 10
	}
	return value
}

// instrumentedStream relays a generation stream and closes the call span on its
// terminal event.
//
// The span cannot end at Open: the provider's request/response ids and the usage
// envelope only exist once the stream reports its result. It also must end
// exactly once — Next can fail, Result can succeed, Close can abandon the
// stream — which is what the Once guarantees.
type instrumentedStream struct {
	inner inference.Stream
	call  *call[inference.GenerateResponse]
	once  sync.Once
	// completed records that Next reported io.EOF, which is what makes Result
	// meaningful; done records that the span has been closed.
	completed atomic.Bool
	done      atomic.Bool
}

func (s *instrumentedStream) finish(response inference.GenerateResponse, err error) {
	s.once.Do(func() {
		s.done.Store(true)
		s.call.finish(response, err)
	})
}

// earlyClose is the failure classification for a stream abandoned before its
// terminal event. It is ProviderTruncated rather than ProviderFailure because
// the caller ended it, not the provider.
func earlyClose() error {
	return inference.NewError(
		inference.ProviderTruncated,
		inference.FieldStream,
		errors.New("stream closed before completion"),
	).WithDetail("stream.closed.early")
}

func (s *instrumentedStream) Next(ctx context.Context) (inference.StreamEvent, error) {
	event, err := s.inner.Next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			s.completed.Store(true)
			return event, err
		}
		s.finish(inference.GenerateResponse{}, err)
		return event, err
	}
	if event.Kind == inference.StreamText {
		s.call.firstToken()
	}
	return event, nil
}

func (s *instrumentedStream) Result() (inference.GenerateResponse, error) {
	response, err := s.inner.Result()
	s.finish(response, err)
	return response, err
}

func (s *instrumentedStream) Close() error {
	if !s.done.Load() {
		if s.completed.Load() {
			// The stream ended cleanly but the caller never asked for the
			// result. Read it now so the span still carries the usage and the
			// provider ids, rather than reporting a truncated call that was not.
			response, err := s.inner.Result()
			s.finish(response, err)
		} else {
			s.finish(inference.GenerateResponse{}, earlyClose())
		}
	}
	return s.inner.Close()
}
