package bindings

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// Wire plumbing shared by the entry points: the request parsers that turn
// a script argument into a canonical request plus resolved extensions,
// the response projectors that add a route trace or a model's declared
// limits, and the iterator handles the streaming and duplex entry points
// hand back to the script.

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
		"finish": func() error {
			if done {
				return nil
			}
			finisher, ok := session.(inference.TranscriptionSessionFinisher)
			if !ok {
				return errdefs.Validationf(
					"inference.transcribeSession.finish: session does not support explicit end-of-input",
				)
			}
			return finisher.FinishInput(ctx)
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
) (model.ModelRef, inference.GenerateRequest, error) {
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
) (model.ModelRef, T, inference.Extensions, error) {
	var ref model.ModelRef
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
) (model.ModelRef, inference.EmbedRequest, error) {
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
