package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
)

// Extensions cross the boundary through host-registered decoders:
// the script adds an "extensions" array of {provider, id, fields}
// objects to the request, and the bridge resolves each entry through
// the decoders the host wired with WithExtensionDecoder. Unregistered
// identities fail with a validation error — scripts can only pick
// from the menu the host explicit offers, never invent provider knobs.

// ExtensionDecoder builds one typed extension from its script-facing
// fields object. Decoding must be strict: unknown fields are typos,
// not optional extras.
type ExtensionDecoder func(fields json.RawMessage) (inference.Extension, error)

// ExtensionDecoderFor adapts a factory for a provider's option struct
// into an ExtensionDecoder. T must be a pointer type whose fields
// carry the provider's JSON contract (e.g. func() *kimi.GenerateOptions).
func ExtensionDecoderFor[T inference.Extension](factory func() T) ExtensionDecoder {
	return func(fields json.RawMessage) (inference.Extension, error) {
		ext := factory()
		value := reflect.ValueOf(ext)
		if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
			return nil, errdefs.Internalf("extension factory returned %T, want a non-nil pointer", ext)
		}
		dec := json.NewDecoder(bytes.NewReader(fields))
		dec.DisallowUnknownFields()
		if err := dec.Decode(ext); err != nil {
			return nil, errdefs.Validationf("extension fields: %v", err)
		}
		return ext, nil
	}
}

// InferenceBridgeOption customizes NewInferenceBridge.
type InferenceBridgeOption func(*inferenceBridgeConfig)

type inferenceBridgeConfig struct {
	extensions map[string]ExtensionDecoder
}

