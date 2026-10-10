package storage

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// segmentEncoding is case-insensitive-safe on purpose: workspace backends
// run on case-insensitive filesystems (macOS, Windows), and two base64
// segments that differ only by letter case would collide there. Base32's
// alphabet is uppercase letters plus digits, so distinct inputs always
// produce case-distinct paths.
var segmentEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

const (
	// encodedPrefix marks name segments that are already canonical
	// storage.EncodeSegment output (base32, case-insensitive safe).
	encodedPrefix = "k_"
	// literalPrefix marks path segments that encode a name segment which
	// is not already canonical. The two prefixes keep the mapping from
	// name segments to path segments injective: a canonical segment is
	// passed through unchanged, every other segment is base32-encoded.
	// Passing canonical segments through keeps the total expansion at one
	// base32 layer instead of two, which otherwise pushed long caller IDs
	// (conversation/user identifiers) past the 255-byte filesystem limit.
	literalPrefix = "e_"
	// maxSegmentBytes is the portable per-segment path limit shared by
	// ext4, APFS, and NTFS. Encoding a name segment that would exceed it
	// fails with a clear error instead of a platform ENAMETOOLONG.
	maxSegmentBytes = 255
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
	return encodedPrefix + segmentEncoding.EncodeToString([]byte(value))
}

// decodeSegment reverses encodeSegment and verifies canonical form.
func decodeSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, encodedPrefix) {
		return "", errors.New("storage: non-canonical path segment")
	}
	raw, err := segmentEncoding.DecodeString(strings.TrimPrefix(segment, encodedPrefix))
	if err != nil {
		return "", errors.New("storage: decode path segment")
	}
	value := string(raw)
	if encodeSegment(value) != segment {
		return "", errors.New("storage: non-canonical path segment")
	}
	return value, nil
}

// isCanonicalSegment reports whether segment starts with the encodeSegment
// output of some non-empty value, optionally followed by a literal suffix
// such as ".json". Base32's alphabet never contains punctuation, so the
// encoded core is exactly the leading run of alphabet bytes.
func isCanonicalSegment(segment string) bool {
	if !strings.HasPrefix(segment, encodedPrefix) {
		return false
	}
	rest := segment[len(encodedPrefix):]
	end := 0
	for end < len(rest) && base32AlphabetByte(rest[end]) {
		end++
	}
	if end == 0 {
		return false
	}
	core := rest[:end]
	raw, err := segmentEncoding.DecodeString(core)
	if err != nil || len(raw) == 0 {
		return false
	}
	return segmentEncoding.EncodeToString(raw) == core
}

func base32AlphabetByte(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= '2' && value <= '7')
}

// encodePathSegment maps one name segment onto exactly one path segment.
func encodePathSegment(segment string) string {
	if isCanonicalSegment(segment) {
		return segment
	}
	return literalPrefix + segmentEncoding.EncodeToString([]byte(segment))
}

// decodePathSegment reverses encodePathSegment and verifies canonical form.
func decodePathSegment(segment string) (string, error) {
	if isCanonicalSegment(segment) {
		return segment, nil
	}
	if !strings.HasPrefix(segment, literalPrefix) {
		return "", errors.New("storage: non-canonical path segment")
	}
	raw, err := segmentEncoding.DecodeString(strings.TrimPrefix(segment, literalPrefix))
	if err != nil {
		return "", errors.New("storage: decode path segment")
	}
	value := string(raw)
	if value == "" || encodePathSegment(value) != segment {
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
		encoded[index] = encodePathSegment(segment)
		if len(encoded[index]) > maxSegmentBytes {
			return "", fmt.Errorf(
				"storage: encoded name segment is %d bytes, the portable path limit is %d",
				len(encoded[index]), maxSegmentBytes)
		}
	}
	return strings.Join(append([]string{root}, encoded...), "/"), nil
}

// pathToName decodes a path under root back to its canonical name.
func pathToName(root, path string) (string, error) {
	// Workspace Walk yields slash-separated paths on every platform, but a
	// workspace adapter built against an older core (before Walk normalized
	// separators) may still hand back backslashes on Windows. Normalize
	// defensively: storage names are a slash-separated namespace, so a
	// backslash can only ever be a host separator here.
	path = normalizeWalkPath(path)
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
		value, err := decodePathSegment(segment)
		if err != nil {
			return "", err
		}
		decoded[index] = value
	}
	return strings.Join(decoded, "/"), nil
}

// normalizeWalkPath converts host separators returned by a workspace Walk
// into the slash-separated workspace namespace.
func normalizeWalkPath(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}
	return strings.ReplaceAll(value, "\\", "/")
}
