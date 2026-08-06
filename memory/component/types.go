// Package component defines the narrow capabilities used by memory pipelines.
package component

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

// ArtifactKind identifies a typed value moving through write-side derivation.
type ArtifactKind string

// Artifact is an immutable-by-convention value produced by a Deriver.
// ID must be stable for the same logical value.
type Artifact struct {
	Kind     ArtifactKind          `json:"kind"`
	ID       string                `json:"id"`
	Content  sdkmessage.Content    `json:"content"`
	Entities []string              `json:"entities,omitempty"`
	Sources  []sdkmemory.SourceRef `json:"sources"`
	Metadata sdkmemory.Metadata    `json:"metadata,omitempty"`
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
	artifact.Sources = append([]sdkmemory.SourceRef(nil), artifact.Sources...)
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
type ProjectionRequest struct {
	Scope      sdkmemory.Scope
	Projection string
	Artifacts  []Artifact
	Metadata   sdkmemory.Metadata
}

// Indexer writes a projection that can be rebuilt from canonical artifacts.
type Indexer interface {
	Rebuild(context.Context, ProjectionRequest) error
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
type ProjectionDelta struct {
	Scope              sdkmemory.Scope
	Projection         string
	Upserts            []Artifact
	DeleteIDs          []string
	DeleteDocuments    []DocumentAddress
	ReconcileDocuments []DocumentAddress
	ActiveIDs          []string
	SourceRevision     string
	SourceDigest       string
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
	Scope    sdkmemory.Scope
	Query    string
	Limit    int
	Metadata sdkmemory.Metadata
}

// VectorSearchFilter limits precomputed-vector search without weakening Scope.
type VectorSearchFilter struct {
	Name     string
	Metadata sdkmemory.Metadata
}

// VectorSearchRequest searches a vector projection without invoking inference.
type VectorSearchRequest struct {
	Scope  sdkmemory.Scope
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
	Source      sdkmemory.SourceRef
	Address     CandidateAddress
	Metadata    sdkmemory.Metadata
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
	Kind           sdkmemory.ContextItemKind `json:"kind"`
	ConversationID string                    `json:"conversation_id,omitempty"`
	DatasetID      string                    `json:"dataset_id,omitempty"`
	DocumentID     string                    `json:"document_id,omitempty"`
	ItemID         string                    `json:"item_id"`
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
	case sdkmemory.ContextRawMessage, sdkmemory.ContextFact, sdkmemory.ContextSummary:
		if strings.TrimSpace(address.ConversationID) == "" {
			return errors.New("memory component: candidate address conversation_id is required")
		}
	case sdkmemory.ContextDocumentResource, sdkmemory.ContextDocumentSection,
		sdkmemory.ContextDocumentChunk, sdkmemory.ContextDocumentSummary:
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
	kind := sdkmemory.ContextItemKind(artifact.Metadata["context_kind"])
	if kind == "" {
		switch artifact.Kind {
		case ArtifactKind(sdkmemory.ContextRawMessage):
			kind = sdkmemory.ContextRawMessage
		case ArtifactKind(sdkmemory.ContextFact):
			kind = sdkmemory.ContextFact
		case ArtifactKind(sdkmemory.ContextSummary):
			kind = sdkmemory.ContextSummary
		case ArtifactKind(sdkmemory.ContextDocumentChunk):
			kind = sdkmemory.ContextDocumentChunk
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

type RerankRequest struct {
	Scope      sdkmemory.Scope
	Query      string
	Candidates []Candidate
}

// Reranker may return only a unique subset of the supplied candidates, in any
// order and with updated finite scores. It cannot create identities.
type Reranker interface {
	Rerank(context.Context, RerankRequest) ([]Candidate, error)
}

// Packer selects hydrated, normalized context items within a budget.
type Packer interface {
	Pack(context.Context, []sdkmemory.ContextItem, sdkmemory.Budget) (sdkmemory.ContextResult, error)
}
