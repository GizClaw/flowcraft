package bindings

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// This file isolates the model.* → map[string]any projections that
// the LLM bridge serves up to scripts. Keeping projection separate
// from round logic and the bridge facade lets us evolve the script
// surface without touching execution code.
//
// Projection rules:
//   - "type" is always present and reuses the canonical model.PartType
//     string (e.g. "text", "image", "tool_call").
//   - Only fields populated on the source struct land in the output.
//     A nil pointer or zero scalar is omitted; scripts can safely
//     check `if (p.image)` to detect presence without hasOwnProperty.
//   - Field names use snake_case to match the LLM bridge's input
//     schema and the model package's JSON tags.
//   - We never expose Go-only zero values (e.g. an empty MediaRef);
//     when a struct has nothing meaningful, the surrounding key is
//     omitted entirely so scripts get a stable shape.

// partToMap projects one model.Part into a script-facing object.
//
// The output always carries "type"; type-specific fields live under
// keys that mirror model.Part's JSON tags (text/image/audio/file/
// data/tool_call/tool_result). Unknown PartTypes still produce a
// {"type": ...} object so scripts can branch defensively on
// future-added types.
func partToMap(p model.Part) map[string]any {
	out := map[string]any{"type": string(p.Type)}
	switch p.Type {
	case model.PartText:
		if p.Text != "" {
			out["text"] = p.Text
		}
	case model.PartImage:
		if p.Image != nil {
			out["image"] = mediaRefToMap(*p.Image)
		}
	case model.PartAudio:
		if p.Audio != nil {
			out["audio"] = mediaRefToMap(*p.Audio)
		}
	case model.PartFile:
		if p.File != nil {
			out["file"] = fileRefToMap(*p.File)
		}
	case model.PartData:
		if p.Data != nil {
			out["data"] = dataRefToMap(*p.Data)
		}
	case model.PartToolCall:
		if p.ToolCall != nil {
			out["tool_call"] = toolCallToMap(*p.ToolCall)
		}
	case model.PartToolResult:
		if p.ToolResult != nil {
			out["tool_result"] = toolResultToMap(*p.ToolResult)
		}
	}
	return out
}

// partsToList projects a slice of parts; nil stays nil so scripts
// can distinguish "no parts" from "empty array" if they care.
func partsToList(parts []model.Part) []map[string]any {
	if parts == nil {
		return nil
	}
	out := make([]map[string]any, len(parts))
	for i, p := range parts {
		out[i] = partToMap(p)
	}
	return out
}

// messageToMap projects one model.Message. Role plus parts is the
// canonical shape; we also expose `content` (= Message.Content()) as
// a convenience for scripts that only care about the concatenated
// text and don't want to walk parts themselves.
func messageToMap(m model.Message) map[string]any {
	out := map[string]any{
		"role":  string(m.Role),
		"parts": partsToList(m.Parts),
	}
	if c := m.Content(); c != "" {
		out["content"] = c
	}
	return out
}

// messagesToList projects a slice of messages.
func messagesToList(msgs []model.Message) []map[string]any {
	if msgs == nil {
		return nil
	}
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = messageToMap(m)
	}
	return out
}

// toolCallToMap matches the schema scripts already see in
// roundResult.tool_calls so a part-level call object and the result
// summary stay shape-compatible.
func toolCallToMap(tc model.ToolCall) map[string]any {
	return map[string]any{
		"id":        tc.ID,
		"name":      tc.Name,
		"arguments": tc.Arguments,
	}
}

func toolCallsToList(calls []model.ToolCall) []map[string]any {
	if calls == nil {
		return nil
	}
	out := make([]map[string]any, len(calls))
	for i, tc := range calls {
		out[i] = toolCallToMap(tc)
	}
	return out
}

// toolResultToMap is the script-facing shape for a single tool
// invocation result. is_error defaults to false on the Go side, so
// we always include it explicitly to spare scripts a `?? false` dance.
func toolResultToMap(tr model.ToolResult) map[string]any {
	return map[string]any{
		"tool_call_id": tr.ToolCallID,
		"content":      tr.Content,
		"is_error":     tr.IsError,
	}
}

func toolResultsToList(results []model.ToolResult) []map[string]any {
	if results == nil {
		return nil
	}
	out := make([]map[string]any, len(results))
	for i, tr := range results {
		out[i] = toolResultToMap(tr)
	}
	return out
}

// mediaRefToMap projects an image / audio reference. Only non-empty
// fields are emitted so scripts can rely on `if (m.url)` rather than
// checking for empty strings.
func mediaRefToMap(m model.MediaRef) map[string]any {
	out := make(map[string]any, 3)
	if m.URL != "" {
		out["url"] = m.URL
	}
	if m.Base64 != "" {
		out["base64"] = m.Base64
	}
	if m.MediaType != "" {
		out["media_type"] = m.MediaType
	}
	return out
}

