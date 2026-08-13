package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Opaque is a resource's raw settings subtree. It carries bytes only;
// interpretation belongs to the owning factory.
type Opaque json.RawMessage

// UnmarshalJSON accepts any valid JSON value.
func (o *Opaque) UnmarshalJSON(data []byte) error {
	if !json.Valid(data) {
		return errors.New("resource settings: invalid JSON")
	}
	*o = append((*o)[:0], data...)
	return nil
}

// Bytes returns the raw settings bytes.
func (o Opaque) Bytes() []byte { return o }

// Decode strictly decodes the settings into target.
func (o Opaque) Decode(target any) error {
	return DecodeSettings(target, o)
}

// DecodeSettings strictly decodes raw settings into target: unknown
// fields are rejected so a typo fails the build instead of silently
// dropping policy. When expansion options are given, scalar string
// references (${env:NAME}, ${base}, ~, ...) are expanded first; see
// [Expand].
func DecodeSettings(target any, raw []byte, opts ...ExpandOption) error {
	if len(opts) > 0 {
		expanded, err := Expand(raw, opts...)
		if err != nil {
			return err
		}
		raw = expanded
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return errdefs.Validationf("resource settings: %v", err)
	}
	return nil
}

// DecodeTyped is a typed convenience wrapper: it decodes raw into a
// fresh T and returns it, applying expansion options when given.
func DecodeTyped[T any](raw []byte, opts ...ExpandOption) (T, error) {
	var value T
	if err := DecodeSettings(&value, raw, opts...); err != nil {
		return value, fmt.Errorf("resource settings decode: %w", err)
	}
	return value, nil
}
