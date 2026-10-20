package bindings

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
)

// Extensions cross the boundary through host-registered decoders:
// the script adds an "extensions" array of {provider, id, fields}
// objects to the request, and the bridge resolves each entry through
// the decoders the host wired with WithExtensionDecoder. Unregistered
// identities fail with a validation error — scripts can only pick
// from the menu the host explicit offers, never invent provider knobs.

// InferenceBridgeOption customizes NewInferenceBridge.
type InferenceBridgeOption func(*inferenceBridgeConfig)

type inferenceBridgeConfig struct {
	extensions map[string]inference.ExtensionDecoder
}

// WithExtensionDecoder registers the decoder for one
// provider/extension identity pair, keyed "provider/id".
func WithExtensionDecoder(provider, id string, decoder inference.ExtensionDecoder) InferenceBridgeOption {
	return func(cfg *inferenceBridgeConfig) {
		if cfg.extensions == nil {
			cfg.extensions = make(map[string]inference.ExtensionDecoder)
		}
		cfg.extensions[provider+"/"+id] = decoder
	}
}

// inferenceBridge carries one script execution's context plus the
// dependencies and model cache that outlive it, so the entry points can be
// ordinary methods instead of closures nested 20 deep in one map literal: one
// method per entry point, one table listing them all.
type inferenceBridge struct {
	ctx      context.Context
	cfg      *inferenceBridgeConfig
	assembly *inference.Assembly
	router   *route.Router
	// live is the bridge-lifetime assembly cache (see
	// inference.NewBindingCache): it outlives the individual script calls that
	// run through it.
	live *inferenceBindings
}

// newInferenceBridge builds the value that outlives the script executions
// bound from it: the decoded options, the wired dependencies, and the model
// cache. Nothing per-execution lives here — a [BindingFunc] is invoked once per
// script run, and a fresh cache per run would reopen every model a script
// names.
func newInferenceBridge(
	assembly *inference.Assembly,
	router *route.Router,
	opts ...InferenceBridgeOption,
) *inferenceBridge {
	cfg := &inferenceBridgeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return &inferenceBridge{
		cfg:      cfg,
		assembly: assembly,
		router:   router,
		// The bridge outlives the script calls that run through it, and
		// script arguments can name any configured model, so one cache
		// serves the whole bridge: models a script keeps addressing stay
		// open, one-off targets cycle out (see inference.BindingCache).
		live: &inferenceBindings{cache: inference.NewBindingCache(assembly)},
	}
}

// withContext returns the bridge as it serves one script execution: the struct
// is copied, so the options, dependencies and cache stay shared while the
// context — and everything derived from it — is per execution.
func (b inferenceBridge) withContext(ctx context.Context) *inferenceBridge {
	b.ctx = ctx
	return &b
}

