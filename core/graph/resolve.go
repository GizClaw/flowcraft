package graph

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ContainsRef reports whether v — a decoded JSON value or a plain
// string — contains at least one live "${board:*}" reference.
//
// Deprecated: use [agent.ContainsBoardRef].
func ContainsRef(v any) bool {
	return agent.ContainsBoardRef(v)
}

// ExtractRefs returns the sorted, deduplicated board paths referenced
// anywhere inside v.
//
// Deprecated: use [agent.ExtractBoardRefs].
func ExtractRefs(v any) []string {
	return agent.ExtractBoardRefs(v)
}

// resolveConfig resolves board references inside a raw JSON node
// config, returning the rewritten raw JSON for the typed decode.
// Configs without the reference marker are returned unchanged.
//
// References are resolved per invocation, immediately before the typed
// config decode: a node can consume values written by an upstream node
// in the same run, and nothing is baked into shared registration
// state. Missing references fail the node with a validation error
// unless the reference carries a default.
func resolveConfig(raw json.RawMessage, board *agent.Board) (json.RawMessage, error) {
	if len(raw) == 0 || !bytes.Contains(raw, []byte(agent.BoardRefMarker)) {
		return raw, nil
	}
	// Numbers stay json.Number through the round-trip so unrelated
	// literals (e.g. 64-bit or arbitrarily large integers) are re-encoded
	// verbatim instead of losing precision via float64.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errdefs.Validationf("node config is not valid JSON: %v", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		if err == nil {
			return nil, errdefs.Validationf("node config has trailing data after the JSON value")
		}
		return nil, errdefs.Validationf("node config is not valid JSON: %v", err)
	}
	resolved, err := board.Resolve(v)
	if err != nil {
		return nil, errdefs.Validationf("node config: %v", err)
	}
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, errdefs.Internalf("failed to re-encode resolved node config: %v", err)
	}
	return out, nil
}
