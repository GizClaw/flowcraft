package observation

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

// LedgerWorkspaceStore persists observation ledger records as JSON files in a
// workspace.
//
// Each observation is stored under:
// observations/{encodedObservationID}.json
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

// defaultLedgerPathSegmentPrefix marks encoded workspace path segments. It is
// not part of observation IDs or other business identifiers.
const defaultLedgerPathSegmentPrefix = "obs_"

// LedgerWorkspaceStoreOption configures a LedgerWorkspaceStore.
type LedgerWorkspaceStoreOption interface {
	applyLedgerWorkspaceStore(*LedgerWorkspaceStore)
}

type ledgerPathSegmentPrefixOption string

// WithLedgerPathSegmentPrefix sets the encoded workspace path segment marker.
// Passing an empty prefix is explicit and disables the marker.
func WithLedgerPathSegmentPrefix(prefix string) LedgerWorkspaceStoreOption {
	return ledgerPathSegmentPrefixOption(prefix)
}

func (o ledgerPathSegmentPrefixOption) applyLedgerWorkspaceStore(s *LedgerWorkspaceStore) {
	s.pathSegmentPrefix = string(o)
}

// NewLedgerWorkspaceStore returns a workspace-backed observation ledger store.
func NewLedgerWorkspaceStore(ws workspace.Workspace, opts ...LedgerWorkspaceStoreOption) *LedgerWorkspaceStore {
	s := &LedgerWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultLedgerPathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyLedgerWorkspaceStore(s)
		}
	}
	return s
}

// Put stores or replaces the authoritative observation for its id.
func (s *LedgerWorkspaceStore) Put(ctx context.Context, observation Observation) (Observation, error) {
	if s.ws == nil {
		return Observation{}, errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateObservation(observation); err != nil {
		return Observation{}, err
	}

	observation = cloneObservation(observation)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeObservation(ctx, observation); err != nil {
		return Observation{}, err
	}
	return cloneObservation(observation), nil
}

// Get returns one observation by id.
func (s *LedgerWorkspaceStore) Get(ctx context.Context, id string) (Observation, bool, error) {
	if s.ws == nil {
		return Observation{}, false, errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateObservationID(id); err != nil {
		return Observation{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	observation, ok, err := s.readObservation(ctx, id)
	if err != nil {
		return Observation{}, false, err
	}
	if !ok {
		return Observation{}, false, nil
	}
	return cloneObservation(observation), true, nil
}

// List returns observations ordered by ascending observation id.
func (s *LedgerWorkspaceStore) List(ctx context.Context, opts ListOptions) ([]Observation, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateListOptions(opts); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.observationIDs(ctx, opts.AfterID)
	if err != nil {
		return nil, err
	}

	out := make([]Observation, 0, len(ids))
	for _, id := range ids {
		observation, ok, err := s.readObservation(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.Scope != nil && !sameScopeIdentity(observation.Scope, *opts.Scope) {
			continue
		}
		if opts.Subject != "" && observation.Subject != opts.Subject {
			continue
		}
		out = append(out, cloneObservation(observation))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Delete removes one observation by id. It is idempotent.
func (s *LedgerWorkspaceStore) Delete(ctx context.Context, id string) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateObservationID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.observationPath(id)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete observation %q: %w", ledgerErrPrefix, id, err)
	}
	return nil
}

// DeleteScope removes all observations for one scope identity. It is idempotent.
func (s *LedgerWorkspaceStore) DeleteScope(ctx context.Context, scope Scope) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", ledgerErrPrefix)
	}
	if err := validateScope(scope); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := s.observationIDs(ctx, "")
	if err != nil {
		return err
	}
	for _, id := range ids {
		observation, ok, err := s.readObservation(ctx, id)
		if err != nil {
			return err
		}
		if !ok || !sameScopeIdentity(observation.Scope, scope) {
			continue
		}
		if err := s.ws.Delete(ctx, s.observationPath(id)); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("%s: delete observation %q in scope %q: %w", ledgerErrPrefix, id, scope.HardPartitionKey(), err)
		}
	}
	return nil
}

func (s *LedgerWorkspaceStore) observationIDs(ctx context.Context, afterID string) ([]string, error) {
	entries, err := s.ws.List(ctx, s.observationsDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list observations: %w", ledgerErrPrefix, err)
	}

	ids := make([]string, 0, len(entries))
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
			return nil, fmt.Errorf("%s: decode observation id %q: %w", ledgerErrPrefix, entry.Name(), err)
		}
		if id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *LedgerWorkspaceStore) readObservation(ctx context.Context, id string) (Observation, bool, error) {
	data, err := s.ws.Read(ctx, s.observationPath(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Observation{}, false, nil
		}
		return Observation{}, false, fmt.Errorf("%s: read observation %q: %w", ledgerErrPrefix, id, err)
	}

	var observation Observation
	if err := decodeObservation(data, &observation); err != nil {
		return Observation{}, false, fmt.Errorf("%s: decode observation %q: %w", ledgerErrPrefix, id, err)
	}
	return observation, true, nil
}

func (s *LedgerWorkspaceStore) writeObservation(ctx context.Context, observation Observation) error {
	data, err := encodeObservation(observation)
	if err != nil {
		return fmt.Errorf("%s: marshal observation %q: %w", ledgerErrPrefix, observation.ID, err)
	}

	livePath := s.observationPath(observation.ID)
	tmpPath := s.tmpObservationPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write observation tmp %q: %w", ledgerErrPrefix, observation.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish observation %q: %w", ledgerErrPrefix, observation.ID, err)
	}
	return nil
}

