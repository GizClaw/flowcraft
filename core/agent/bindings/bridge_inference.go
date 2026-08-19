package bindings

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message/media"
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

// NewInferenceBridge exposes LLM generation to scripts as global
// "inference" with two entry points:
//
//   - generate(request): one Runtime.Generate call. The request is the
//     canonical inference.GenerateRequest wire JSON plus a required
//     "model" key (inference.ModelRef JSON, stripped before the strict
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
//   - transcribeSession(request) / routeTranscribeSession(request):
//     duplex speech-recognition sessions. Both return a session handle:
//
//     var s = inference.transcribeSession(req)   // input_format + model
//     s.send({data: <base64>, sequence: 1})      // media.AudioChunk
//     var ev
//     while ((ev = s.next()) !== null) { ... }   // partial/final events
//     var resp = s.result()                      // TranscriptionResponse
//     s.interrupt()                              // barge-in (optional)
//     s.close()
//
//     send() feeds audio chunks; next() projects TranscriptionSessionEvent
//     wire JSON and returns null at EOF; result() yields the accumulated
//     TranscriptionResponse; interrupt() terminates the session abnormally;
//     close() exits early. routeTranscribeSession attaches "trace" to the
//     result object, mirroring routeTranscribe.
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
	cfg := &inferenceBridgeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return func(ctx context.Context) (string, any) {
		return "inference", map[string]any{
			"generate": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.generate: no assembly wired")
				}
				ref, req, err := parseInferenceGenerateCall(
					raw,
					cfg.extensions,
					"inference.generate",
				)
				if err != nil {
					return nil, err
				}
				resp, err := assembly.Generate(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(resp, "inference.generate response")
			},
			"route": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.route: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
					raw,
					cfg.extensions,
					"inference.route",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				resp, trace, err := router.Generate(ctx, req)
				if err != nil {
					return nil, err
				}
				out, err := toScriptJSON(resp, "inference.route response")
				if err != nil {
					return nil, err
				}
				return attachTrace(out, trace, "inference.route")
			},
			"explain": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.explain: no assembly wired")
				}
				ref, req, err := parseInferenceGenerateCall(
					raw,
					cfg.extensions,
					"inference.explain",
				)
				if err != nil {
					return nil, err
				}
				explanation, err := assembly.ExplainGenerate(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(explanation, "inference.explain response")
			},
			"routeExplain": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeExplain: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
					raw,
					cfg.extensions,
					"inference.routeExplain",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				explanation, decision, err := router.ExplainGenerate(ctx, req)
				if err != nil {
					return nil, err
				}
				return routeExplainResult(
					router,
					explanation,
					decision,
					"inference.routeExplain",
				)
			},
			"models": func() (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.models: no assembly wired")
				}
				return toScriptJSON(assembly.Models(), "inference.models response")
			},
			"inspect": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.inspect: no assembly wired")
				}
				var ref inference.ModelRef
				if err := decodeStrictJSON(raw, &ref, "inference.inspect.model"); err != nil {
					return nil, err
				}
				descriptor, err := assembly.InspectModel(ref)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(descriptor, "inference.inspect response")
			},
			"explainStream": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.explainStream: no assembly wired")
				}
				ref, req, err := parseInferenceGenerateCall(
					raw,
					cfg.extensions,
					"inference.explainStream",
				)
				if err != nil {
					return nil, err
				}
				explanation, err := assembly.ExplainGenerateStream(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(explanation, "inference.explainStream response")
			},
			"routeExplainStream": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeExplainStream: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
					raw,
					cfg.extensions,
					"inference.routeExplainStream",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				explanation, decision, err := router.ExplainGenerateStream(ctx, req)
				if err != nil {
					return nil, err
				}
				return routeExplainResult(
					router,
					explanation,
					decision,
					"inference.routeExplainStream",
				)
			},
			"embed": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.embed: no assembly wired")
				}
				ref, req, err := parseInferenceEmbedCall(
					raw,
					cfg.extensions,
					"inference.embed",
				)
				if err != nil {
					return nil, err
				}
				resp, err := assembly.Embed(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(resp, "inference.embed response")
			},
			"routeEmbed": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeEmbed: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.EmbedRequest](
					raw,
					cfg.extensions,
					"inference.routeEmbed",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				resp, trace, err := router.Embed(ctx, req)
				if err != nil {
					return nil, err
				}
				out, err := toScriptJSON(resp, "inference.routeEmbed response")
				if err != nil {
					return nil, err
				}
				return attachTrace(out, trace, "inference.routeEmbed")
			},
			"explainEmbed": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.explainEmbed: no assembly wired")
				}
				ref, req, err := parseInferenceEmbedCall(
					raw,
					cfg.extensions,
					"inference.explainEmbed",
				)
				if err != nil {
					return nil, err
				}
				explanation, err := assembly.ExplainEmbed(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(explanation, "inference.explainEmbed response")
			},
			"routeExplainEmbed": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeExplainEmbed: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.EmbedRequest](
					raw,
					cfg.extensions,
					"inference.routeExplainEmbed",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				explanation, decision, err := router.ExplainEmbed(ctx, req)
				if err != nil {
					return nil, err
				}
				return routeExplainResult(
					router,
					explanation,
					decision,
					"inference.routeExplainEmbed",
				)
			},
			"transcribe": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.transcribe: no assembly wired")
				}
				ref, req, extensions, err := parseInferenceModelCall[inference.TranscriptionRequest](
					raw,
					cfg.extensions,
					"inference.transcribe",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				resp, err := assembly.Transcribe(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(resp, "inference.transcribe response")
			},
			"routeTranscribe": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeTranscribe: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.TranscriptionRequest](
					raw,
					cfg.extensions,
					"inference.routeTranscribe",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				resp, trace, err := router.Transcribe(ctx, req)
				if err != nil {
					return nil, err
				}
				out, err := toScriptJSON(resp, "inference.routeTranscribe response")
				if err != nil {
					return nil, err
				}
				return attachTrace(out, trace, "inference.routeTranscribe")
			},
			"explainTranscribe": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.explainTranscribe: no assembly wired")
				}
				ref, req, extensions, err := parseInferenceModelCall[inference.TranscriptionRequest](
					raw,
					cfg.extensions,
					"inference.explainTranscribe",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				explanation, err := assembly.ExplainTranscribe(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(explanation, "inference.explainTranscribe response")
			},
			"routeExplainTranscribe": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeExplainTranscribe: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.TranscriptionRequest](
					raw,
					cfg.extensions,
					"inference.routeExplainTranscribe",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				explanation, decision, err := router.ExplainTranscribe(ctx, req)
				if err != nil {
					return nil, err
				}
				return routeExplainResult(
					router,
					explanation,
					decision,
					"inference.routeExplainTranscribe",
				)
			},
			"transcribeSession": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.transcribeSession: no assembly wired")
				}
				ref, req, extensions, err := parseInferenceModelCall[inference.TranscriptionSessionRequest](
					raw,
					cfg.extensions,
					"inference.transcribeSession",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				session, err := assembly.TranscribeSession(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return newTranscriptionSessionHandle(ctx, session, nil), nil
			},
			"routeTranscribeSession": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeTranscribeSession: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.TranscriptionSessionRequest](
					raw,
					cfg.extensions,
					"inference.routeTranscribeSession",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				session, trace, err := router.TranscribeSession(ctx, req)
				if err != nil {
					return nil, err
				}
				return newTranscriptionSessionHandle(ctx, session, &trace), nil
			},
			"stream": func(raw any) (any, error) {
				if assembly == nil {
					return nil, errdefs.NotAvailablef("inference.stream: no assembly wired")
				}
				ref, req, err := parseInferenceGenerateCall(
					raw,
					cfg.extensions,
					"inference.stream",
				)
				if err != nil {
					return nil, err
				}
				stream, err := assembly.GenerateStream(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return newStreamHandle(ctx, stream, nil), nil
			},
			"routeStream": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeStream: no router wired")
				}
				req, extensions, err := parseInferenceRouteCall[inference.GenerateRequest](
					raw,
					cfg.extensions,
					"inference.routeStream",
				)
				if err != nil {
					return nil, err
				}
				req.Extensions = extensions
				stream, trace, err := router.GenerateStream(ctx, req)
				if err != nil {
					return nil, err
				}
				return newStreamHandle(ctx, stream, &trace), nil
			},
		}
	}
}