// WithExtensionDecoder registers the decoder for one
// provider/extension identity pair, keyed "provider/id".
func WithExtensionDecoder(provider, id string, decoder ExtensionDecoder) InferenceBridgeOption {
	return func(cfg *inferenceBridgeConfig) {
		if cfg.extensions == nil {
			cfg.extensions = make(map[string]ExtensionDecoder)
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
func NewInferenceBridge(runtime *inference.Runtime, router *route.Router, opts ...InferenceBridgeOption) BindingFunc {
	cfg := &inferenceBridgeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return func(ctx context.Context) (string, any) {
		return "inference", map[string]any{
			"generate": func(raw any) (any, error) {
				if runtime == nil {
					return nil, errdefs.NotAvailablef("inference.generate: no runtime wired")
				}
				ref, req, err := parseInferenceGenerateCall(raw, cfg.extensions)
				if err != nil {
					return nil, err
				}
				resp, err := runtime.Generate(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return toScriptJSON(resp, "inference.generate response")
			},
			"route": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.route: no router wired")
				}
				req, err := parseInferenceRouteCall(raw, cfg.extensions, "inference.route")
				if err != nil {
					return nil, err
				}
				resp, trace, err := router.Generate(ctx, req)
				if err != nil {
					return nil, err
				}
				out, err := toScriptJSON(resp, "inference.route response")
				if err != nil {
					return nil, err
				}
				obj, ok := out.(map[string]any)
				if !ok {
					return nil, errdefs.Internalf("inference.route: response projection is %T, want object", out)
				}
				traceJSON, err := toScriptJSON(trace, "inference.route trace")
				if err != nil {
					return nil, err
				}
				obj["trace"] = traceJSON
				return obj, nil
			},
			"stream": func(raw any) (any, error) {
				if runtime == nil {
					return nil, errdefs.NotAvailablef("inference.stream: no runtime wired")
				}
				ref, req, err := parseInferenceGenerateCall(raw, cfg.extensions)
				if err != nil {
					return nil, err
				}
				stream, err := runtime.GenerateStream(ctx, ref, req)
				if err != nil {
					return nil, err
				}
				return newStreamHandle(ctx, stream, nil), nil
			},
			"routeStream": func(raw any) (any, error) {
				if router == nil {
					return nil, errdefs.NotAvailablef("inference.routeStream: no router wired")
				}
				req, err := parseInferenceRouteCall(raw, cfg.extensions, "inference.routeStream")
				if err != nil {
					return nil, err
				}
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

// parseInferenceGenerateCall splits the script-facing generate request
// into the model target and the canonical GenerateRequest. "model" is
// a bridge-level key (the wire request has no model field — routing
// ownership lives with the caller, not the payload), so it is stripped
// before the strict decode; "extensions" is stripped the same way and
// resolved through the host-registered decoders.
func parseInferenceGenerateCall(raw any, decoders map[string]ExtensionDecoder) (inference.ModelRef, inference.GenerateRequest, error) {
	var ref inference.ModelRef
	var req inference.GenerateRequest
	obj, ok := raw.(map[string]any)
	if !ok {
		return ref, req, errdefs.Validationf("inference.generate: expected an object, got %T", raw)
	}
	modelRaw, ok := obj["model"]
	if !ok || modelRaw == nil {
		return ref, req, errdefs.Validationf("inference.generate: model is required (use inference.route for router-chosen targets)")
	}
	if err := decodeStrictJSON(modelRaw, &ref, "inference.generate.model"); err != nil {
		return ref, req, err
	}
	rest := make(map[string]any, len(obj)-1)
	for k, v := range obj {
		if k != "model" {
			rest[k] = v
		}
	}
	if err := decodeRequestRest(rest, &req, decoders, "inference.generate"); err != nil {
		return ref, req, err
	}
	return ref, req, nil
}

// parseInferenceRouteCall decodes a router-entry request: no model
// key (rejected by the strict decode), extensions stripped and
// resolved like the runtime entry.
func parseInferenceRouteCall(raw any, decoders map[string]ExtensionDecoder, field string) (inference.GenerateRequest, error) {
	var req inference.GenerateRequest
	obj, ok := raw.(map[string]any)
	if !ok {
		return req, errdefs.Validationf("%s: expected an object, got %T", field, raw)
	}
	if err := decodeRequestRest(obj, &req, decoders, field); err != nil {
		return req, err
	}
	return req, nil
}

// decodeRequestRest strips the bridge-level "extensions" key, strict-
// decodes the remaining canonical wire into req, then resolves the
// extensions through the host registry.
func decodeRequestRest(obj map[string]any, req *inference.GenerateRequest, decoders map[string]ExtensionDecoder, field string) error {
	extRaw, rest := splitExtensionsKey(obj)
	if err := decodeStrictJSON(rest, req, field); err != nil {
		return err
	}
	extensions, err := decodeScriptExtensions(extRaw, decoders, field+".extensions")
	if err != nil {
		return err
	}
	req.Extensions = extensions
	return nil
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
// host-registered decoders. Unregistered identities are validation
// errors: the host's registry is the whole menu.
func decodeScriptExtensions(raw any, decoders map[string]ExtensionDecoder, field string) (inference.Extensions, error) {
	if raw == nil {
		return nil, nil
	}
	list, err := asAnyList(raw, field)
	if err != nil {
		return nil, err
	}
	var extensions inference.Extensions
	for i, item := range list {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, errdefs.Validationf("%s: expected an object, got %T", entryField, item)
		}
		provider, _ := obj["provider"].(string)
		id, _ := obj["id"].(string)
		if provider == "" || id == "" {
			return nil, errdefs.Validationf("%s: provider and id are required", entryField)
		}
		key := provider + "/" + id
		decoder, ok := decoders[key]
		if !ok {
			return nil, errdefs.Validationf("%s: extension %q is not registered by the host", entryField, key)
		}
		fieldsJSON, err := json.Marshal(obj["fields"])
		if err != nil {
			return nil, errdefs.Validationf("%s.fields: value is not JSON-encodable: %v", entryField, err)
		}
		extension, err := decoder(fieldsJSON)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entryField, err)
		}
		if extension.ProviderID() != provider || extension.ExtensionID() != id {
			return nil, errdefs.Internalf(
				"%s: decoder for %q returned extension %q/%q",
				entryField, key, extension.ProviderID(), extension.ExtensionID(),
			)
		}
		extensions = append(extensions, extension)
	}
	return extensions, nil
}
