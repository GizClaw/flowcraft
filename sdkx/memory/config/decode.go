package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

// DecodeYAML reads a memory.yaml and returns the typed
// Document. The decoder uses KnownFields strictness so a
// typo at any level fails the build rather than silently
// dropping policy. The returned Document is the input the
// Builder consumes; callers should not mutate it.
func DecodeYAML(r io.Reader) (Document, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Document{}, fmt.Errorf("read memory yaml: %w", err)
	}
	decoder := yamlv3.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("decode memory yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents")
		}
		return Document{}, fmt.Errorf("decode memory yaml: %w", err)
	}
	return doc, nil
}

// DecodeYAMLFile reads the file at path and decodes it. It is
// a thin wrapper that surfaces a path-prefixed error on
// failure so build errors are easy to locate.
func DecodeYAMLFile(path string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read %s: %w", path, err)
	}
	return DecodeYAML(bytes.NewReader(data))
}

// parseDuration is a small wrapper that surfaces a typed
// error for invalid duration strings. The standard library
// does not, so test code that constructs Documents by hand
// benefits from a single source of truth.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}