func (s *LedgerWorkspaceStore) tmpObservationPath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *LedgerWorkspaceStore) observationsDir() string {
	return "observations"
}

func (s *LedgerWorkspaceStore) observationPath(id string) string {
	return path.Join(s.observationsDir(), s.pathSegment(id)+".json")
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

func sameScopeIdentity(a, b Scope) bool {
	return a == b
}

type observationRecord struct {
	ID         string              `json:"id"`
	Scope      scopeRecord         `json:"scope"`
	Subject    string              `json:"subject"`
	Predicate  string              `json:"predicate"`
	Object     string              `json:"object"`
	Confidence float64             `json:"confidence"`
	SourceRefs []sourceRefRecord   `json:"source_refs,omitempty"`
	Signature  views.ViewSignature `json:"signature"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Metadata   map[string]any      `json:"metadata,omitempty"`
}

type scopeRecord struct {
	RuntimeID      string `json:"runtime_id"`
	UserID         string `json:"user_id,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	DatasetID      string `json:"dataset_id,omitempty"`
	EntityID       string `json:"entity_id,omitempty"`
}

type sourceRefRecord struct {
	Kind     views.SourceKind         `json:"kind"`
	Message  *views.MessageSourceRef  `json:"message,omitempty"`
	Document *views.DocumentSourceRef `json:"document,omitempty"`
}

func encodeObservation(observation Observation) ([]byte, error) {
	observation = cloneObservation(observation)
	return json.Marshal(observationRecord{
		ID:         observation.ID,
		Scope:      scopeRecordFromScope(observation.Scope),
		Subject:    observation.Subject,
		Predicate:  observation.Predicate,
		Object:     observation.Object,
		Confidence: observation.Confidence,
		SourceRefs: sourceRefRecords(observation.SourceRefs),
		Signature:  cloneViewSignature(observation.Signature),
		CreatedAt:  observation.CreatedAt,
		UpdatedAt:  observation.UpdatedAt,
		Metadata:   cloneAnyMap(observation.Metadata),
	})
}

func decodeObservation(data []byte, observation *Observation) error {
	var record observationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return err
	}
	*observation = Observation{
		ID:         record.ID,
		Scope:      scopeFromRecord(record.Scope),
		Subject:    record.Subject,
		Predicate:  record.Predicate,
		Object:     record.Object,
		Confidence: record.Confidence,
		SourceRefs: sourceRefsFromRecords(record.SourceRefs),
		Signature:  cloneViewSignature(record.Signature),
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		Metadata:   cloneAnyMap(record.Metadata),
	}
	return nil
}

func scopeRecordFromScope(scope Scope) scopeRecord {
	return scopeRecord{
		RuntimeID:      scope.RuntimeID,
		UserID:         scope.UserID,
		AgentID:        scope.AgentID,
		ConversationID: scope.ConversationID,
		DatasetID:      scope.DatasetID,
		EntityID:       scope.EntityID,
	}
}

func scopeFromRecord(record scopeRecord) Scope {
	return Scope{
		RuntimeID:      record.RuntimeID,
		UserID:         record.UserID,
		AgentID:        record.AgentID,
		ConversationID: record.ConversationID,
		DatasetID:      record.DatasetID,
		EntityID:       record.EntityID,
	}
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
