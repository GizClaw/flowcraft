package fact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// LedgerWorkspaceStore persists fact ledger records as JSON files in a workspace.
//
// Each fact is stored under:
// facts/{encodedFactID}.json
//
// Concurrent writes to the same scoped workspace must go through one
// LedgerWorkspaceStore instance. Cross-instance or cross-process writers require
// an external lock or a workspace backend with stronger concurrency guarantees.
type LedgerWorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64

	mu sync.RWMutex
}

var _ Store = (*LedgerWorkspaceStore)(nil)

// defaultFactPathSegmentPrefix marks encoded workspace path segments. It is not
// part of fact IDs or other business identifiers.
const defaultFactPathSegmentPrefix = "fact_"

// LedgerWorkspaceStoreOption configures a LedgerWorkspaceStore.
type LedgerWorkspaceStoreOption interface {
	applyLedgerWorkspaceStore(*LedgerWorkspaceStore)
}

type factPathSegmentPrefixOption string

// WithFactPathSegmentPrefix sets the encoded workspace path segment marker.
// Passing an empty prefix is explicit and disables the marker.
func WithFactPathSegmentPrefix(prefix string) LedgerWorkspaceStoreOption {
	return factPathSegmentPrefixOption(prefix)
}

func (o factPathSegmentPrefixOption) applyLedgerWorkspaceStore(s *LedgerWorkspaceStore) {
	s.pathSegmentPrefix = string(o)
}

// NewLedgerWorkspaceStore returns a workspace-backed fact ledger store.
func NewLedgerWorkspaceStore(ws workspace.Workspace, opts ...LedgerWorkspaceStoreOption) *LedgerWorkspaceStore {
	s := &LedgerWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultFactPathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyLedgerWorkspaceStore(s)
		}
	}
	return s
}

