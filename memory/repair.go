package memory

import (
	"context"
	"errors"

	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// RepairRequest names the scope whose projection lanes are rebuilt from
// canonical state, and the lanes to rebuild. An empty Lanes list rebuilds every
// configured lane whose rebuild needs no inference; the embedding lane is
// rebuilt only when it is named, because re-indexing it embeds every artifact
// again.
//
// A repair is a re-index, not a re-derive: canonical state -- the messages, the
// facts of the active generation, the chunks of the active document builds -- is
// indexed again the way the derivation worker indexed it, and nothing else about
// the scope moves. Watermarks, the published generation, and the derivation
// policy digest stay where they are, so the next pass resumes where it stopped
// instead of deriving the scope again.
type RepairRequest struct {
	Scope corememory.Scope `json:"scope"`
	// Lanes names the lanes to rebuild. An unknown name is a validation error
	// rather than a silent no-op.
	Lanes []string `json:"lanes,omitempty"`
}

// LaneRepair reports what one lane's rebuild published.
type LaneRepair struct {
	Name string `json:"name"`
	// Artifacts counts the canonical artifacts the rebuild indexed. It is the
	// same set for every lane in one repair, since the lanes project one scope.
	Artifacts int `json:"artifacts"`
	// StaleBefore is set when the state the lane served was written by another
	// producer version, which is the condition a rebuild converges.
	StaleBefore bool `json:"stale_before"`
	// SourceDigest and BuildDigest are the cumulative digests of the manifest
	// the rebuild published. Rebuilding twice publishes the same digests.
	SourceDigest string `json:"source_digest"`
	BuildDigest  string `json:"build_digest"`
}

// RepairResult reports what one repair rebuilt.
type RepairResult struct {
	Scope corememory.Scope `json:"scope"`
	// Artifacts counts the canonical artifacts the repair read for the lanes it
	// rebuilt.
	Artifacts int          `json:"artifacts"`
	Lanes     []LaneRepair `json:"lanes"`
}

// Repair rebuilds the named lanes of one scope from canonical state: it
// re-indexes the messages, the facts of the active generation, and the document
// chunks the scope holds -- through the same artifact shapes the derivation
// worker writes, and without calling a deriver, so a stale lexical lane is
// converged without a model call.
//
// It refuses while a derivation pass over the scope is running, and derivation
// refuses while it runs: a pass that indexed beside the rebuild would leave
// entries in a lane the rebuild is about to replace. Both refusals are
// retryable, and both mean nothing was rebuilt.
//
// The lanes are rebuilt in the order they are configured, and a failure reports
// the lanes already rebuilt: a repair that stopped halfway left the lanes it
// finished converged, and running it again is safe.
func (assembly *Assembly) Repair(ctx context.Context, request RepairRequest) (RepairResult, error) {
	if assembly == nil || assembly.repair == nil {
		return RepairResult{}, errors.New("memory assembly: lane repair is not configured")
	}
	if ctx == nil {
		return RepairResult{}, errors.New("memory assembly: context is required")
	}
	if err := request.Scope.Validate(); err != nil {
		return RepairResult{}, err
	}
	return assembly.repair(ctx, request)
}
