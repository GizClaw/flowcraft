package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
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

// SubDocument is the settings shape shared by every resource impl that
// wraps a module's own config loader — workspace, sandbox, inference,
// tool. Each of those modules parses a versioned document of its own,
// so a deployment does not restate their schemas: it says where the
// document is.
//
// Exactly one form must be present:
//
//	settings: {file: ./workspaces.json}   # standalone file
//
//	settings:                             # kept inline
//	  inline:
//	    version: v1
//	    workspaces:
//	      project: {driver: local, settings: {root: .}}
//
// Inline keeps the whole deployment in one document; file keeps large
// sections reviewable on their own. Both feed the module's own parser,
// so the module's version field and strictness still apply — there is
// no second schema to keep in sync. Module parsers should decode the
// returned bytes with sdk/config/utils, which accepts JSON directly and
// treats YAML as authoring sugar.
type SubDocument struct {
	File   string  `json:"file,omitempty"`
	Inline *Opaque `json:"inline,omitempty"`
}

// Bytes returns the sub-document bytes exactly as stored, ready for the
// module's parser.
//
// Requiring exactly one form is deliberate: a mistyped file key that
// silently fell back to an empty inline document would produce an empty
// registry and fail much later, at the first missing ref.
func (s SubDocument) Bytes() ([]byte, error) {
	switch {
	case s.File != "" && s.Inline != nil:
		return nil, errdefs.Validationf(
			"config settings: file and inline are mutually exclusive")
	case s.File != "":
		data, err := os.ReadFile(s.File)
		if err != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"config settings: read %s: %w", s.File, err))
		}
		return data, nil
	case s.Inline != nil:
		data := s.Inline.Bytes()
		if len(data) == 0 {
			return nil, errdefs.Validationf(
				"config settings: inline document is empty")
		}
		return data, nil
	default:
		return nil, errdefs.Validationf(
			"config settings: file or inline is required")
	}
}