// NewInferenceBridge exposes LLM generation to scripts as global
// "inference" with two entry points:
//
//   - generate(request): one Runtime.Generate call. The request is the
//     canonical inference.GenerateRequest wire JSON plus a required
//     "model" key (model.ModelRef JSON, stripped before the strict
//     decode): { model: {id: {provider, name}, profile?}, context,
//     input }
//
//   - route(request): one Router.Generate call. No "model" key — the
//     router's selector/fallback chain chooses the target; the
//     response gains a "trace" key with the route.Trace projection.
//
//   - explain(request): one exact-model Assembly.ExplainGenerate call.
//     Same request shape as generate (model key required); returns the
//     canonical Explanation wire JSON with no provider I/O, so scripts
//     can preflight a request against one concrete model.
//
//   - routeExplain(request): one Router.ExplainGenerate call. Same
//     shape as route (no model key); returns {explanation, decision,
//     limits} where decision records the selected model and limits is
//     that model's declared ModelLimits — scripts can compare the
//     pending context against max_input_tokens before executing.
//
//   - models(): one Assembly.Models call. Returns the full catalog of
//     ModelDescriptor wire JSON (id, capabilities, limits, lifecycle),
//     so scripts can enumerate what the host registered.
//
//   - inspect(model): one Assembly.InspectModel call. The argument is
//     a ModelRef wire JSON ({id: {provider, name}, profile?}); returns
//     that model's ModelDescriptor.
//
//   - embed(request) / routeEmbed(request): the Embed twins of
//     generate/route — exact model (model key required) vs router
//     selection (no model key, response gains "trace").
//
//   - explainEmbed(request) / routeExplainEmbed(request): the Embed
//     twins of explain/routeExplain; routeExplainEmbed returns
//     {explanation, decision, limits} for the selected embed model.
//
//   - transcribe(request) / routeTranscribe(request): the speech
//     recognition twins of generate/route — exact model (model key
//     required, audio source in the canonical request) vs router
//     selection (no model key, response gains "trace").
//
//   - explainTranscribe(request) / routeExplainTranscribe(request):
//     the transcription twins of explain/routeExplain;
//     routeExplainTranscribe returns {explanation, decision, limits}.
//
//   - decision(request) / routeDecision(request): the decision twins of
//     generate/route — evidence plus ordered questions, answers back in
//     request order (a question the model declined comes back as a
//     "refusal" answer). Exact model (model key required) vs router
//     selection (no model key, response gains "trace"). Both are unary:
//     decision has no streaming shape.
//
//   - explainDecision(request) / routeExplainDecision(request): the
//     decision twins of explain/routeExplain; routeExplainDecision
//     returns {explanation, decision, limits}.
//
//   - transcribeSession(request) / routeTranscribeSession(request):
//     duplex speech-recognition sessions. Both return a session handle:
//
//     var s = inference.transcribeSession(req)   // input_format + model
//     s.send({data: <base64>, sequence: 1})      // media.AudioChunk
//     s.finish()                                 // end-of-input (optional)
//     var ev
//     while ((ev = s.next()) !== null) { ... }   // partial/final events
//     var resp = s.result()                      // TranscriptionResponse
//     s.interrupt()                              // barge-in (optional)
//     s.close()
//
//     send() feeds audio chunks; next() projects TranscriptionSessionEvent
//     wire JSON and returns null at EOF; finish() signals that no more audio
//     is coming where the provider supports it (a no-op otherwise), which is
//     how continuous sessions reach EOF after multiple final events;
//     result() yields the accumulated TranscriptionResponse; interrupt()
//     terminates the session abnormally; close() exits early.
//     routeTranscribeSession attaches "trace" to the result object, mirroring
//     routeTranscribe.
//
//   - explainStream(request) / routeExplainStream(request): the
//     Generate-stream twins of explain/routeExplain — local stream
//     compilation without opening a provider stream;
//     routeExplainStream returns {explanation, decision, limits}.
//
//   - stream(request) / routeStream(request): the streaming twins of
//     the above. Both return an iterator handle:
//
//     var s = inference.stream(req)
//     var ev
//     while ((ev = s.next()) !== null) { ... }   // event wire JSON
//     var resp = s.result()                      // GenerateResponse
//     s.close()
//
//     next() projects each GenerateStreamEvent verbatim and returns
//     null at EOF; result() yields the driver-accumulated
//     GenerateResponse — the exact shape generate/route return — so a
//     tool loop looks identical regardless of entry point (routeStream
//     attaches "trace" to it). Streaming deltas stay inside the
//     bridge; publishing them live (host.emit) is the script's own
//     composition decision.
//
// Both return the canonical inference.GenerateResponse wire JSON
// ({message, finish_reason, usage, metadata}); response.message can be
// appended to the next request's context verbatim.
//
// The bridge performs exactly one Generate per call — multi-turn tool
// loops live in script-land: check finish_reason, execute the
// message's tool_call parts via tools.callAll, and continue with
// input.role="tool". tools.definitions() yields wire-ready entries for
// input.content.intent.text.tools.
//
// Extensions ride a bridge-level "extensions" key — entries of
// {provider, id, fields} resolved through the host-registered
// decoders (WithExtensionDecoder) — so scripts can set provider knobs
// the host explicitly offers while the canonical request stays pure
// wire JSON. Route fallback across providers keeps working: each
// attempt applies only its own provider's extensions.
// Either entry point may be unwired (nil runtime/router); calling it
// fails with NotAvailable.
func NewInferenceBridge(assembly *inference.Assembly, router *route.Router, opts ...InferenceBridgeOption) BindingFunc {
	bridge := newInferenceBridge(assembly, router, opts...)
	return func(ctx context.Context) (string, any) {
		return "inference", bridge.withContext(ctx).entries()
	}
}

// entries is the script-facing "inference" table: every entry point the
// bridge exposes, mapped to the method that serves it. The methods live
// with their family — generate and the stream twins in
// bridge_inference_generate.go, embed in bridge_inference_embed.go,
// transcribe and its session twins in bridge_inference_transcribe.go,
// decision in bridge_inference_decision.go, the catalog reads in
// bridge_inference_model.go — and the wire plumbing they share in
// bridge_inference_wire.go.
//
// Adding an entry point means adding one method and one row here.
//
// "models" stays nullary: the script runtimes bind a Go func by its
// arity, so the script-visible call keeps taking no argument.
func (b *inferenceBridge) entries() map[string]any {
	return map[string]any{
		"generate":               b.generate,
		"route":                  b.route,
		"explain":                b.explain,
		"routeExplain":           b.routeExplain,
		"models":                 b.models,
		"inspect":                b.inspect,
		"explainStream":          b.explainStream,
		"routeExplainStream":     b.routeExplainStream,
		"embed":                  b.embed,
		"routeEmbed":             b.routeEmbed,
		"explainEmbed":           b.explainEmbed,
		"routeExplainEmbed":      b.routeExplainEmbed,
		"transcribe":             b.transcribe,
		"routeTranscribe":        b.routeTranscribe,
		"explainTranscribe":      b.explainTranscribe,
		"routeExplainTranscribe": b.routeExplainTranscribe,
		"transcribeSession":      b.transcribeSession,
		"routeTranscribeSession": b.routeTranscribeSession,
		"decision":               b.decision,
		"routeDecision":          b.routeDecision,
		"explainDecision":        b.explainDecision,
		"routeExplainDecision":   b.routeExplainDecision,
		"stream":                 b.stream,
		"routeStream":            b.routeStream,
	}
}

// requireAssembly reports the NotAvailable failure that every
// assembly-backed entry point shares when the host wired no assembly.
func (b *inferenceBridge) requireAssembly(op string) error {
	if b.assembly == nil {
		return errdefs.NotAvailablef("%s: no assembly wired", op)
	}
	return nil
}

// requireRouter is requireAssembly for the router-backed entry points.
func (b *inferenceBridge) requireRouter(op string) error {
	if b.router == nil {
		return errdefs.NotAvailablef("%s: no router wired", op)
	}
	return nil
}
