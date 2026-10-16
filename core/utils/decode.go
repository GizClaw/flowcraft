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
	"math"
	"sort"
	"strconv"
	"strings"
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
//
// A YAML document in which two distinct keys of one mapping would become the
// same JSON key is refused instead of converted: see keyCollisions.
func ToJSON(data []byte) ([]byte, error) {
	if FormatOf(data) == FormatJSON {
		if !json.Valid(data) {
			return nil, fmt.Errorf("config utils: invalid JSON document")
		}
		return data, nil
	}
	document, err := ensureSingleYAMLDocument(data)
	if err != nil {
		return nil, fmt.Errorf("config utils: %w", err)
	}
	var value any
	if err := yaml.UnmarshalStrict(data, &value); err != nil {
		return nil, fmt.Errorf("config utils: convert YAML to JSON: %w", err)
	}
	if collisions := keyCollisions(document); len(collisions) > 0 {
		return nil, fmt.Errorf("config utils: convert YAML to JSON: %s",
			strings.Join(collisions, "; "))
	}
	out, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("config utils: encode JSON: %w", err)
	}
	return out, nil
}

// ensureSingleYAMLDocument decodes the YAML document in data and rejects
// streams with more than one. sigs.k8s.io/yaml silently decodes only the
// first document, which would let a stray "---" separator hide
// configuration; the deployment protocol treats that as a parse error.
//
// It returns the document as yaml.v2 spells it — before the conversion to
// JSON rewrites its mapping keys — because that spelling is what makes the
// conversion auditable: see keyCollisions.
func ensureSingleYAMLDocument(data []byte) (any, error) {
	decoder := yamlv2.NewDecoder(bytes.NewReader(data))
	var first any
	if err := decoder.Decode(&first); err != nil && err != io.EOF {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple YAML documents")
		}
		return nil, err
	}
	return first, nil
}

// keyCollisions reports the mappings of a decoded YAML document in which
// two distinct YAML keys are one and the same JSON key, in the sorted
// order of that JSON key.
//
// Such a document has no defined conversion to JSON at all: sigs.k8s.io/yaml
// spells mapping keys while iterating a Go map, so which of the colliding
// values survives is decided by map iteration order, i.e. per process.
// Measured on `1` against `"1"` over 400 runs: one value 353 times and the
// other 47, which is the start-offset distribution of a small Go map.
//
// The refusal is by key, not by value: a pair that collapses onto two equal
// values converts to the same JSON whichever of them survives, and is still
// refused, because the document it converts to holds one JSON key twice and
// that is then for whichever JSON library reads it next to decide rather than
// for the document to state.
//
// The spelling is lossy in more than one way, so the set is wider than
// int-versus-string: a YAML 1.1 octal key is spelled in decimal (`010` and
// `"8"`), the infinities and NaN are spelled `.inf`, `-.inf` and `.nan`,
// each of them colliding with its own quoted string, and a float key is
// spelled at float32 precision, which loses more than digits: `1` and
// `1.0000000001` are both "1", a float above the float32 range is ".inf",
// and one below it is "0".
//
// A key type the conversion itself refuses is not considered here: such a
// document fails the strict decode in ToJSON before it can be ambiguous,
// and that error is the one reported.
func keyCollisions(value any) []string {
	var collisions []keyCollision
	collectKeyCollisions(value, &collisions)
	sort.Slice(collisions, func(i, j int) bool {
		if collisions[i].jsonKey != collisions[j].jsonKey {
			return collisions[i].jsonKey < collisions[j].jsonKey
		}
		return collisions[i].String() < collisions[j].String()
	})
	out := make([]string, 0, len(collisions))
	for _, collision := range collisions {
		// An aliased anchor is decoded once per alias, so the same mapping is
		// walked once per path to it and reports the same sentence every
		// time. One sentence per distinct collision rather than per place it
		// was found -- which also merges two mappings that collide the same
		// way, since a sentence names keys and not locations.
		if report := collision.String(); len(out) == 0 || out[len(out)-1] != report {
			out = append(out, report)
		}
	}
	return out
}

// keyCollision is one JSON key that two or more YAML keys of a single
// mapping are spelled as, with those keys' spellings.
type keyCollision struct {
	jsonKey  string
	yamlKeys []string
}

func (c keyCollision) String() string {
	return fmt.Sprintf("YAML keys %s convert to the same JSON key %q",
		strings.Join(c.yamlKeys, ", "), c.jsonKey)
}

func collectKeyCollisions(value any, collisions *[]keyCollision) {
	switch typed := value.(type) {
	case map[interface{}]interface{}:
		// yaml.v2 decodes a mapping into this shape whenever the target is
		// an `any`, nested mappings included.
		spelled := make(map[string][]string, len(typed))
		for key, entry := range typed {
			if jsonKey, ok := jsonKeySpelling(key); ok {
				spelled[jsonKey] = append(spelled[jsonKey], yamlKeySpelling(key))
			}
			collectKeyCollisions(entry, collisions)
		}
		for jsonKey, yamlKeys := range spelled {
			if len(yamlKeys) < 2 {
				continue
			}
			sort.Strings(yamlKeys)
			*collisions = append(*collisions, keyCollision{
				jsonKey:  jsonKey,
				yamlKeys: yamlKeys,
			})
		}
	case []interface{}:
		for _, entry := range typed {
			collectKeyCollisions(entry, collisions)
		}
	}
}

// jsonKeySpelling reproduces v1.4.0 of sigs.k8s.io/yaml: a mapping key is
// a JSON key when it is a string, decimal when it is an int or an int64,
// and for a float64 the `g` form at float32 precision with Go's "+Inf",
// "-Inf" and "NaN" spelled the YAML way.
//
// TestJSONKeySpelling_MatchesLibrary asks this function for a key and reads
// the library's own answer back out of the library's conversion of the same
// document, form by form, so a drift between the two fails a test instead of
// quietly leaving the refusal above unable to find a collapse.
func jsonKeySpelling(key any) (string, bool) {
	switch typed := key.(type) {
	case string:
		return typed, true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		switch s := strconv.FormatFloat(typed, 'g', -1, 32); s {
		case "+Inf":
			return ".inf", true
		case "-Inf":
			return "-.inf", true
		case "NaN":
			return ".nan", true
		default:
			return s, true
		}
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

// yamlKeySpelling names a YAML key the way the document's own syntax would:
// the tag it resolved to, then its value. The tag is what keeps two keys of
// one collision apart — `!!int 1` and `!!float 1` are both printed as "1"
// by their JSON key, and a reader has to be able to tell which is which. It
// is also what tells them apart when the two spellings do not look alike at
// all: `!!float 1e+40` is the JSON key ".inf".
func yamlKeySpelling(key any) string {
	switch typed := key.(type) {
	case string:
		return fmt.Sprintf("!!str %q", typed)
	case int:
		return fmt.Sprintf("!!int %d", typed)
	case int64:
		return fmt.Sprintf("!!int %d", typed)
	case float64:
		return "!!float " + yamlFloatSpelling(typed)
	case bool:
		return fmt.Sprintf("!!bool %t", typed)
	default:
		return fmt.Sprintf("%T", key)
	}
}

// yamlFloatSpelling names a float the way YAML spells it: the infinities and
// NaN by the words of the format, everything else in Go's shortest form at
// full precision. Full precision on purpose — the point of the message is
// that the value the document wrote is not the value its JSON key says.
func yamlFloatSpelling(value float64) string {
	switch {
	case math.IsInf(value, 1):
		return ".inf"
	case math.IsInf(value, -1):
		return "-.inf"
	case math.IsNaN(value):
		return ".nan"
	default:
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
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
