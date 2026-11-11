// Package component defines the narrow capabilities used by memory pipelines.
package component

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// ArtifactKind identifies a typed value moving through write-side derivation.
type ArtifactKind string

// GenerationMetadataKey names the derivation generation on a write-side
// artifact. The generation is the identity of the derivation policy that owns a
// pass, and it travels with the derived values so a deriver reads and writes
// the generation it is building instead of whichever one readers currently
// resolve.
const GenerationMetadataKey = "generation"

// Artifact is an immutable-by-convention value produced by a Deriver.
// ID must be stable for the same logical value.
type Artifact struct {
	Kind     ArtifactKind           `json:"kind"`
	ID       string                 `json:"id"`
	Content  coremessage.Content    `json:"content"`
	Entities []string               `json:"entities,omitempty"`
	Sources  []corememory.SourceRef `json:"sources"`
	Metadata corememory.Metadata    `json:"metadata,omitempty"`
}

// Validate checks the portable artifact contract, including provenance.
func (artifact Artifact) Validate() error {
	if strings.TrimSpace(string(artifact.Kind)) == "" {
		return errors.New("memory component: artifact kind is required")
	}
	if strings.TrimSpace(artifact.ID) == "" {
		return errors.New("memory component: artifact id is required")
	}
	if err := artifact.Content.Validate(); err != nil {
		return fmt.Errorf("memory component: artifact content: %w", err)
	}
	if len(artifact.Sources) == 0 {
		return errors.New("memory component: artifact provenance is required")
	}
	for index, source := range artifact.Sources {
		if err := source.Validate(); err != nil {
			return fmt.Errorf("memory component: artifact source %d: %w", index, err)
		}
	}
	return nil
}

// Clone returns an independently owned artifact.
func (artifact Artifact) Clone() Artifact {
	artifact.Content = artifact.Content.Clone()
	artifact.Entities = append([]string(nil), artifact.Entities...)
	artifact.Sources = append([]corememory.SourceRef(nil), artifact.Sources...)
	artifact.Metadata = artifact.Metadata.Clone()
	return artifact
}

// CloneArtifacts returns independently owned artifacts in the same order.
func CloneArtifacts(artifacts []Artifact) []Artifact {
	if artifacts == nil {
		return nil
	}
	cloned := make([]Artifact, len(artifacts))
	for index, artifact := range artifacts {
		cloned[index] = artifact.Clone()
	}
	return cloned
}

// Deriver transforms one typed artifact into zero or more typed artifacts.
type Deriver interface {
	Derive(context.Context, Artifact) ([]Artifact, error)
}

// ProjectionRequest replaces one rebuildable projection from typed artifacts.
//
// Artifacts is read-only: a rebuild must not mutate the entries or keep the
// slice past the call, because one repair hands the same set to every lane.
type ProjectionRequest struct {
	Scope      corememory.Scope
	Projection string
	Artifacts  []Artifact
	Metadata   corememory.Metadata
}

// Indexer writes a projection that can be rebuilt from canonical artifacts.
type Indexer interface {
	Rebuild(context.Context, ProjectionRequest) error
}

// Auditor recomputes a projection's digests independently of stored state so
// lifecycle repair can detect drift without rebuilding.
type Auditor interface {
	AuditDigests(context.Context, corememory.Scope) (
		storedSource, computedSource, storedBuild, computedBuild string, found bool, err error,
	)
}

// ProducerIdentity is the read-only identity of the state one projection lane
// persisted: the version of the producer that wrote it together with the
// cumulative digests its manifest carries.
type ProducerIdentity struct {
	// Version names the producer that wrote the state. An empty version means
	// the state was written before lanes stamped one, which is itself a
	// version: the producer that wrote it is unknown, not current.
	Version string
	// Mixed reports that the scope's shards were written by more than one
	// producer. There is then no single version to report, so Version stays
	// empty and every "is this the current producer" comparison fails: the
	// state a read serves is a blend, and a rebuild is what converges it.
	Mixed bool
	// SourceDigest and BuildDigest are the cumulative digests of the active
	// state: the root over the shards' chains of indexed inputs and of builds.
	SourceDigest string
	BuildDigest  string
	// Shards counts the shards that hold state. It is zero when found is
	// false and grows with the scope's conversations and datasets.
	Shards int
}