// newStreamHandle adapts an inference.GenerateStream into the
// script-facing iterator handle:
//   - next() -> one GenerateStreamEvent projected verbatim, or null
//     once the stream ends (EOF) or fails; errors surface as script
//     exceptions and terminate the iteration
//   - result() -> the driver-accumulated GenerateResponse (the exact
//     shape generate/route return), valid only after next() returned
//     null so partial streams cannot be mistaken for complete ones
//   - close() -> idempotent early exit; reading to EOF does not
//     require it, abandoning a stream mid-way does
//
// trace, when non-nil (router-opened streams), is attached to the
// result object under "trace", mirroring inference.route.
func newStreamHandle(ctx context.Context, stream inference.GenerateStream, trace *route.Trace) map[string]any {
	done := false
	closed := false
	return map[string]any{
		"next": func() (any, error) {
			if done {
				return nil, nil
			}
			event, err := stream.Next(ctx)
			if err != nil {
				done = true
				if errors.Is(err, io.EOF) {
					return nil, nil
				}
				return nil, err
			}
			return toScriptJSON(event, "inference.stream event")
		},
		"result": func() (any, error) {
			if !done {
				return nil, errdefs.Validationf("inference.stream.result: read the stream to completion (next() -> null) first")
			}
			resp, err := stream.Result()
			if err != nil {
				return nil, err
			}
			out, err := toScriptJSON(resp, "inference.stream result")
			if err != nil {
				return nil, err
			}
			obj, ok := out.(map[string]any)
			if !ok {
				return nil, errdefs.Internalf("inference.stream: result projection is %T, want object", out)
			}
			if trace != nil {
				traceJSON, err := toScriptJSON(*trace, "inference.stream trace")
				if err != nil {
					return nil, err
				}
				obj["trace"] = traceJSON
			}
			return obj, nil
		},
		"close": func() error {
			done = true
			if closed {
				return nil
			}
			closed = true
			return stream.Close()
		},
	}
}