// fileRefToMap projects a generic file reference. URI is the only
// required field on the Go side; we keep the same minimum here.
func fileRefToMap(f model.FileRef) map[string]any {
	out := map[string]any{"uri": f.URI}
	if f.MimeType != "" {
		out["mime_type"] = f.MimeType
	}
	if f.Name != "" {
		out["name"] = f.Name
	}
	return out
}

// dataRefToMap projects a data reference. Value is intentionally
// passed through as-is: the model contract says Value is a
// JSON-compatible map, so script runtimes (jsrt, luart) can already
// consume it directly without a per-key copy.
func dataRefToMap(d model.DataRef) map[string]any {
	out := map[string]any{"value": d.Value}
	if d.MimeType != "" {
		out["mime_type"] = d.MimeType
	}
	return out
}

// usageToMap projects token accounting. We keep the int64 native
// types so jsrt/luart preserve numeric precision; both runtimes
// already handle int64 via reflect.
func usageToMap(u model.TokenUsage) map[string]any {
	return map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  u.TotalTokens,
	}
}

// ===========================================================================
// Reverse projections — script-supplied map[string]any → model.* types
// ===========================================================================
//
// Scripts hand back conversation history through the board bridge
// (setChannel / appendChannel). The shape on the script side mirrors
// what messageToMap / partToMap emit, so a script can round-trip:
//
//	var msgs = board.channel("main");
//	msgs.push({ role: "user", parts: [{ type: "text", text: "go on" }] });
//	board.setChannel("main", msgs);
//
// Reverse marshalers are intentionally strict:
//   - Unknown keys at any level are rejected with a path-prefixed error
//     so script typos surface during dev rather than turning into
//     silently-dropped data.
//   - Type mismatches (e.g. a number where text is expected) raise an
//     error rather than coercing — coercion would mask provider /
//     script bugs.
//   - Required fields are explicit per type: only "type" is universal;
//     each part type advertises its own required keys.

// allowedMessageKeys / allowedPart*Keys gate which top-level fields a
// reverse marshaler will accept. We keep these as small string sets
// (rather than dynamic struct tags via reflection) so the contract
// stays readable and the validation loop stays allocation-free.
var (
	allowedMessageKeys = map[string]struct{}{
		"role":    {},
		"parts":   {},
		"content": {}, // emitted by messageToMap as a convenience; ignored on input
	}
	allowedPartTextKeys       = map[string]struct{}{"type": {}, "text": {}}
	allowedPartImageKeys      = map[string]struct{}{"type": {}, "image": {}}
	allowedPartAudioKeys      = map[string]struct{}{"type": {}, "audio": {}}
	allowedPartFileKeys       = map[string]struct{}{"type": {}, "file": {}}
	allowedPartDataKeys       = map[string]struct{}{"type": {}, "data": {}}
	allowedPartToolCallKeys   = map[string]struct{}{"type": {}, "tool_call": {}}
	allowedPartToolResultKeys = map[string]struct{}{"type": {}, "tool_result": {}}
	allowedMediaRefKeys       = map[string]struct{}{"url": {}, "base64": {}, "media_type": {}}
	allowedFileRefKeys        = map[string]struct{}{"uri": {}, "mime_type": {}, "name": {}}
	allowedDataRefKeys        = map[string]struct{}{"value": {}, "mime_type": {}}
	allowedToolCallKeys       = map[string]struct{}{"id": {}, "name": {}, "arguments": {}}
	allowedToolResultKeys     = map[string]struct{}{"tool_call_id": {}, "content": {}, "is_error": {}}
)

// parseChannelMessages converts a script-supplied list (typically
// []any returned by getChannel + push, or freshly built by the script)
// into a []model.Message ready for board.SetChannel.
//
// ctx labels the script entry point that triggered the call so that
// errors point back to setChannel("main", …) rather than just "msgs[1]".
func parseChannelMessages(raw any, ctx string) ([]model.Message, error) {
	if raw == nil {
		return nil, nil
	}
	list, err := asAnyList(raw, ctx+": messages")
	if err != nil {
		return nil, err
	}
	out := make([]model.Message, len(list))
	for i, item := range list {
		msg, err := parseMessage(item, fmt.Sprintf("%s: messages[%d]", ctx, i))
		if err != nil {
			return nil, err
		}
		out[i] = msg
	}
	return out, nil
}