// Producer names the state one projection lane produces and reports the
// identity its persisted state carries. A lane whose stored state was written
// by another version keeps serving it -- the entries an earlier producer wrote
// stay where they are until the lane is rebuilt -- so a mismatch is what tells
// a host that a rebuild would change what the lane answers with.
type Producer interface {
	// ProducerVersion names the identity of the state this lane writes: its
	// algorithm plus the parameters that shape a persisted record.
	ProducerVersion() string
	// StoredProducerIdentity reports the identity of the lane's persisted
	// state for one scope. found is false when the lane holds no state yet,
	// which is not a mismatch: there is nothing an earlier producer wrote.
	StoredProducerIdentity(context.Context, corememory.Scope) (ProducerIdentity, bool, error)
	// OtherStoredNames reports the storage names this lane's family holds
	// state under for one scope, excluding the name this lane writes. A lane
	// that derives its storage name from the producer identity forks one name
	// per generation, so the state an earlier producer wrote survives under a
	// name the running lane no longer reads: no manifest of the current name
	// can report it, and this is what tells a host it is there. The list is
	// empty when nothing else holds state, which is also the state of a scope
	// that was never indexed.
	OtherStoredNames(context.Context, corememory.Scope) ([]string, error)
}

// ReclaimResult reports what one lane's reclaim removed: the immutable bases
// and segments the lane published before and its active manifest stopped
// pointing at. It counts objects rather than bytes because a sweep deletes by
// key: measuring what it removes would read the storage the sweep exists to
// give up, and a host that wants the size stats the workspace it owns.
type ReclaimResult struct {
	// Bases counts the full snapshots removed.
	Bases int `json:"bases"`
	// Segments counts the incremental deltas removed.
	Segments int `json:"segments"`
}

// Empty reports whether the sweep found nothing to remove.
func (result ReclaimResult) Empty() bool { return result.Bases == 0 && result.Segments == 0 }

// Reclaimer reclaims the storage a projection lane no longer serves.
//
// A lane publishes immutable bases and segments and moves one manifest at
// them, so every publication that rewrites the shape -- a compaction, a
// rebuild -- leaves the state it replaced unreachable but stored. Nothing
// reads it: the manifest is the only pointer, and it names what is current.
// Reclaiming is unreachability, not erasure of authority: a lane reads its
// manifest, and a rebuild writes again what a sweep removed.
type Reclaimer interface {
	// ReclaimSuperseded removes the objects of one scope this lane's active
	// manifest does not reference. It is idempotent -- a retried sweep finds
	// less to remove -- and it reports what it removed.
	//
	// It expects the lane's single writer to be held off, which is the same
	// discipline every publication to the lane already needs: a manifest this
	// sweep read while another writer replaced it would name the objects that
	// writer is about to publish.
	ReclaimSuperseded(context.Context, corememory.Scope) (ReclaimResult, error)
}

// LegacyLayout names one projection layout version a scope still holds state
// under, together with the number of keys a sweep removed from it.
type LegacyLayout struct {
	Version string `json:"version"`
	Keys    int    `json:"keys"`
}

// LegacyReclaimResult reports what one lane's legacy-layout sweep removed.
//
// It counts keys rather than bases and segments because a layout this build
// does not read is not one whose object types it knows: the sweep deletes a
// subtree, not the artifacts of a protocol it can parse.
type LegacyReclaimResult struct {
	// Layouts lists the versions the sweep found state under, in the order it
	// swept them, with the keys it removed from each.
	Layouts []LegacyLayout `json:"layouts,omitempty"`
	// Keys counts the keys removed across versions.
	Keys int `json:"keys"`
}