// newTranscriptionSessionHandle adapts an inference.TranscriptionSession
// into the script-facing handle:
//   - send(chunk) -> one media.AudioChunk wire JSON ({data, sequence});
//     fails once the session has drained or terminated
//   - next() -> one TranscriptionSessionEvent wire JSON, or null once the
//     session ends normally (EOF) or fails; errors surface as script
//     exceptions and terminate the iteration
//   - result() -> the driver-accumulated TranscriptionResponse (the exact
//     shape transcribe/routeTranscribe return), valid only after next()
//     returned null so partial sessions cannot be mistaken for complete ones
//   - interrupt() -> terminates the session abnormally (barge-in); the
//     interruption surfaces from the next next()/result() call
//   - close() -> idempotent early exit
//
// trace, when non-nil (router-opened sessions), is attached to the result
// object under "trace", mirroring inference.routeTranscribe.
func newTranscriptionSessionHandle(
	ctx context.Context,
	session inference.TranscriptionSession,
	trace *route.Trace,
) map[string]any {
	done := false
	closed := false
	return map[string]any{
		"send": func(chunk any) error {
			if done {
				return errdefs.Validationf(
					"inference.transcribeSession: session ended",
				)
			}
			var audio media.AudioChunk
			if err := decodeStrictJSON(
				chunk,
				&audio,
				"inference.transcribeSession.send",
			); err != nil {
				return err
			}
			return session.Send(ctx, audio)
		},
		"next": func() (any, error) {
			if done {
				return nil, nil
			}
			event, err := session.Next(ctx)
			if err != nil {
				done = true
				if errors.Is(err, io.EOF) {
					return nil, nil
				}
				return nil, err
			}
			return toScriptJSON(event, "inference.transcribeSession event")
		},
		"result": func() (any, error) {
			if !done {
				return nil, errdefs.Validationf(
					"inference.transcribeSession.result: drain the session (next() -> null) first",
				)
			}
			resp, err := session.Result()
			if err != nil {
				return nil, err
			}
			out, err := toScriptJSON(resp, "inference.transcribeSession result")
			if err != nil {
				return nil, err
			}
			obj, ok := out.(map[string]any)
			if !ok {
				return nil, errdefs.Internalf(
					"inference.transcribeSession: result projection is %T, want object",
					out,
				)
			}
			if trace != nil {
				traceJSON, err := toScriptJSON(
					*trace,
					"inference.transcribeSession trace",
				)
				if err != nil {
					return nil, err
				}
				obj["trace"] = traceJSON
			}
			return obj, nil
		},
		"interrupt": func() error {
			return session.Interrupt()
		},
		"close": func() error {
			done = true
			if closed {
				return nil
			}
			closed = true
			return session.Close()
		},
	}
}

