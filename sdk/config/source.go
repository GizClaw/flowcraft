package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// SourceKind classifies the form of a [Source] or [Ref].
type SourceKind uint8

const (
	// SourceLiteral is a plain string: the content itself, in the
	// module's author format.
	SourceLiteral SourceKind = iota
	// SourceContent is a structured JSON/YAML subtree: the document
	// nested directly in the parent document.
	SourceContent
	// SourceFile references an external file, resolved against the
	// Loader's base directory.
	SourceFile
	// SourceEmbed references a build-time embedded asset from the
	// Loader's embed registry.
	SourceEmbed
)

func (k SourceKind) String() string {
	switch k {
	case SourceLiteral:
		return "literal"
	case SourceContent:
		return "content"
	case SourceFile:
		return "file"
	case SourceEmbed:
		return "embed"
	default:
		return "unknown"
	}
}

// Source is the "content or reference" build-time document type. Only
// Source-typed fields are ever dereferenced by a [Loader]: a plain
// string field is always literal, while a Source field is either
// literal content, an external file reference, or an embedded-asset
// reference. Whether reference semantics apply is decided by the field
// type, never by string content — there are no magic strings.
//
// The wire form is either a string, a structured object, or a
// reference object:
//
//	settings: "version: v1"                 # string literal
//	settings:                                # structured content
//	  version: v1
//	  workspaces:
//	    project: {driver: local, settings: {root: .}}
//	settings: {file: ./workspace.yaml}      # external file
//	settings: {embed: scenarios/.../x.yaml} # embedded asset
//
// An object whose keys are exactly "file" and/or "embed" (one of them
// non-empty) is a reference; any other object — including one whose
// module schema happens to have a top level "embed" or "file" field —
// is the document itself, nested inline. An empty {} reference form or
// {"file": ""} fails at decode time.
type Source struct {
	kind    SourceKind
	literal string          // SourceLiteral
	raw     json.RawMessage // SourceContent
	path    string          // SourceFile / SourceEmbed
}

// LiteralSource builds a Source from plain document content.
func LiteralSource(literal string) Source {
	return Source{kind: SourceLiteral, literal: literal}
}

// ContentSource builds a Source from a structured JSON/YAML subtree
// carried inline in the parent document.
func ContentSource(raw json.RawMessage) Source {
	return Source{kind: SourceContent, raw: raw}
}

// FileSource builds a Source that references an external file, resolved
// against the Loader's base directory.
func FileSource(path string) Source {
	return Source{kind: SourceFile, path: path}
}

// EmbedSource builds a Source that references a build-time embedded
// asset from the Loader's embed registry.
func EmbedSource(path string) Source {
	return Source{kind: SourceEmbed, path: path}
}

// Kind returns the source form.
func (s Source) Kind() SourceKind { return s.kind }

// IsLiteral reports whether the source is literal content.
func (s Source) IsLiteral() bool { return s.kind == SourceLiteral }

// IsRef reports whether the source references a file or an embedded
// asset rather than carrying content itself.
func (s Source) IsRef() bool { return s.kind != SourceLiteral }

// Literal returns the literal content and whether the source is a
// literal. The bool is false for file and embed references.
func (s Source) Literal() (string, bool) {
	if s.kind != SourceLiteral {
		return "", false
	}
	return s.literal, true
}

// Content returns the nested document subtree and whether the source
// is structured content. The bool is false for literal strings and
// references.
func (s Source) Content() (json.RawMessage, bool) {
	if s.kind != SourceContent {
		return nil, false
	}
	return s.raw, true
}

// File returns the referenced file path and whether the source is a
// file reference.
func (s Source) File() (string, bool) {
	if s.kind != SourceFile {
		return "", false
	}
	return s.path, true
}

// Embed returns the embedded-asset name and whether the source is an
// embed reference.
func (s Source) Embed() (string, bool) {
	if s.kind != SourceEmbed {
		return "", false
	}
	return s.path, true
}

// sourceWire is the strict-decoded object form of a Source.
type sourceWire struct {
	File  string `json:"file,omitempty"`
	Embed string `json:"embed,omitempty"`
}