// Empty reports whether the sweep found nothing to remove.
func (result LegacyReclaimResult) Empty() bool { return result.Keys == 0 }

// LegacyLayoutReclaimer reclaims the state a lane's family holds under a
// projection layout this build does not read.
//
// A layout is a segment of every key the family writes, so a build that bumps
// it leaves the tree of the version it replaced behind: the lanes of this build
// read the one layout they write, and every sweep of a lane stops at its own
// root inside that layout. This is the sweep that reaches the others, and it is
// the only one that deletes state no build in this tree can read.
//
// It is separate from Reclaimer because the two answer different questions.
// ReclaimSuperseded keeps what a manifest names, so it deletes only what a
// rebuild could publish again under the same layout; a lane that skipped a
// legacy version would leave a tree that a rollback to an earlier build still
// serves. A host calls this one when it has decided no rollback is coming.
type LegacyLayoutReclaimer interface {
	// ReclaimLegacyLayouts removes one scope's state under every layout
	// version this build does not read, and reports what it removed. It is
	// idempotent -- a retried sweep finds less to remove -- and it touches no
	// other scope, because the partition is the same in every version.
	ReclaimLegacyLayouts(context.Context, corememory.Scope) (LegacyReclaimResult, error)
}

// DocumentAddress qualifies a document within one hard scope.
type DocumentAddress struct {
	DatasetID  string `json:"dataset_id"`
	DocumentID string `json:"document_id"`
}

func (address DocumentAddress) Validate() error {
	if strings.TrimSpace(address.DatasetID) == "" || strings.TrimSpace(address.DocumentID) == "" {
		return errors.New("memory component: document address dataset_id and document_id are required")
	}
	if strings.ContainsRune(address.DatasetID, '\x00') || strings.ContainsRune(address.DocumentID, '\x00') {
		return errors.New("memory component: document address fields must not contain NUL")
	}
	return nil
}

func (address DocumentAddress) Key() string { return address.DatasetID + "\x00" + address.DocumentID }

// ProjectionDelta mutates only changed projection records. DeleteDocuments
// removes every active item belonging to each qualified tombstoned document.
//
// Shard names the one shard the delta writes to. A delta is always addressed,
// never mixed: every upsert it carries belongs to that address family and the
// ids it deletes are ids of that shard, which is what lets the store route a
// delete without an id-to-shard catalog. The caller knows the address -- it is
// processing one conversation's commit or one document event -- so saying it
// is cheaper and stricter than making the store infer it from the upserts,
// and a delete-only delta (a retired generation) has no upsert to infer from.
type ProjectionDelta struct {
	Scope corememory.Scope
	// Shard is required for every delta, including one that only deletes.
	Shard              string
	Projection         string
	Upserts            []Artifact
	DeleteIDs          []string
	DeleteDocuments    []DocumentAddress
	ReconcileDocuments []DocumentAddress
	ActiveIDs          []string
	SourceRevision     string
	SourceDigest       string
}

// ValidateShard checks that the delta names one shard and that everything it
// carries belongs to it. A delta spanning two addresses is refused: its
// deletes would have to reach a shard the delta does not name.
func (delta ProjectionDelta) ValidateShard() error {
	if strings.TrimSpace(delta.Shard) == "" {
		return errors.New("memory component: projection delta shard is required")
	}
	for _, artifact := range delta.Upserts {
		shard, err := ShardForAddress(AddressFromArtifact(artifact))
		if err != nil {
			return fmt.Errorf("memory component: projection delta artifact %q: %w", artifact.ID, err)
		}
		if shard != delta.Shard {
			return fmt.Errorf("memory component: projection delta writes artifact %q to shard %q, not %q",
				artifact.ID, shard, delta.Shard)
		}
	}
	for _, addresses := range [][]DocumentAddress{delta.DeleteDocuments, delta.ReconcileDocuments} {
		for _, address := range addresses {
			shard, err := DatasetShard(address.DatasetID)
			if err != nil {
				return fmt.Errorf("memory component: projection delta document %q: %w", address.DocumentID, err)
			}
			if shard != delta.Shard {
				return fmt.Errorf("memory component: projection delta writes document %q of %q to shard %q, not %q",
					address.DocumentID, address.DatasetID, shard, delta.Shard)
			}
		}
	}
	return nil
}

