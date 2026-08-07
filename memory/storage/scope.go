package storage

import (
	"errors"
	"strings"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// EncodeSegment returns the canonical path-safe form of one name segment.
// Segment encoding is a storage-level convention: every backend must resolve
// the same name to the same layout, and no user input ever reaches a
// filesystem path verbatim.
func EncodeSegment(value string) string { return encodeSegment(value) }

// DecodeSegment reverses EncodeSegment.
func DecodeSegment(segment string) (string, error) { return decodeSegment(segment) }

// ScopePartition returns the canonical, path-safe partition name for a scope.
func ScopePartition(scope sdkmemory.Scope) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	return encodeSegment(scope.RuntimeID) + "/" +
		encodeSegment(scope.UserID) + "/" +
		encodeSegment(scope.AgentID), nil
}

// StreamName returns the canonical stream name for one conversation in a
// scope: the scope partition plus the encoded conversation ID.
func StreamName(scope sdkmemory.Scope, conversationID string) (string, error) {
	partition, err := ScopePartition(scope)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(conversationID) == "" {
		return "", errors.New("storage: conversation id is required")
	}
	if strings.ContainsRune(conversationID, '\x00') {
		return "", errors.New("storage: conversation id must not contain NUL")
	}
	return partition + "/" + encodeSegment(conversationID), nil
}

// ScopeFromPartition reverses ScopePartition.
func ScopeFromPartition(partition string) (sdkmemory.Scope, error) {
	segments := strings.Split(partition, "/")
	if len(segments) != 3 {
		return sdkmemory.Scope{}, errors.New("storage: invalid scope partition")
	}
	var scope sdkmemory.Scope
	for index, destination := range []*string{&scope.RuntimeID, &scope.UserID, &scope.AgentID} {
		value, err := decodeSegment(segments[index])
		if err != nil {
			return sdkmemory.Scope{}, err
		}
		*destination = value
	}
	if err := scope.Validate(); err != nil {
		return sdkmemory.Scope{}, err
	}
	return scope, nil
}