// parseInferenceGenerateCall splits the script-facing generate-family
// request into the model target and the canonical GenerateRequest.
func parseInferenceGenerateCall(
	raw any,
	decoders map[string]inference.ExtensionDecoder,
	field string,
) (inference.ModelRef, inference.GenerateRequest, error) {
	ref, req, extensions, err := parseInferenceModelCall[inference.GenerateRequest](
		raw,
		decoders,
		field,
	)
	if err != nil {
		return ref, req, err
	}
	req.Extensions = extensions
	return ref, req, nil
}

// parseInferenceModelCall splits a script-facing model-addressed request
// into the model target and the canonical request. "model" is a
// bridge-level key (the wire request has no model field — routing
// ownership lives with the caller, not the payload), so it is stripped
// before the strict decode; "extensions" is stripped the same way and
// resolved through the host-registered decoders. The caller assigns the
// resolved extensions to req.Extensions (the wire structs keep the field
// JSON-hidden).
func parseInferenceModelCall[T any](
	raw any,
	decoders map[string]inference.ExtensionDecoder,
	field string,
) (inference.ModelRef, T, inference.Extensions, error) {
	var ref inference.ModelRef
	var req T
	obj, ok := raw.(map[string]any)
	if !ok {
		return ref, req, nil, errdefs.Validationf(
			"%s: expected an object, got %T",
			field,
			raw,
		)
	}
	modelRaw, ok := obj["model"]
	if !ok || modelRaw == nil {
		return ref, req, nil, errdefs.Validationf(
			"%s: model is required (use the route entry point for router-chosen targets)",
			field,
		)
	}
	if err := decodeStrictJSON(modelRaw, &ref, field+".model"); err != nil {
		return ref, req, nil, err
	}
	rest := make(map[string]any, len(obj)-1)
	for k, v := range obj {
		if k != "model" {
			rest[k] = v
		}
	}
	extensions, err := decodeRequestRest(rest, &req, decoders, field)
	if err != nil {
		return ref, req, nil, err
	}
	return ref, req, extensions, nil
}

// parseInferenceEmbedCall splits the script-facing embed-family
// request into the model target and the canonical EmbedRequest. It
// mirrors parseInferenceGenerateCall through the shared generic parser.
func parseInferenceEmbedCall(
	raw any,
	decoders map[string]inference.ExtensionDecoder,
	field string,
) (inference.ModelRef, inference.EmbedRequest, error) {
	ref, req, extensions, err := parseInferenceModelCall[inference.EmbedRequest](
		raw,
		decoders,
		field,
	)
	if err != nil {
		return ref, req, err
	}
	req.Extensions = extensions
	return ref, req, nil
}

