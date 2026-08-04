package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/message"
)

// isNilValue reports whether value is a typed nil (e.g. (*message.Part)(nil))
// sitting behind an any. reflection.Value.IsNil only works on
// chan/func/interface/map/pointer/slice; this is the safe equivalent
// for an any that may carry one.
func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// normalizePart collapses the pointer method set inherited from the canonical
// value types. Both T and *T satisfy message.Part in Go; runtime boundaries therefore
// accept either form and normalize pointers back to values. This is the
// inference-side mirror of the helper inside the message package; it exists
// here because inference owns its own validation paths (ActiveFields, part
// role checks) and does not want to leak normalizePart through the public
// message API.
func normalizePart(part message.Part) (message.Part, error) {
	if isNilValue(part) {
		return nil, fmt.Errorf("content part is nil")
	}
	switch value := part.(type) {
	case message.TextPart, message.ImagePart, message.AudioPart, message.VideoPart,
		message.FilePart, message.DataPart, message.ToolCallPart,
		message.ToolResultPart, message.ReasoningPart:
		return value, nil
	case *message.TextPart:
		return *value, nil
	case *message.ImagePart:
		return *value, nil
	case *message.AudioPart:
		return *value, nil
	case *message.VideoPart:
		return *value, nil
	case *message.FilePart:
		return *value, nil
	case *message.DataPart:
		return *value, nil
	case *message.ToolCallPart:
		return *value, nil
	case *message.ToolResultPart:
		return *value, nil
	case *message.ReasoningPart:
		return *value, nil
	default:
		return nil, fmt.Errorf("unsupported content part type %T", part)
	}
}

// decodeStrict decodes a JSON object/value with strict-mode settings: it
// rejects unknown fields and trailing values. inference and message
// each carry their own copy because the helper is small and both packages
// want to keep the call site local to the type it is decoding.
func decodeStrict(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
