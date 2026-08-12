package resource

import (
	"bytes"
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Source is a configuration subtree that is either inline content or a
// reference to a file / embedded asset. The reference forms are the
// whole-subtree objects {"file": path} and {"embed": name}; any other
// JSON value — a string, a structured object, an array — is inline
// content and is never resolved.
type Source struct {
	// Inline holds literal content when the source is not a
	// reference. For a JSON string the content is the decoded string;
	// otherwise it is the original JSON bytes.
	Inline []byte
	// File is the referenced file path (relative to the loader base
	// dir) when the source is {"file": path}.
	File string
	// Embed is the referenced fs.FS asset name when the source is
	// {"embed": name}.
	Embed string
}

// IsRef reports whether the source is a file or embed reference.
func (s Source) IsRef() bool {
	return s.File != "" || s.Embed != ""
}

// ParseSource interprets a settings subtree as a Source. Only the
// whole-subtree objects {"file": path} and {"embed": name} are
// references; everything else — including objects that merely contain
// a "file" or "embed" key alongside others — is inline content.
func ParseSource(raw json.RawMessage) (Source, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Source{}, errdefs.Validationf("resource source is empty")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return Source{}, errdefs.Validationf(
				"resource source: invalid string: %v", err)
		}
		return Source{Inline: []byte(text)}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return Source{}, errdefs.Validationf(
			"resource source: invalid JSON: %v", err)
	}
	if object == nil {
		return Source{}, errdefs.Validationf(
			"resource source: scalar values must be JSON strings")
	}
	if len(object) == 0 {
		return Source{}, errdefs.Validationf(
			"resource source: empty object is not valid")
	}
	if len(object) == 1 {
		if fileRaw, ok := object["file"]; ok {
			var path string
			if err := json.Unmarshal(fileRaw, &path); err != nil {
				return Source{}, errdefs.Validationf(
					"resource source: file reference must be a string: %v", err)
			}
			if path == "" {
				return Source{}, errdefs.Validationf(
					"resource source: file reference path is empty")
			}
			return Source{File: path}, nil
		}
		if embedRaw, ok := object["embed"]; ok {
			var name string
			if err := json.Unmarshal(embedRaw, &name); err != nil {
				return Source{}, errdefs.Validationf(
					"resource source: embed reference must be a string: %v", err)
			}
			if name == "" {
				return Source{}, errdefs.Validationf(
					"resource source: embed reference name is empty")
			}
			return Source{Embed: name}, nil
		}
	}
	return Source{Inline: append([]byte(nil), trimmed...)}, nil
}