func (delta ProjectionDelta) ValidateDocumentAddresses() error {
	for _, values := range [][]DocumentAddress{delta.DeleteDocuments, delta.ReconcileDocuments} {
		for _, address := range values {
			if err := address.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeltaIndexer applies changed records without rebuilding unaffected entries.
type DeltaIndexer interface {
	ApplyDelta(context.Context, ProjectionDelta) error
}

// FullRebuilder performs an explicit immutable repair rebuild.
type FullRebuilder interface {
	FullRebuild(context.Context, ProjectionRequest) error
}

// SearchRequest is local to one retrieval lane. Scores returned for it are not
// required to share a scale with any other lane.
type SearchRequest struct {
	Scope    corememory.Scope
	Query    string
	Limit    int
	Metadata corememory.Metadata
}

// VectorSearchFilter limits precomputed-vector search without weakening Scope.
type VectorSearchFilter struct {
	Name     string
	Metadata corememory.Metadata
}

// VectorSearchRequest searches a vector projection without invoking inference.
type VectorSearchRequest struct {
	Scope  corememory.Scope
	Vector []float32
	Limit  int
	Filter VectorSearchFilter
}

// Candidate is an unhydrated result from one named search lane. Score retains
// the lane's native meaning and is deliberately not validated as normalized.
type Candidate struct {
	ID          string
	Lane        string
	Name        string
	Score       float64
	Explanation ScoreExplanation
	Source      corememory.SourceRef
	Address     CandidateAddress
	Metadata    corememory.Metadata
}

// ScoreExplanation preserves the portable score derivation. Fusion candidates
// contain one term for every successful lane in which the item appeared.
type ScoreExplanation struct {
	Terms []ScoreTerm
}

type ScoreTerm struct {
	Lane               string
	Raw                float64
	Calibrated         float64
	Weight             float64
	Contribution       float64
	CalibrationVersion string
}

// CandidateAddress is an explicit read-side address for hydration. Empty
// fields are permitted for searchers that instead carry a locator envelope
// such as {"schema_version":1,"address":{...}} in Source.Locator.
type CandidateAddress struct {
	Kind           corememory.ContextItemKind `json:"kind"`
	ConversationID string                     `json:"conversation_id,omitempty"`
	DatasetID      string                     `json:"dataset_id,omitempty"`
	DocumentID     string                     `json:"document_id,omitempty"`
	ItemID         string                     `json:"item_id"`
}

// IsZero reports whether no explicit address was supplied.
func (address CandidateAddress) IsZero() bool {
	return address.Kind == "" && address.ConversationID == "" &&
		address.DatasetID == "" && address.DocumentID == "" && address.ItemID == ""
}

// Validate checks the address shape for its hydrated view kind.
func (address CandidateAddress) Validate() error {
	if address.IsZero() {
		return nil
	}
	if strings.TrimSpace(address.ItemID) == "" {
		return errors.New("memory component: candidate address item_id is required")
	}
	switch address.Kind {
	case corememory.ContextRawMessage, corememory.ContextFact, corememory.ContextSummary:
		if strings.TrimSpace(address.ConversationID) == "" {
			return errors.New("memory component: candidate address conversation_id is required")
		}
	case corememory.ContextDocumentResource, corememory.ContextDocumentSection,
		corememory.ContextDocumentChunk, corememory.ContextDocumentSummary:
		if strings.TrimSpace(address.DatasetID) == "" || strings.TrimSpace(address.DocumentID) == "" {
			return errors.New("memory component: candidate address dataset_id and document_id are required")
		}
	default:
		return fmt.Errorf("memory component: unsupported candidate address kind %q", address.Kind)
	}
	return nil
}

// AddressFromArtifact reads the shared explicit-address metadata. Known
// artifact kinds supply the context kind; item_id defaults to Artifact.ID.
// Unknown kinds return a zero address and must use a SourceRef locator.
func AddressFromArtifact(artifact Artifact) CandidateAddress {
	kind := corememory.ContextItemKind(artifact.Metadata["context_kind"])
	if kind == "" {
		switch artifact.Kind {
		case ArtifactKind(corememory.ContextRawMessage):
			kind = corememory.ContextRawMessage
		case ArtifactKind(corememory.ContextFact):
			kind = corememory.ContextFact
		case ArtifactKind(corememory.ContextSummary):
			kind = corememory.ContextSummary
		case ArtifactKind(corememory.ContextDocumentChunk):
			kind = corememory.ContextDocumentChunk
		default:
			return CandidateAddress{}
		}
	}
	itemID := artifact.Metadata["item_id"]
	if itemID == "" {
		itemID = artifact.ID
	}
	return CandidateAddress{
		Kind: kind, ConversationID: artifact.Metadata["conversation_id"],
		DatasetID: artifact.Metadata["dataset_id"], DocumentID: artifact.Metadata["document_id"], ItemID: itemID,
	}
}

// Validate checks candidate identity and provenance without claiming that its
// lane-native score is portable or normalized.
func (candidate Candidate) Validate() error {
	if strings.TrimSpace(candidate.ID) == "" {
		return errors.New("memory component: candidate id is required")
	}
	if strings.TrimSpace(candidate.Lane) == "" {
		return errors.New("memory component: candidate lane is required")
	}
	if strings.TrimSpace(candidate.Name) == "" {
		return errors.New("memory component: candidate name is required")
	}
	if math.IsNaN(candidate.Score) || math.IsInf(candidate.Score, 0) {
		return errors.New("memory component: candidate score must be finite")
	}
	if err := candidate.Source.Validate(); err != nil {
		return fmt.Errorf("memory component: candidate source: %w", err)
	}
	if err := candidate.Address.Validate(); err != nil {
		return err
	}
	for index, term := range candidate.Explanation.Terms {
		if strings.TrimSpace(term.Lane) == "" || strings.TrimSpace(term.CalibrationVersion) == "" ||
			math.IsNaN(term.Raw) || math.IsInf(term.Raw, 0) ||
			math.IsNaN(term.Calibrated) || math.IsInf(term.Calibrated, 0) ||
			term.Calibrated < 0 || term.Calibrated > 1 ||
			math.IsNaN(term.Weight) || math.IsInf(term.Weight, 0) || term.Weight < 0 ||
			math.IsNaN(term.Contribution) || math.IsInf(term.Contribution, 0) || term.Contribution < 0 {
			return fmt.Errorf("memory component: candidate score explanation term %d is invalid", index)
		}
	}
	return nil
}

// Clone returns an independently owned candidate.
func (candidate Candidate) Clone() Candidate {
	candidate.Metadata = candidate.Metadata.Clone()
	candidate.Explanation.Terms = append([]ScoreTerm(nil), candidate.Explanation.Terms...)
	return candidate
}

// Searcher returns candidates from one lane without cross-lane normalization.
type Searcher interface {
	Search(context.Context, SearchRequest) ([]Candidate, error)
}

// VectorSearcher searches persisted vectors using a precomputed query vector.
type VectorSearcher interface {
	SearchVector(context.Context, VectorSearchRequest) ([]Candidate, error)
}

// Packer selects hydrated, normalized context items within a budget.
type Packer interface {
	Pack(context.Context, []corememory.ContextItem, corememory.Budget) (corememory.ContextResult, error)
}