// UnmarshalJSON accepts a plain string (literal) or a strict object
// with exactly one of file / embed. Structural mistakes — unknown keys,
// both forms, or neither form — fail here at parse time.
func (s *Source) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("config source: empty value")
	}
	switch trimmed[0] {
	case '"':
		var literal string
		if err := json.Unmarshal(trimmed, &literal); err != nil {
			return err
		}
		*s = Source{kind: SourceLiteral, literal: literal}
		return nil
	case '{':
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &keys); err != nil {
			return err
		}
		allReferenceKeys := true
		for key := range keys {
			if key != "file" && key != "embed" {
				allReferenceKeys = false
				break
			}
		}
		if !allReferenceKeys {
			// Any other key means the object is the document itself —
			// a module document whose schema happens to include a top
			// level "embed" (or "file") field stays content.
			raw := make(json.RawMessage, len(trimmed))
			copy(raw, trimmed)
			*s = Source{kind: SourceContent, raw: raw}
			return nil
		}
		var wire sourceWire
		if err := json.Unmarshal(trimmed, &wire); err != nil {
			return err
		}
		switch {
		case wire.File != "" && wire.Embed != "":
			return errdefs.Validationf(
				"config source: file and embed are mutually exclusive")
		case wire.File != "":
			*s = Source{kind: SourceFile, path: wire.File}
			return nil
		case wire.Embed != "":
			*s = Source{kind: SourceEmbed, path: wire.Embed}
			return nil
		default:
			return errdefs.Validationf(
				"config source: file/embed requires a non-empty value")
		}
	default:
		return errdefs.Validationf(
			"config source: must be a string or an object with file/embed")
	}
}

// MarshalJSON serializes the source back to its wire form.
func (s Source) MarshalJSON() ([]byte, error) {
	switch s.kind {
	case SourceLiteral:
		return json.Marshal(s.literal)
	case SourceContent:
		return s.raw, nil
	case SourceFile:
		return json.Marshal(sourceWire{File: s.path})
	case SourceEmbed:
		return json.Marshal(sourceWire{Embed: s.path})
	default:
		return nil, fmt.Errorf("config source: cannot marshal empty source")
	}
}

// Ref is the "location, not content" reference type used by fields
// whose inline form is the enclosing structure itself (for example an
// agent recipe). It has no literal member: the wire form must be an
// object with exactly one of "file" / "embed", and a string is a
// decode error rather than silently becoming content.
type Ref struct {
	kind SourceKind // SourceFile or SourceEmbed
	path string
}

// Kind returns the reference form (file or embed).
func (r Ref) Kind() SourceKind { return r.kind }

// File returns the referenced file path and whether the ref is a file
// reference.
func (r Ref) File() (string, bool) {
	if r.kind != SourceFile {
		return "", false
	}
	return r.path, true
}

// Embed returns the embedded-asset name and whether the ref is an
// embed reference.
func (r Ref) Embed() (string, bool) {
	if r.kind != SourceEmbed {
		return "", false
	}
	return r.path, true
}

// UnmarshalJSON accepts only the object form {"file": ...} / {"embed":
// ...}; a string is rejected so a mistyped ref never silently becomes
// literal content.
func (r *Ref) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("config ref: empty value")
	}
	if trimmed[0] == '"' {
		return errdefs.Validationf(
			"config ref: must be an object with file or embed, got a string")
	}
	var wire sourceWire
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	switch {
	case wire.File != "" && wire.Embed != "":
		return errdefs.Validationf(
			"config ref: file and embed are mutually exclusive")
	case wire.File != "":
		*r = Ref{kind: SourceFile, path: wire.File}
		return nil
	case wire.Embed != "":
		*r = Ref{kind: SourceEmbed, path: wire.Embed}
		return nil
	default:
		return errdefs.Validationf(
			"config ref: exactly one of file or embed is required")
	}
}

// MarshalJSON serializes the ref back to its wire form.
func (r Ref) MarshalJSON() ([]byte, error) {
	switch r.kind {
	case SourceFile:
		return json.Marshal(sourceWire{File: r.path})
	case SourceEmbed:
		return json.Marshal(sourceWire{Embed: r.path})
	default:
		return nil, fmt.Errorf("config ref: cannot marshal empty ref")
	}
}