// attachTrace adds a route.Trace projection to a script-facing
// response object under "trace".
func attachTrace(out any, trace route.Trace, field string) (any, error) {
	obj, ok := out.(map[string]any)
	if !ok {
		return nil, errdefs.Internalf(
			"%s: response projection is %T, want object",
			field,
			out,
		)
	}
	traceJSON, err := toScriptJSON(trace, field+" trace")
	if err != nil {
		return nil, err
	}
	obj["trace"] = traceJSON
	return obj, nil
}

// routeExplainResult projects an explanation plus the selected model's
// declared limits into the script-facing {explanation, decision,
// limits} shape shared by every route*Explain entry point.
func routeExplainResult(
	router *route.Router,
	explanation inference.Explanation,
	decision route.Decision,
	field string,
) (any, error) {
	descriptor, err := router.Target().InspectModel(decision.Selected)
	if err != nil {
		return nil, errdefs.Internalf("%s: inspect selected model: %v", field, err)
	}
	out, err := toScriptJSON(map[string]any{
		"explanation": explanation,
		"decision":    decision,
		"limits":      descriptor.Limits,
	}, field+" response")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseInferenceRouteCall decodes a router-entry request: no model key
// (rejected by the strict decode), extensions stripped and resolved
// like the runtime entry. The caller assigns the resolved extensions
// to req.Extensions.
func parseInferenceRouteCall[T any](
	raw any,
	decoders map[string]inference.ExtensionDecoder,
	field string,
) (T, inference.Extensions, error) {
	var req T
	obj, ok := raw.(map[string]any)
	if !ok {
		return req, nil, errdefs.Validationf("%s: expected an object, got %T", field, raw)
	}
	extensions, err := decodeRequestRest(obj, &req, decoders, field)
	if err != nil {
		return req, nil, err
	}
	return req, extensions, nil
}

// decodeRequestRest strips the bridge-level "extensions" key, strict-
// decodes the remaining canonical wire into req, and resolves the
// extensions through the host registry. The caller assigns the
// resolved extensions to req.Extensions (the wire structs keep the
// field JSON-hidden).
func decodeRequestRest[T any](
	obj map[string]any,
	req *T,
	decoders map[string]inference.ExtensionDecoder,
	field string,
) (inference.Extensions, error) {
	extRaw, rest := splitExtensionsKey(obj)
	if err := decodeStrictJSON(rest, req, field); err != nil {
		return nil, err
	}
	return decodeScriptExtensions(extRaw, decoders, field+".extensions")
}

// splitExtensionsKey pulls the bridge-level "extensions" key out of a
// script request object, leaving only canonical GenerateRequest keys.
func splitExtensionsKey(obj map[string]any) (extRaw any, rest map[string]any) {
	extRaw, has := obj["extensions"]
	if !has || extRaw == nil {
		return nil, obj
	}
	rest = make(map[string]any, len(obj)-1)
	for k, v := range obj {
		if k != "extensions" {
			rest[k] = v
		}
	}
	return extRaw, rest
}

// decodeScriptExtensions resolves a script "extensions" array —
// entries of {provider, id, fields} — into typed extensions via the
// host-registered decoders. Strict decoding rejects unknown entry
// keys as typos.
func decodeScriptExtensions(raw any, decoders map[string]inference.ExtensionDecoder, field string) (inference.Extensions, error) {
	if raw == nil {
		return nil, nil
	}
	var entries []inference.ExtensionEntry
	if err := decodeStrictJSON(raw, &entries, field); err != nil {
		return nil, err
	}
	return inference.DecodeExtensions(entries, decoders, field)
}
