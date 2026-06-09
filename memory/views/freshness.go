package views

import (
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// SourceRevision captures the canonical source-side revision identity a view was
// derived from. SourceKey must be SourceRef.StableKey() or an equivalent opaque
// stable source identity; parseable SourceRef stable keys must match Kind.
// Revision and ContentHash are owned by the source and at least one is required
// for freshness checks. ObservedAt is metadata about when that source identity
// was observed and is not part of freshness equality.
type SourceRevision struct {
	Kind        SourceKind
	SourceKey   string
	Revision    string
	ContentHash string
	ObservedAt  time.Time
}

// UpstreamViewRef identifies a derived view output that this view depends on.
// It is lineage between views, not canonical evidence; canonical evidence stays
// in SourceRevision/SourceRef. OutputSignature is the upstream output identity
// token used for equality. RecordKey is optional and scopes the dependency to a
// single upstream record when the view exposes record-level identity.
type UpstreamViewRef struct {
	ViewID          ID
	OutputSignature string
	RecordKey       string
}

// ViewSignature captures the inputs and transform configuration that produced a
// view output. SourceRevisions are canonical evidence inputs. UpstreamViewRefs
// are derived view inputs. TransformSignature is the canonical overall
// output transform digest used for equality. DiagnosticSignatures is an optional
// component-level breakdown such as chunker, embedder, prompt, or config; it is
// only diagnostic and does not participate in stale equality. Callers that need
// component drift to affect output identity should fold it into
// TransformSignature.
//
// The zero value is valid as an unbound or unknown signature. It does not prove
// freshness; it only means this value carries no known output identity.
type ViewSignature struct {
	ViewID               ID
	SourceRevisions      []SourceRevision
	UpstreamViewRefs     []UpstreamViewRef
	TransformSignature   string
	DiagnosticSignatures map[string]string
}

// Validate checks that a source revision can participate in freshness checks.
func (r SourceRevision) Validate() error {
	if !validSourceKind(r.Kind) {
		return errdefs.Validationf("memory/views: unsupported source kind %q", r.Kind)
	}
	if r.SourceKey == "" {
		return errdefs.Validationf("memory/views: source revision key is required")
	}
	if err := r.validateSourceKeyKind(); err != nil {
		return err
	}
	if r.Revision == "" && r.ContentHash == "" {
		return errdefs.Validationf("memory/views: source revision or content hash is required")
	}
	return nil
}

// Validate checks that an upstream derived-view dependency is specific enough to
// participate in freshness checks.
func (r UpstreamViewRef) Validate() error {
	if r.ViewID == "" {
		return errdefs.Validationf("memory/views: upstream view id is required")
	}
	if r.OutputSignature == "" {
		return errdefs.Validationf("memory/views: upstream view output signature is required")
	}
	return nil
}

// Validate checks signature ownership, source revisions, upstream view refs, and
// duplicate input identities. It permits the zero value, which represents an
// unknown signature rather than proven freshness. Non-zero signatures require a
// ViewID so input identity is always attached to an owner view.
func (s ViewSignature) Validate() error {
	if !s.IsZero() && s.ViewID == "" {
		return errdefs.Validationf("memory/views: view signature view id is required")
	}

	seen := make(map[string]struct{}, len(s.SourceRevisions))
	for _, rev := range s.SourceRevisions {
		if err := rev.Validate(); err != nil {
			return err
		}
		key := rev.identityKey()
		if _, ok := seen[key]; ok {
			return errdefs.Validationf("memory/views: duplicate source revision %q", key)
		}
		seen[key] = struct{}{}
	}
	seenUpstreams := make(map[string]struct{}, len(s.UpstreamViewRefs))
	for _, ref := range s.UpstreamViewRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
		key := ref.identityKey()
		if _, ok := seenUpstreams[key]; ok {
			return errdefs.Validationf("memory/views: duplicate upstream view ref %q", key)
		}
		seenUpstreams[key] = struct{}{}
	}
	return nil
}

// HasInputIdentity reports whether the signature names canonical source
// revisions or upstream view outputs. TransformSignature alone describes the
// output transform, not the complete global input graph.
func (s ViewSignature) HasInputIdentity() bool {
	return len(s.SourceRevisions) > 0 ||
		len(s.UpstreamViewRefs) > 0
}

// IsStaleAgainst reports whether the receiver differs from the desired view id,
// canonical transform digest, source revisions set, or upstream view refs set.
// When both signatures are zero it reports no difference, not proven freshness;
// use IsKnown when callers need to distinguish unknown signatures.
func (s ViewSignature) IsStaleAgainst(want ViewSignature) bool {
	return s.StaleCountAgainst(want) > 0
}