// parseMessage validates and converts one script-side message map.
func parseMessage(raw any, ctx string) (model.Message, error) {
	m, err := asStringMap(raw, ctx)
	if err != nil {
		return model.Message{}, err
	}
	if err := rejectUnknownKeys(m, allowedMessageKeys, ctx); err != nil {
		return model.Message{}, err
	}

	role, err := requiredString(m, "role", ctx)
	if err != nil {
		return model.Message{}, err
	}

	partsRaw, hasParts := m["parts"]
	if !hasParts {
		return model.Message{}, fmt.Errorf("%s: missing required field %q", ctx, "parts")
	}
	partsList, err := asAnyList(partsRaw, ctx+".parts")
	if err != nil {
		return model.Message{}, err
	}
	parts := make([]model.Part, len(partsList))
	for i, p := range partsList {
		part, err := parsePart(p, fmt.Sprintf("%s.parts[%d]", ctx, i))
		if err != nil {
			return model.Message{}, err
		}
		parts[i] = part
	}
	return model.Message{Role: model.Role(role), Parts: parts}, nil
}

// parsePart dispatches on "type" to a per-type validator. Unknown
// types are explicitly rejected — silently dropping them would let a
// script attach data the bridge couldn't represent.
func parsePart(raw any, ctx string) (model.Part, error) {
	m, err := asStringMap(raw, ctx)
	if err != nil {
		return model.Part{}, err
	}
	typeStr, err := requiredString(m, "type", ctx)
	if err != nil {
		return model.Part{}, err
	}
	pt := model.PartType(typeStr)
	switch pt {
	case model.PartText:
		if err := rejectUnknownKeys(m, allowedPartTextKeys, ctx); err != nil {
			return model.Part{}, err
		}
		text, err := optionalString(m, "text", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, Text: text}, nil

	case model.PartImage:
		if err := rejectUnknownKeys(m, allowedPartImageKeys, ctx); err != nil {
			return model.Part{}, err
		}
		ref, err := optionalMediaRef(m, "image", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, Image: ref}, nil

	case model.PartAudio:
		if err := rejectUnknownKeys(m, allowedPartAudioKeys, ctx); err != nil {
			return model.Part{}, err
		}
		ref, err := optionalMediaRef(m, "audio", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, Audio: ref}, nil

	case model.PartFile:
		if err := rejectUnknownKeys(m, allowedPartFileKeys, ctx); err != nil {
			return model.Part{}, err
		}
		ref, err := optionalFileRef(m, "file", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, File: ref}, nil

	case model.PartData:
		if err := rejectUnknownKeys(m, allowedPartDataKeys, ctx); err != nil {
			return model.Part{}, err
		}
		ref, err := optionalDataRef(m, "data", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, Data: ref}, nil

	case model.PartToolCall:
		if err := rejectUnknownKeys(m, allowedPartToolCallKeys, ctx); err != nil {
			return model.Part{}, err
		}
		tc, err := optionalToolCall(m, "tool_call", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, ToolCall: tc}, nil

	case model.PartToolResult:
		if err := rejectUnknownKeys(m, allowedPartToolResultKeys, ctx); err != nil {
			return model.Part{}, err
		}
		tr, err := optionalToolResult(m, "tool_result", ctx)
		if err != nil {
			return model.Part{}, err
		}
		return model.Part{Type: pt, ToolResult: tr}, nil

	default:
		return model.Part{}, fmt.Errorf("%s: unknown part type %q", ctx, typeStr)
	}
}

