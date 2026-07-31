package bindings

import (
	"bytes"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// Generic projections between script-facing values (map[string]any,
// []any, scalars — the only shapes JS/Lua VMs understand) and
// inference types.
//
// The script-side contract IS the inference wire format: messages
// look like {"role": "...", "content": {"parts": [...]}} and parts
// like {"type": "text", "text": "..."} or {"type": "image",
// "source": {"kind": "url", "url": "...", "media_type": "..."}}.
// Instead of hand-written per-type mappings the projections
// round-trip through encoding/json, so the inference types' own JSON
// contract and validation apply in both directions and future part
// kinds work without code here. Decoding is strict (unknown fields
// rejected) so script typos surface during development.

// partsFromScript converts a script-supplied array into
// []inference.Part by decoding it through inference.Content (the type
// that owns part JSON). field prefixes the error path.
func partsFromScript(raw any, field string) ([]inference.Part, error) {
	list, err := asAnyList(raw, field)
	if err != nil {
		return nil, err
	}
	var content inference.Content
	if err := decodeStrictJSON(map[string]any{"parts": list}, &content, field); err != nil {
		return nil, err
	}
	if err := content.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: %v", field, err)
	}
	return content.Parts, nil
}

// partsToScript projects parts into script-facing objects. Nil stays
// nil so scripts can distinguish "no parts" from "empty array".
func partsToScript(parts []inference.Part) ([]any, error) {
	if parts == nil {
		return nil, nil
	}
	buf, err := json.Marshal(inference.Content{Parts: parts})
	if err != nil {
		return nil, errdefs.Internalf("parts are not JSON-encodable: %v", err)
	}
	var wire struct {
		Parts []any `json:"parts"`
	}
	if err := json.Unmarshal(buf, &wire); err != nil {
		return nil, errdefs.Internalf("parts projection: %v", err)
	}
	return wire.Parts, nil
}

// messageFromScript converts one script-supplied message object into
// an inference.Message. field prefixes the error path.
func messageFromScript(raw any, field string) (inference.Message, error) {
	var msg inference.Message
	if raw == nil {
		return msg, errdefs.Validationf("%s: message must be an object, got null", field)
	}
	if err := decodeStrictJSON(raw, &msg, field); err != nil {
		return msg, err
	}
	if err := msg.Validate(); err != nil {
		return msg, errdefs.Validationf("%s: %v", field, err)
	}
	return msg, nil
}

// messagesFromScript converts a script-supplied array into
// []inference.Message.
func messagesFromScript(raw any, field string) ([]inference.Message, error) {
	list, err := asAnyList(raw, field)
	if err != nil {
		return nil, err
	}
	msgs := make([]inference.Message, len(list))
	for i, item := range list {
		m, err := messageFromScript(item, field+"["+itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		msgs[i] = m
	}
	return msgs, nil
}

// messagesToScript projects messages into script-facing objects. Nil
// stays nil so scripts can distinguish "no messages" from "empty".
func messagesToScript(msgs []inference.Message) ([]any, error) {
	if msgs == nil {
		return nil, nil
	}
	out := make([]any, len(msgs))
	for i, m := range msgs {
		buf, err := json.Marshal(m)
		if err != nil {
			return nil, errdefs.Internalf("messages[%d]: message is not JSON-encodable: %v", i, err)
		}
		var mm map[string]any
		if err := json.Unmarshal(buf, &mm); err != nil {
			return nil, errdefs.Internalf("messages[%d]: %v", i, err)
		}
		out[i] = mm
	}
	return out, nil
}

// decodeStrictJSON marshals a script value and decodes it into target
// with unknown-field rejection, so typos fail at the boundary.
func decodeStrictJSON(raw any, target any, field string) error {
	buf, err := json.Marshal(raw)
	if err != nil {
		return errdefs.Validationf("%s: value is not JSON-encodable: %v", field, err)
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return errdefs.Validationf("%s: %v", field, err)
	}
	return nil
}

// toScriptJSON projects any JSON-marshalable value (typically an
// inference response type) into the script-facing shape — maps,
// slices, scalars. The value's own JSON contract defines what scripts
// see; there is no hand-maintained field list to drift.
func toScriptJSON(v any, field string) (any, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, errdefs.Internalf("%s: value is not JSON-encodable: %v", field, err)
	}
	var out any
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, errdefs.Internalf("%s: projection: %v", field, err)
	}
	return out, nil
}

// asAnyList asserts raw is a []any (the script-side array shape).
func asAnyList(raw any, field string) ([]any, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, errdefs.Validationf("%s: expected an array, got %T", field, raw)
	}
	return list, nil
}

// itoa avoids importing strconv for the rare error path.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