// StaleCountAgainst returns the number of changed signature dimensions between
// the receiver and the desired signature: view id, canonical transform digest,
// source revisions set, and upstream view refs set. It is not a count of stale
// input records. DiagnosticSignatures is a diagnostic component breakdown and is
// intentionally ignored; fold component drift into TransformSignature when it
// should affect freshness.
func (s ViewSignature) StaleCountAgainst(want ViewSignature) int {
	stale := 0
	if s.ViewID != want.ViewID {
		stale++
	}
	if s.TransformSignature != want.TransformSignature {
		stale++
	}
	if !slices.EqualFunc(sortedSourceRevisions(s.SourceRevisions), sortedSourceRevisions(want.SourceRevisions), sourceRevisionEqual) {
		stale++
	}
	if !slices.EqualFunc(sortedUpstreamViewRefs(s.UpstreamViewRefs), sortedUpstreamViewRefs(want.UpstreamViewRefs), upstreamViewRefEqual) {
		stale++
	}
	return stale
}

// IsZero reports whether the signature is unknown or unbound. A zero-vs-zero
// freshness comparison means "no difference observed"; it does not prove fresh.
func (s ViewSignature) IsZero() bool {
	return s.ViewID == "" &&
		len(s.SourceRevisions) == 0 &&
		len(s.UpstreamViewRefs) == 0 &&
		s.TransformSignature == "" &&
		len(s.DiagnosticSignatures) == 0
}

// IsKnown reports whether the signature carries any local output identity
// metadata. It does not imply a globally complete freshness proof.
func (s ViewSignature) IsKnown() bool {
	return !s.IsZero()
}

func cloneViewSignature(in ViewSignature) ViewSignature {
	out := in
	if in.SourceRevisions != nil {
		out.SourceRevisions = append([]SourceRevision(nil), in.SourceRevisions...)
		slices.SortFunc(out.SourceRevisions, compareSourceRevision)
	}
	if in.UpstreamViewRefs != nil {
		out.UpstreamViewRefs = append([]UpstreamViewRef(nil), in.UpstreamViewRefs...)
		slices.SortFunc(out.UpstreamViewRefs, compareUpstreamViewRef)
	}
	if in.DiagnosticSignatures != nil {
		out.DiagnosticSignatures = maps.Clone(in.DiagnosticSignatures)
	}
	return out
}

func sortedSourceRevisions(in []SourceRevision) []SourceRevision {
	out := append([]SourceRevision(nil), in...)
	slices.SortFunc(out, compareSourceRevision)
	return out
}

func sortedUpstreamViewRefs(in []UpstreamViewRef) []UpstreamViewRef {
	out := append([]UpstreamViewRef(nil), in...)
	slices.SortFunc(out, compareUpstreamViewRef)
	return out
}

func sourceRevisionEqual(a, b SourceRevision) bool {
	return a.Kind == b.Kind &&
		a.SourceKey == b.SourceKey &&
		a.Revision == b.Revision &&
		a.ContentHash == b.ContentHash
}

func upstreamViewRefEqual(a, b UpstreamViewRef) bool {
	return a.ViewID == b.ViewID &&
		a.OutputSignature == b.OutputSignature &&
		a.RecordKey == b.RecordKey
}

func compareSourceRevision(a, b SourceRevision) int {
	if a.Kind != b.Kind {
		return compareString(string(a.Kind), string(b.Kind))
	}
	if a.SourceKey != b.SourceKey {
		return compareString(a.SourceKey, b.SourceKey)
	}
	if a.Revision != b.Revision {
		return compareString(a.Revision, b.Revision)
	}
	if a.ContentHash != b.ContentHash {
		return compareString(a.ContentHash, b.ContentHash)
	}
	return 0
}

func compareUpstreamViewRef(a, b UpstreamViewRef) int {
	if a.ViewID != b.ViewID {
		return compareString(string(a.ViewID), string(b.ViewID))
	}
	if a.RecordKey != b.RecordKey {
		return compareString(a.RecordKey, b.RecordKey)
	}
	return compareString(a.OutputSignature, b.OutputSignature)
}

func (r SourceRevision) identityKey() string {
	return string(r.Kind) + "\x00" + r.SourceKey
}

func (r UpstreamViewRef) identityKey() string {
	// A downstream output may depend on at most one output identity for the same
	// upstream record; OutputSignature is intentionally excluded here.
	return string(r.ViewID) + "\x00" + r.RecordKey
}

func (r SourceRevision) validateSourceKeyKind() error {
	var key sourceRefStableKey
	if err := json.Unmarshal([]byte(r.SourceKey), &key); err != nil {
		return nil
	}
	if key.Schema != sourceRefStableKeySchema {
		return nil
	}
	if key.Kind != r.Kind {
		return errdefs.Validationf("memory/views: source revision kind %q does not match stable source key kind %q", r.Kind, key.Kind)
	}
	return nil
}
