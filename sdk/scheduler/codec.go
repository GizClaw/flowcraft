package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// NewJSONPayload encodes value as a versioned JSON payload.
func NewJSONPayload(kind string, version int, value any) (Payload, error) {
	payload := Payload{Kind: kind, Version: version}
	if err := required("Payload.Kind", kind); err != nil {
		return Payload{}, err
	}
	if version <= 0 {
		return Payload{}, invalidf("Payload.Version must be greater than zero")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Payload{}, invalidf("encode Payload.Data: %v", err)
	}
	payload.Data = data
	if err := payload.Validate(); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

// DecodeJSON strictly decodes payload after matching its expected kind and version.
func DecodeJSON[T any](payload Payload, expectedKind string, expectedVersion int) (T, error) {
	var zero T
	if err := required("expected payload kind", expectedKind); err != nil {
		return zero, err
	}
	if expectedVersion <= 0 {
		return zero, invalidf("expected payload version must be greater than zero")
	}
	if err := payload.Validate(); err != nil {
		return zero, err
	}
	if payload.Kind != expectedKind {
		return zero, invalidf("payload kind %q does not match expected %q", payload.Kind, expectedKind)
	}
	if payload.Version != expectedVersion {
		return zero, invalidf("payload version %d does not match expected %d", payload.Version, expectedVersion)
	}
	var value T
	if err := decodeStrict(payload.Data, &value, true); err != nil {
		return zero, invalidf("decode payload %q v%d: %v", expectedKind, expectedVersion, err)
	}
	return value, nil
}

func decodeStrict(data []byte, out any, rejectUnknown bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if rejectUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}
