// Package utils is the shared boundary between authoring formats and
// the JSON core protocol. It detects JSON the way Kubernetes does — the
// first non-whitespace byte is an open brace — and treats everything
// else as YAML, converting it to JSON before strict decoding. Core
// protocol packages never see YAML.
package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode"

	yamlv2 "gopkg.in/yaml.v2"
	"sigs.k8s.io/yaml"
)

// Format identifies the syntax of a configuration document.
type Format uint8

const (
	// FormatYAML is reported for every document that does not start
	// with an open brace.
	FormatYAML Format = iota
	// FormatJSON is reported when the first non-whitespace byte is an
	// open brace.
	FormatJSON
)

func (f Format) String() string {
	switch f {
	case FormatJSON:
		return "json"
	default:
		return "yaml"
	}
}

var jsonPrefix = []byte("{")

// FormatOf reports the syntax of data the way Kubernetes does: JSON
// when the first non-whitespace byte is an open brace, otherwise YAML.
// A JSON array is therefore reported as YAML, mirroring
// k8s.io/apimachinery/pkg/util/yaml, and handled by the YAML path.
func FormatOf(data []byte) Format {
	if hasJSONPrefix(data) {
		return FormatJSON
	}
	return FormatYAML
}

func hasJSONPrefix(buf []byte) bool {
	trim := bytes.TrimLeftFunc(buf, unicode.IsSpace)
	return bytes.HasPrefix(trim, jsonPrefix)
}

// ToJSON converts one configuration document to its JSON form. JSON
// input is validated and returned as-is; YAML input is converted
// strictly (duplicate mapping keys are rejected) through the same
// library Kubernetes uses, sigs.k8s.io/yaml.
func ToJSON(data []byte) ([]byte, error) {
	if FormatOf(data) == FormatJSON {
		if !json.Valid(data) {
			return nil, fmt.Errorf("config utils: invalid JSON document")
		}
		return data, nil
	}
	if err := ensureSingleYAMLDocument(data); err != nil {
		return nil, fmt.Errorf("config utils: %w", err)
	}
	var value any
	if err := yaml.UnmarshalStrict(data, &value); err != nil {
		return nil, fmt.Errorf("config utils: convert YAML to JSON: %w", err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("config utils: encode JSON: %w", err)
	}
	return out, nil
}

// ensureSingleYAMLDocument rejects streams with more than one YAML
// document. sigs.k8s.io/yaml silently decodes only the first document,
// which would let a stray "---" separator hide configuration; the
// deployment protocol treats that as a parse error.
func ensureSingleYAMLDocument(data []byte) error {
	decoder := yamlv2.NewDecoder(bytes.NewReader(data))
	var first any
	if err := decoder.Decode(&first); err != nil && err != io.EOF {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple YAML documents")
		}
		return err
	}
	return nil
}

// Decode decodes one configuration document into T. The document may
// be JSON or YAML; both are decoded with strict, JSON semantics:
// unknown fields are errors, and a trailing document is an error.
func Decode[T any](data []byte) (T, error) {
	var out T
	jsonData, err := ToJSON(data)
	if err != nil {
		return out, err
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, fmt.Errorf(
			"config utils: decode %s config: %w", FormatOf(data), err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON documents")
		}
		return out, fmt.Errorf(
			"config utils: decode %s config: %w", FormatOf(data), err)
	}
	return out, nil
}