func optionalMediaRef(m map[string]any, key, ctx string) (*model.MediaRef, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, nil
	}
	sub, err := asStringMap(raw, ctx+"."+key)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(sub, allowedMediaRefKeys, ctx+"."+key); err != nil {
		return nil, err
	}
	url, err := optionalString(sub, "url", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	b64, err := optionalString(sub, "base64", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	mt, err := optionalString(sub, "media_type", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	return &model.MediaRef{URL: url, Base64: b64, MediaType: mt}, nil
}

func optionalFileRef(m map[string]any, key, ctx string) (*model.FileRef, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, nil
	}
	sub, err := asStringMap(raw, ctx+"."+key)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(sub, allowedFileRefKeys, ctx+"."+key); err != nil {
		return nil, err
	}
	uri, err := requiredString(sub, "uri", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	mime, err := optionalString(sub, "mime_type", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	name, err := optionalString(sub, "name", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	return &model.FileRef{URI: uri, MimeType: mime, Name: name}, nil
}

func optionalDataRef(m map[string]any, key, ctx string) (*model.DataRef, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, nil
	}
	sub, err := asStringMap(raw, ctx+"."+key)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(sub, allowedDataRefKeys, ctx+"."+key); err != nil {
		return nil, err
	}
	mime, err := optionalString(sub, "mime_type", ctx+"."+key)
	if err != nil {
		return nil, err
	}

	// Value is the only required field of DataRef; we accept the map
	// verbatim because the model contract already says it is JSON-shaped.
	valRaw, ok := sub["value"]
	if !ok {
		return nil, fmt.Errorf("%s.%s: missing required field %q", ctx, key, "value")
	}
	val, err := asStringMap(valRaw, ctx+"."+key+".value")
	if err != nil {
		return nil, err
	}
	return &model.DataRef{MimeType: mime, Value: val}, nil
}

func optionalToolCall(m map[string]any, key, ctx string) (*model.ToolCall, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, nil
	}
	sub, err := asStringMap(raw, ctx+"."+key)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(sub, allowedToolCallKeys, ctx+"."+key); err != nil {
		return nil, err
	}
	id, err := requiredString(sub, "id", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	name, err := requiredString(sub, "name", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	args, err := optionalString(sub, "arguments", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	return &model.ToolCall{ID: id, Name: name, Arguments: args}, nil
}

func optionalToolResult(m map[string]any, key, ctx string) (*model.ToolResult, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, nil
	}
	sub, err := asStringMap(raw, ctx+"."+key)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(sub, allowedToolResultKeys, ctx+"."+key); err != nil {
		return nil, err
	}
	id, err := requiredString(sub, "tool_call_id", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	content, err := optionalString(sub, "content", ctx+"."+key)
	if err != nil {
		return nil, err
	}
	isErr := false
	if v, ok := sub["is_error"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s.%s.is_error: expected bool, got %T", ctx, key, v)
		}
		isErr = b
	}
	return &model.ToolResult{ToolCallID: id, Content: content, IsError: isErr}, nil
}

// ---------------------------------------------------------------------------
// Generic helpers — keep validation loops short and consistent.
// ---------------------------------------------------------------------------

// asStringMap accepts the variants jsrt / luart can deliver:
//   - map[string]any   (most common path)
//   - nil              (treated as missing-with-error so callers
//     responsible for "not provided" decide the policy)
func asStringMap(v any, ctx string) (map[string]any, error) {
	if v == nil {
		return nil, fmt.Errorf("%s: expected object, got nil", ctx)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: expected object, got %T", ctx, v)
	}
	return m, nil
}

// asAnyList accepts the variants the bridge encounters in practice:
//
//   - []any                   — what luart / jsrt deliver from script
//     literals and from arrays they re-marshal back to Go.
//   - []map[string]any        — what messageToMap / partsToList emit
//     directly. Lets a script round-trip llm.run() results straight
//     into board.setChannel without an extra normalization pass.
//
// Anything else is rejected — the strict contract avoids surprises
// with arrays-of-numbers being misinterpreted as messages.
func asAnyList(v any, ctx string) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	switch list := v.(type) {
	case []any:
		return list, nil
	case []map[string]any:
		// Wrap so downstream callers see the uniform []any shape
		// without paying for it on every script-originated call.
		out := make([]any, len(list))
		for i, m := range list {
			out[i] = m
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: expected array, got %T", ctx, v)
	}
}

func requiredString(m map[string]any, key, ctx string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("%s: missing required field %q", ctx, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s: expected string, got %T", ctx, key, v)
	}
	return s, nil
}

func optionalString(m map[string]any, key, ctx string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s.%s: expected string, got %T", ctx, key, v)
	}
	return s, nil
}

func rejectUnknownKeys(m map[string]any, allowed map[string]struct{}, ctx string) error {
	for k := range m {
		if _, ok := allowed[k]; !ok {
			return fmt.Errorf("%s: unknown field %q", ctx, k)
		}
	}
	return nil
}

// roundResultToMap projects the bridge-internal roundResult into the
// shape scripts see from llm.run() / stream().finish(). Optional
// collections (tool_calls, tool_results) are omitted when empty so
// the typical text-only result stays small and easy to inspect.
//
// Schema:
//
//	{
//	    content:      string,                  // provider's raw text
//	    message:      messageToMap(...),       // assistant reply (with parts)
//	    messages:     [messageToMap, ...],     // conversation tail
//	    tool_pending: bool,
//	    tool_calls:   [toolCallToMap, ...],    // omitted when empty
//	    tool_results: [toolResultToMap, ...],  // omitted when empty
//	    usage:        usageToMap(...),
//	}
func roundResultToMap(r *roundResult) map[string]any {
	out := map[string]any{
		"content":      r.Content,
		"message":      messageToMap(r.Message),
		"messages":     messagesToList(r.Messages),
		"tool_pending": r.ToolPending,
		"usage":        usageToMap(r.Usage),
	}
	if len(r.ToolCalls) > 0 {
		out["tool_calls"] = toolCallsToList(r.ToolCalls)
	}
	if len(r.ToolResults) > 0 {
		out["tool_results"] = toolResultsToList(r.ToolResults)
	}
	return out
}
