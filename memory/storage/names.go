package storage

import (
	"encoding/base64"
	"errors"
	"strings"
)

// Names are deterministic, path-like strings: non-empty segments separated
// by "/", with no ".", "..", empty, or NUL-containing segments. Backends
// translate names to their own layout; the workspace adapter encodes every
// segment so no user input ever reaches a filesystem path verbatim.
func validateName(name string) error {
	if name == "" {
		return errors.New("storage: name is required")
	}
	if strings.ContainsRune(name, '\x00') {
		return errors.New("storage: name must not contain NUL")
	}
	for segment := range strings.SplitSeq(name, "/") {
		if segment == "" {
			return errors.New("storage: name has an empty segment")
		}
		if segment == "." || segment == ".." {
			return errors.New("storage: name must not contain dot segments")
		}
	}
	return nil
}

// prefixBase returns the longest proper prefix that is a directory name, so
// a prefix scan can start from a single subtree and filter by exact string
// prefix. "" is returned for a top-level prefix.
func prefixBase(prefix string) string {
	if index := strings.LastIndex(prefix, "/"); index >= 0 {
		return prefix[:index]
	}
	return ""
}

// nameHasPrefix reports whether name is inside the prefix area. Matching is
// segment-boundary: name == prefix or name starts with prefix + "/", so a
// partition prefix never leaks into a sibling partition (for example
// "rt/u/a" does not match "rt/u/ab/...").
func nameHasPrefix(name, prefix string) bool {
	if prefix == "" {
		return true
	}
	return name == prefix || strings.HasPrefix(name, prefix+"/")
}

// encodeSegment makes one arbitrary string a safe workspace path segment.
func encodeSegment(value string) string {
	return "k_" + base64.RawURLEncoding.EncodeToString([]byte(value))
}

// decodeSegment reverses encodeSegment and verifies canonical form.
func decodeSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, "k_") {
		return "", errors.New("storage: non-canonical path segment")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, "k_"))
	if err != nil {
		return "", errors.New("storage: decode path segment")
	}
	value := string(raw)
	if encodeSegment(value) != segment {
		return "", errors.New("storage: non-canonical path segment")
	}
	return value, nil
}

// nameToPath encodes a name into path segments under root.
func nameToPath(root, name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	segments := strings.Split(name, "/")
	encoded := make([]string, len(segments))
	for index, segment := range segments {
		encoded[index] = encodeSegment(segment)
	}
	return strings.Join(append([]string{root}, encoded...), "/"), nil
}

// pathToName decodes a path under root back to its canonical name.
func pathToName(root, path string) (string, error) {
	prefix := root + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", errors.New("storage: path outside root")
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return "", errors.New("storage: path is the root")
	}
	segments := strings.Split(rest, "/")
	decoded := make([]string, len(segments))
	for index, segment := range segments {
		value, err := decodeSegment(segment)
		if err != nil {
			return "", err
		}
		decoded[index] = value
	}
	return strings.Join(decoded, "/"), nil
}
