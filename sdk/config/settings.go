package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Opaque captures a JSON subtree without decoding it. Implementing
// UnmarshalJSON lets an enclosing strict decoder keep factory-owned
// payloads opaque while every other field stays strictly checked.
type Opaque json.RawMessage

// UnmarshalJSON stores the subtree verbatim.
func (o *Opaque) UnmarshalJSON(data []byte) error {
	if o == nil {
		return fmt.Errorf("config: UnmarshalJSON on nil Opaque")
	}
	*o = append((*o)[:0], data...)
	return nil
}

// Bytes returns the captured subtree verbatim as JSON.
func (o *Opaque) Bytes() []byte {
	if o == nil {
		return nil
	}
	return []byte(*o)
}

// Decode decodes the captured subtree into target with strict JSON
// semantics: unknown fields are errors. target must be a non-nil
// pointer. A nil Opaque leaves target unchanged.
func (o *Opaque) Decode(target any) error {
	if o == nil || len(*o) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(*o))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// DecodeSettings decodes a JSON settings subtree into T with strict
// decoding: unknown keys are errors, so a typo in configuration fails
// the build instead of silently dropping policy. Every resource and
// hook factory SHOULD decode through this helper.
//
// A nil or empty subtree decodes as the zero value of T.
func DecodeSettings[T any](raw json.RawMessage) (T, error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON documents")
		}
		return out, err
	}
	return out, nil
}
