package fsx

import (
	"path"
	"strings"
)

// Pattern is a slash-separated glob evaluated against a root-relative
// path. A segment may be "*" (any single segment), "**" (zero or more
// segments), or a path.Match pattern. Leading slashes are ignored.
type Pattern string

// Match reports whether the slash-separated relative path rel matches p.
func (p Pattern) Match(rel string) bool {
	pattern := strings.Split(strings.Trim(string(p), "/"), "/")
	if len(pattern) == 1 && pattern[0] == "" {
		return rel == "" || rel == "."
	}
	return matchSegments(pattern, strings.Split(strings.Trim(rel, "/"), "/"))
}

func matchSegments(pattern, segments []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(segments); i++ {
				if matchSegments(pattern[1:], segments[i:]) {
					return true
				}
			}
			return false
		}
		if len(segments) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], segments[0])
		if err != nil || !ok {
			return false
		}
		pattern, segments = pattern[1:], segments[1:]
	}
	return len(segments) == 0
}

// Policy is the file-access boundary. An empty allow list means "no
// restriction in that direction"; DenyRead is consulted before Read.
type Policy struct {
	// Read lists the readable root-relative patterns. Empty means the
	// whole tree is readable unless DenyRead excludes it.
	Read []Pattern
	// Write lists the writable root-relative patterns. Empty means
	// read-only.
	Write []Pattern
	// DenyRead excludes patterns from Read. It wins over Read and over
	// an empty read list.
	DenyRead []Pattern
}

// HasFullRead reports whether the policy places no read restriction at
// all. It is the predicate a caller uses to decide whether the read path
// needs kernel enforcement: with full read access there is nothing to
// enforce.
func (p Policy) HasFullRead() bool {
	return len(p.Read) == 0 && len(p.DenyRead) == 0
}

// HasFullWrite reports whether the policy places no write restriction.
func (p Policy) HasFullWrite() bool {
	return len(p.Write) == 0
}

// ReadOnly reports whether the policy grants no write access at all.
func (p Policy) ReadOnly() bool { return len(p.Write) == 0 }

// CanRead reports whether rel, a root-relative slash path, is readable.
func (p Policy) CanRead(rel string) bool {
	for _, deny := range p.DenyRead {
		if deny.Match(rel) {
			return false
		}
	}
	if len(p.Read) == 0 {
		return true
	}
	for _, allow := range p.Read {
		if allow.Match(rel) {
			return true
		}
	}
	return false
}

// CanWrite reports whether rel, a root-relative slash path, is writable.
func (p Policy) CanWrite(rel string) bool {
	for _, allow := range p.Write {
		if allow.Match(rel) {
			return true
		}
	}
	return false
}