// Put stores or replaces the authoritative fact for its id. Empty status is
// normalized to active.
func (s *LedgerWorkspaceStore) Put(ctx context.Context, fact Fact) (Fact, error) {
	if s.ws == nil {
		return Fact{}, errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	fact = normalizeFact(cloneFact(fact))
	if err := validateFact(fact); err != nil {
		return Fact{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeFact(ctx, fact); err != nil {
		return Fact{}, err
	}
	return cloneFact(fact), nil
}

// Get returns one fact by id.
func (s *LedgerWorkspaceStore) Get(ctx context.Context, id FactID) (Fact, bool, error) {
	if s.ws == nil {
		return Fact{}, false, errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateFactID(id); err != nil {
		return Fact{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	fact, ok, err := s.readFact(ctx, id)
	if err != nil {
		return Fact{}, false, err
	}
	if !ok {
		return Fact{}, false, nil
	}
	return cloneFact(fact), true, nil
}

// List returns facts ordered by ascending fact id.
func (s *LedgerWorkspaceStore) List(ctx context.Context, opts ListOptions) ([]Fact, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateListOptions(opts); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.factIDs(ctx, opts.AfterID)
	if err != nil {
		return nil, err
	}

	out := make([]Fact, 0, len(ids))
	for _, id := range ids {
		fact, ok, err := s.readFact(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.Subject != "" && fact.Subject != opts.Subject {
			continue
		}
		if opts.Predicate != "" && fact.Predicate != opts.Predicate {
			continue
		}
		if opts.Status != nil && fact.Status != *opts.Status {
			continue
		}
		out = append(out, cloneFact(fact))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Delete removes one fact by id. It is idempotent.
func (s *LedgerWorkspaceStore) Delete(ctx context.Context, id FactID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateFactID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.factPath(id)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete fact %q: %w", ledgerErrPrefix, id, err)
	}
	return nil
}

// DeleteSubject removes all facts for one subject. It is idempotent.
func (s *LedgerWorkspaceStore) DeleteSubject(ctx context.Context, subject string) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateSubject(subject); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := s.factIDs(ctx, "")
	if err != nil {
		return err
	}
	for _, id := range ids {
		fact, ok, err := s.readFact(ctx, id)
		if err != nil {
			return err
		}
		if !ok || fact.Subject != subject {
			continue
		}
		if err := s.ws.Delete(ctx, s.factPath(id)); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("%s: delete fact %q for subject %q: %w", ledgerErrPrefix, id, subject, err)
		}
	}
	return nil
}

func (s *LedgerWorkspaceStore) factIDs(ctx context.Context, afterID FactID) ([]FactID, error) {
	entries, err := s.ws.List(ctx, s.factsDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list facts: %w", ledgerErrPrefix, err)
	}

	ids := make([]FactID, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		segment := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
			continue
		}
		id, err := s.rawPathSegment(segment)
		if err != nil {
			return nil, fmt.Errorf("%s: decode fact id %q: %w", ledgerErrPrefix, entry.Name(), err)
		}
		factID := FactID(id)
		if factID > afterID {
			ids = append(ids, factID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids, nil
}

func (s *LedgerWorkspaceStore) readFact(ctx context.Context, id FactID) (Fact, bool, error) {
	data, err := s.ws.Read(ctx, s.factPath(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Fact{}, false, nil
		}
		return Fact{}, false, fmt.Errorf("%s: read fact %q: %w", ledgerErrPrefix, id, err)
	}

	var fact Fact
	if err := decodeFact(data, &fact); err != nil {
		return Fact{}, false, fmt.Errorf("%s: decode fact %q: %w", ledgerErrPrefix, id, err)
	}
	return fact, true, nil
}

func (s *LedgerWorkspaceStore) writeFact(ctx context.Context, fact Fact) error {
	data, err := encodeFact(fact)
	if err != nil {
		return fmt.Errorf("%s: marshal fact %q: %w", ledgerErrPrefix, fact.ID, err)
	}

	livePath := s.factPath(fact.ID)
	tmpPath := s.tmpFactPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write fact tmp %q: %w", ledgerErrPrefix, fact.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish fact %q: %w", ledgerErrPrefix, fact.ID, err)
	}
	return nil
}

func (s *LedgerWorkspaceStore) tmpFactPath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *LedgerWorkspaceStore) factsDir() string {
	return "facts"
}

func (s *LedgerWorkspaceStore) factPath(id FactID) string {
	return path.Join(s.factsDir(), s.pathSegment(string(id))+".json")
}

func (s *LedgerWorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (s *LedgerWorkspaceStore) rawPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
		return "", fmt.Errorf("missing %q prefix", s.pathSegmentPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, s.pathSegmentPrefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type factRecord struct {
	ID              FactID              `json:"id"`
	Subject         string              `json:"subject"`
	Predicate       string              `json:"predicate"`
	Object          string              `json:"object"`
	Status          FactStatus          `json:"status"`
	Confidence      float64             `json:"confidence"`
	ValidFrom       *time.Time          `json:"valid_from,omitempty"`
	ValidUntil      *time.Time          `json:"valid_until,omitempty"`
	ObservationRefs []observationRecord `json:"observation_refs,omitempty"`
	SourceRefs      []sourceRefRecord   `json:"source_refs,omitempty"`
	Signature       views.ViewSignature `json:"signature"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Metadata        map[string]any      `json:"metadata,omitempty"`
}

type observationRecord struct {
	ObservationID string `json:"observation_id"`
	ScopeKind     string `json:"scope_kind,omitempty"`
	ScopeID       string `json:"scope_id,omitempty"`
}

type sourceRefRecord struct {
	Kind     views.SourceKind         `json:"kind"`
	Message  *views.MessageSourceRef  `json:"message,omitempty"`
	Document *views.DocumentSourceRef `json:"document,omitempty"`
}

func encodeFact(fact Fact) ([]byte, error) {
	fact = cloneFact(fact)
	return json.Marshal(factRecord{
		ID:              fact.ID,
		Subject:         fact.Subject,
		Predicate:       fact.Predicate,
		Object:          fact.Object,
		Status:          fact.Status,
		Confidence:      fact.Confidence,
		ValidFrom:       cloneTimePtr(fact.ValidFrom),
		ValidUntil:      cloneTimePtr(fact.ValidUntil),
		ObservationRefs: observationRecords(fact.ObservationRefs),
		SourceRefs:      sourceRefRecords(fact.SourceRefs),
		Signature:       cloneViewSignature(fact.Signature),
		CreatedAt:       fact.CreatedAt,
		UpdatedAt:       fact.UpdatedAt,
		Metadata:        cloneAnyMap(fact.Metadata),
	})
}

func decodeFact(data []byte, fact *Fact) error {
	var record factRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*fact = Fact{
		ID:              record.ID,
		Subject:         record.Subject,
		Predicate:       record.Predicate,
		Object:          record.Object,
		Status:          record.Status,
		Confidence:      record.Confidence,
		ValidFrom:       cloneTimePtr(record.ValidFrom),
		ValidUntil:      cloneTimePtr(record.ValidUntil),
		ObservationRefs: observationRefsFromRecords(record.ObservationRefs),
		SourceRefs:      sourceRefsFromRecords(record.SourceRefs),
		Signature:       cloneViewSignature(record.Signature),
		CreatedAt:       record.CreatedAt,
		UpdatedAt:       record.UpdatedAt,
		Metadata:        cloneAnyMap(record.Metadata),
	}
	return nil
}

func observationRecords(refs []ObservationRef) []observationRecord {
	if refs == nil {
		return nil
	}
	records := make([]observationRecord, len(refs))
	for i, ref := range refs {
		records[i] = observationRecord{
			ObservationID: ref.ObservationID,
			ScopeKind:     ref.ScopeKind,
			ScopeID:       ref.ScopeID,
		}
	}
	return records
}

func observationRefsFromRecords(records []observationRecord) []ObservationRef {
	if records == nil {
		return nil
	}
	refs := make([]ObservationRef, len(records))
	for i, record := range records {
		refs[i] = ObservationRef{
			ObservationID: record.ObservationID,
			ScopeKind:     record.ScopeKind,
			ScopeID:       record.ScopeID,
		}
	}
	return refs
}

func sourceRefRecords(refs []views.SourceRef) []sourceRefRecord {
	if refs == nil {
		return nil
	}
	records := make([]sourceRefRecord, len(refs))
	for i, ref := range refs {
		ref = cloneSourceRef(ref)
		records[i] = sourceRefRecord{
			Kind:     ref.Kind,
			Message:  ref.Message,
			Document: ref.Document,
		}
	}
	return records
}

func sourceRefsFromRecords(records []sourceRefRecord) []views.SourceRef {
	if records == nil {
		return nil
	}
	refs := make([]views.SourceRef, len(records))
	for i, record := range records {
		refs[i] = cloneSourceRef(views.SourceRef{
			Kind:     record.Kind,
			Message:  record.Message,
			Document: record.Document,
		})
	}
	return refs
}
