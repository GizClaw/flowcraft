package entity

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
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// ProfileWorkspaceStore persists entity profile records as JSON files in a workspace.
//
// Each profile is stored under:
// entity/profiles/{encodedProfileID}.json
//
// Concurrent writes to the same scoped workspace must go through one
// ProfileWorkspaceStore instance. Cross-instance or cross-process writers
// require an external lock or a workspace backend with stronger concurrency
// guarantees.
type ProfileWorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64

	mu sync.RWMutex
}

var _ ProfileStore = (*ProfileWorkspaceStore)(nil)

// defaultProfilePathSegmentPrefix marks encoded workspace path segments. It is
// not part of profile IDs or other business identifiers.
const defaultProfilePathSegmentPrefix = "eprof_"

// ProfileWorkspaceStoreOption configures a ProfileWorkspaceStore.
type ProfileWorkspaceStoreOption interface {
	applyProfileWorkspaceStore(*ProfileWorkspaceStore)
}

type profilePathSegmentPrefixOption string

// WithProfilePathSegmentPrefix sets the encoded workspace path segment marker.
// Passing an empty prefix is explicit and disables the marker.
func WithProfilePathSegmentPrefix(prefix string) ProfileWorkspaceStoreOption {
	return profilePathSegmentPrefixOption(prefix)
}

func (o profilePathSegmentPrefixOption) applyProfileWorkspaceStore(s *ProfileWorkspaceStore) {
	s.pathSegmentPrefix = string(o)
}

// NewProfileWorkspaceStore returns a workspace-backed profile store.
func NewProfileWorkspaceStore(ws workspace.Workspace, opts ...ProfileWorkspaceStoreOption) *ProfileWorkspaceStore {
	s := &ProfileWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultProfilePathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyProfileWorkspaceStore(s)
		}
	}
	return s
}

// Put stores or replaces the authoritative profile record for its id.
func (s *ProfileWorkspaceStore) Put(ctx context.Context, record ProfileRecord) (ProfileRecord, error) {
	if s.ws == nil {
		return ProfileRecord{}, errdefs.Validationf("%s: workspace is required", profileErrPrefix)
	}
	record = cloneProfileRecord(record)
	if err := validateProfileRecord(record); err != nil {
		return ProfileRecord{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeProfile(ctx, record); err != nil {
		return ProfileRecord{}, err
	}
	return cloneProfileRecord(record), nil
}

// Get returns one profile record by id.
func (s *ProfileWorkspaceStore) Get(ctx context.Context, id ProfileID) (ProfileRecord, bool, error) {
	if s.ws == nil {
		return ProfileRecord{}, false, errdefs.Validationf("%s: workspace is required", profileErrPrefix)
	}
	if err := validateProfileID(id); err != nil {
		return ProfileRecord{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok, err := s.readProfile(ctx, id)
	if err != nil {
		return ProfileRecord{}, false, err
	}
	if !ok {
		return ProfileRecord{}, false, nil
	}
	return cloneProfileRecord(record), true, nil
}

// List returns profiles ordered by ascending profile id.
func (s *ProfileWorkspaceStore) List(ctx context.Context, opts ProfileListOptions) ([]ProfileRecord, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", profileErrPrefix)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.profileIDs(ctx, opts.AfterID)
	if err != nil {
		return nil, err
	}

	out := make([]ProfileRecord, 0, len(ids))
	for _, id := range ids {
		record, ok, err := s.readProfile(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.EntityID != "" && record.EntityID != opts.EntityID {
			continue
		}
		if opts.Label != "" && record.Label != opts.Label {
			continue
		}
		out = append(out, cloneProfileRecord(record))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Delete removes one profile by id. It is idempotent.
func (s *ProfileWorkspaceStore) Delete(ctx context.Context, id ProfileID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", profileErrPrefix)
	}
	if err := validateProfileID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.profilePath(id)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("%s: delete profile %q: %w", profileErrPrefix, id, err)
	}
	return nil
}

// DeleteEntity removes all profile records for one entity. It is idempotent.
func (s *ProfileWorkspaceStore) DeleteEntity(ctx context.Context, entityID fact.NodeID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", profileErrPrefix)
	}
	if err := validateEntityID(profileErrPrefix, entityID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := s.profileIDs(ctx, "")
	if err != nil {
		return err
	}
	for _, id := range ids {
		record, ok, err := s.readProfile(ctx, id)
		if err != nil {
			return err
		}
		if !ok || record.EntityID != entityID {
			continue
		}
		if err := s.ws.Delete(ctx, s.profilePath(id)); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("%s: delete profile %q for entity %q: %w", profileErrPrefix, id, entityID, err)
		}
	}
	return nil
}

func (s *ProfileWorkspaceStore) profileIDs(ctx context.Context, afterID ProfileID) ([]ProfileID, error) {
	entries, err := s.ws.List(ctx, s.profilesDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list profiles: %w", profileErrPrefix, err)
	}

	ids := make([]ProfileID, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		segment := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
			continue
		}
		id, err := rawPathSegment(segment, s.pathSegmentPrefix)
		if err != nil {
			return nil, fmt.Errorf("%s: decode profile id %q: %w", profileErrPrefix, entry.Name(), err)
		}
		profileID := ProfileID(id)
		if profileID > afterID {
			ids = append(ids, profileID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids, nil
}

func (s *ProfileWorkspaceStore) readProfile(ctx context.Context, id ProfileID) (ProfileRecord, bool, error) {
	data, err := s.ws.Read(ctx, s.profilePath(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ProfileRecord{}, false, nil
		}
		return ProfileRecord{}, false, fmt.Errorf("%s: read profile %q: %w", profileErrPrefix, id, err)
	}

	var record ProfileRecord
	if err := decodeProfileRecord(data, &record); err != nil {
		return ProfileRecord{}, false, fmt.Errorf("%s: decode profile %q: %w", profileErrPrefix, id, err)
	}
	return record, true, nil
}

func (s *ProfileWorkspaceStore) writeProfile(ctx context.Context, record ProfileRecord) error {
	data, err := encodeProfileRecord(record)
	if err != nil {
		return fmt.Errorf("%s: marshal profile %q: %w", profileErrPrefix, record.ID, err)
	}

	livePath := s.profilePath(record.ID)
	tmpPath := s.tmpProfilePath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write profile tmp %q: %w", profileErrPrefix, record.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish profile %q: %w", profileErrPrefix, record.ID, err)
	}
	return nil
}

func (s *ProfileWorkspaceStore) tmpProfilePath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *ProfileWorkspaceStore) profilesDir() string {
	return path.Join("entity", "profiles")
}

func (s *ProfileWorkspaceStore) profilePath(id ProfileID) string {
	return path.Join(s.profilesDir(), s.pathSegment(string(id))+".json")
}

func (s *ProfileWorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + encodedPathSegment(id)
}

func (s *ProfileWorkspaceStore) rawPathSegment(segment string) (string, error) {
	return rawPathSegment(segment, s.pathSegmentPrefix)
}

// TimelineWorkspaceStore persists entity timeline events as JSON files in a workspace.
//
// Each event is stored under:
// entity/timeline/{encodedEventID}.json
//
// Concurrent writes to the same scoped workspace must go through one
// TimelineWorkspaceStore instance. Cross-instance or cross-process writers
// require an external lock or a workspace backend with stronger concurrency
// guarantees.
type TimelineWorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64

	mu sync.RWMutex
}

var _ TimelineStore = (*TimelineWorkspaceStore)(nil)

// defaultTimelinePathSegmentPrefix marks encoded workspace path segments. It is
// not part of event IDs or other business identifiers.
const defaultTimelinePathSegmentPrefix = "etl_"

// TimelineWorkspaceStoreOption configures a TimelineWorkspaceStore.
type TimelineWorkspaceStoreOption interface {
	applyTimelineWorkspaceStore(*TimelineWorkspaceStore)
}

type timelinePathSegmentPrefixOption string

// WithTimelinePathSegmentPrefix sets the encoded workspace path segment marker.
// Passing an empty prefix is explicit and disables the marker.
func WithTimelinePathSegmentPrefix(prefix string) TimelineWorkspaceStoreOption {
	return timelinePathSegmentPrefixOption(prefix)
}

func (o timelinePathSegmentPrefixOption) applyTimelineWorkspaceStore(s *TimelineWorkspaceStore) {
	s.pathSegmentPrefix = string(o)
}

// NewTimelineWorkspaceStore returns a workspace-backed timeline store.
func NewTimelineWorkspaceStore(ws workspace.Workspace, opts ...TimelineWorkspaceStoreOption) *TimelineWorkspaceStore {
	s := &TimelineWorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultTimelinePathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyTimelineWorkspaceStore(s)
		}
	}
	return s
}

// Put stores or replaces the authoritative timeline event for its id.
func (s *TimelineWorkspaceStore) Put(ctx context.Context, event Event) (Event, error) {
	if s.ws == nil {
		return Event{}, errdefs.Validationf("%s: workspace is required", timelineErrPrefix)
	}
	event = cloneEvent(event)
	if err := validateEvent(event); err != nil {
		return Event{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeEvent(ctx, event); err != nil {
		return Event{}, err
	}
	return cloneEvent(event), nil
}

// Get returns one timeline event by id.
func (s *TimelineWorkspaceStore) Get(ctx context.Context, id EventID) (Event, bool, error) {
	if s.ws == nil {
		return Event{}, false, errdefs.Validationf("%s: workspace is required", timelineErrPrefix)
	}
	if err := validateEventID(id); err != nil {
		return Event{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	event, ok, err := s.readEvent(ctx, id)
	if err != nil {
		return Event{}, false, err
	}
	if !ok {
		return Event{}, false, nil
	}
	return cloneEvent(event), true, nil
}

// List returns timeline events ordered by ascending event id.
func (s *TimelineWorkspaceStore) List(ctx context.Context, opts TimelineListOptions) ([]Event, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", timelineErrPrefix)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.eventIDs(ctx, opts.AfterID)
	if err != nil {
		return nil, err
	}

	out := make([]Event, 0, len(ids))
	for _, id := range ids {
		event, ok, err := s.readEvent(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if opts.EntityID != "" && event.EntityID != opts.EntityID {
			continue
		}
		out = append(out, cloneEvent(event))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

// Delete removes one timeline event by id. It is idempotent.
func (s *TimelineWorkspaceStore) Delete(ctx context.Context, id EventID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", timelineErrPrefix)
	}
	if err := validateEventID(id); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.Delete(ctx, s.eventPath(id)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("%s: delete event %q: %w", timelineErrPrefix, id, err)
	}
	return nil
}

// DeleteEntity removes all timeline events for one entity. It is idempotent.
func (s *TimelineWorkspaceStore) DeleteEntity(ctx context.Context, entityID fact.NodeID) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", timelineErrPrefix)
	}
	if err := validateEntityID(timelineErrPrefix, entityID); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := s.eventIDs(ctx, "")
	if err != nil {
		return err
	}
	for _, id := range ids {
		event, ok, err := s.readEvent(ctx, id)
		if err != nil {
			return err
		}
		if !ok || event.EntityID != entityID {
			continue
		}
		if err := s.ws.Delete(ctx, s.eventPath(id)); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("%s: delete event %q for entity %q: %w", timelineErrPrefix, id, entityID, err)
		}
	}
	return nil
}

func (s *TimelineWorkspaceStore) eventIDs(ctx context.Context, afterID EventID) ([]EventID, error) {
	entries, err := s.ws.List(ctx, s.eventsDir())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: list events: %w", timelineErrPrefix, err)
	}

	ids := make([]EventID, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		segment := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
			continue
		}
		id, err := rawPathSegment(segment, s.pathSegmentPrefix)
		if err != nil {
			return nil, fmt.Errorf("%s: decode event id %q: %w", timelineErrPrefix, entry.Name(), err)
		}
		eventID := EventID(id)
		if eventID > afterID {
			ids = append(ids, eventID)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids, nil
}

func (s *TimelineWorkspaceStore) readEvent(ctx context.Context, id EventID) (Event, bool, error) {
	data, err := s.ws.Read(ctx, s.eventPath(id))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return Event{}, false, nil
		}
		return Event{}, false, fmt.Errorf("%s: read event %q: %w", timelineErrPrefix, id, err)
	}

	var event Event
	if err := decodeEvent(data, &event); err != nil {
		return Event{}, false, fmt.Errorf("%s: decode event %q: %w", timelineErrPrefix, id, err)
	}
	return event, true, nil
}

func (s *TimelineWorkspaceStore) writeEvent(ctx context.Context, event Event) error {
	data, err := encodeEvent(event)
	if err != nil {
		return fmt.Errorf("%s: marshal event %q: %w", timelineErrPrefix, event.ID, err)
	}

	livePath := s.eventPath(event.ID)
	tmpPath := s.tmpEventPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return fmt.Errorf("%s: write event tmp %q: %w", timelineErrPrefix, event.ID, err)
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return fmt.Errorf("%s: publish event %q: %w", timelineErrPrefix, event.ID, err)
	}
	return nil
}

func (s *TimelineWorkspaceStore) tmpEventPath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *TimelineWorkspaceStore) eventsDir() string {
	return path.Join("entity", "timeline")
}

func (s *TimelineWorkspaceStore) eventPath(id EventID) string {
	return path.Join(s.eventsDir(), s.pathSegment(string(id))+".json")
}

func (s *TimelineWorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + encodedPathSegment(id)
}

func (s *TimelineWorkspaceStore) rawPathSegment(segment string) (string, error) {
	return rawPathSegment(segment, s.pathSegmentPrefix)
}

func encodedPathSegment(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func rawPathSegment(segment, prefix string) (string, error) {
	if !strings.HasPrefix(segment, prefix) {
		return "", fmt.Errorf("missing %q prefix", prefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, prefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type profileRecord struct {
	ID         ProfileID           `json:"id"`
	EntityID   fact.NodeID         `json:"entity_id"`
	Label      string              `json:"label"`
	Summary    string              `json:"summary,omitempty"`
	Slots      []slotRecord        `json:"slots,omitempty"`
	FactRefs   []factRefRecord     `json:"fact_refs,omitempty"`
	SourceRefs []sourceRefRecord   `json:"source_refs,omitempty"`
	Signature  views.ViewSignature `json:"signature"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
	Metadata   map[string]any      `json:"metadata,omitempty"`
}

type slotRecord struct {
	Name       string          `json:"name"`
	Value      string          `json:"value"`
	Confidence float64         `json:"confidence,omitempty"`
	FactRefs   []factRefRecord `json:"fact_refs,omitempty"`
	Metadata   map[string]any  `json:"metadata,omitempty"`
}

type eventRecord struct {
	ID          EventID             `json:"id"`
	EntityID    fact.NodeID         `json:"entity_id"`
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	OccurredAt  *time.Time          `json:"occurred_at,omitempty"`
	ValidFrom   *time.Time          `json:"valid_from,omitempty"`
	ValidUntil  *time.Time          `json:"valid_until,omitempty"`
	FactRefs    []factRefRecord     `json:"fact_refs,omitempty"`
	SourceRefs  []sourceRefRecord   `json:"source_refs,omitempty"`
	Signature   views.ViewSignature `json:"signature"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
	Metadata    map[string]any      `json:"metadata,omitempty"`
}

type factRefRecord struct {
	FactID fact.FactID `json:"fact_id"`
	Role   string      `json:"role,omitempty"`
}

type sourceRefRecord struct {
	Kind     views.SourceKind         `json:"kind"`
	Message  *views.MessageSourceRef  `json:"message,omitempty"`
	Document *views.DocumentSourceRef `json:"document,omitempty"`
}

func encodeProfileRecord(record ProfileRecord) ([]byte, error) {
	record = cloneProfileRecord(record)
	return json.Marshal(profileRecord{
		ID:         record.ID,
		EntityID:   record.EntityID,
		Label:      record.Label,
		Summary:    record.Summary,
		Slots:      slotRecords(record.Slots),
		FactRefs:   factRefRecords(record.FactRefs),
		SourceRefs: sourceRefRecords(record.SourceRefs),
		Signature:  cloneViewSignature(record.Signature),
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
		Metadata:   cloneAnyMap(record.Metadata),
	})
}

func decodeProfileRecord(data []byte, record *ProfileRecord) error {
	var stored profileRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	*record = ProfileRecord{
		ID:         stored.ID,
		EntityID:   stored.EntityID,
		Label:      stored.Label,
		Summary:    stored.Summary,
		Slots:      slotsFromRecords(stored.Slots),
		FactRefs:   factRefsFromRecords(stored.FactRefs),
		SourceRefs: sourceRefsFromRecords(stored.SourceRefs),
		Signature:  cloneViewSignature(stored.Signature),
		CreatedAt:  stored.CreatedAt,
		UpdatedAt:  stored.UpdatedAt,
		Metadata:   cloneAnyMap(stored.Metadata),
	}
	return nil
}

func encodeEvent(event Event) ([]byte, error) {
	event = cloneEvent(event)
	return json.Marshal(eventRecord{
		ID:          event.ID,
		EntityID:    event.EntityID,
		Title:       event.Title,
		Description: event.Description,
		OccurredAt:  cloneTimePtr(event.OccurredAt),
		ValidFrom:   cloneTimePtr(event.ValidFrom),
		ValidUntil:  cloneTimePtr(event.ValidUntil),
		FactRefs:    factRefRecords(event.FactRefs),
		SourceRefs:  sourceRefRecords(event.SourceRefs),
		Signature:   cloneViewSignature(event.Signature),
		CreatedAt:   event.CreatedAt,
		UpdatedAt:   event.UpdatedAt,
		Metadata:    cloneAnyMap(event.Metadata),
	})
}

func decodeEvent(data []byte, event *Event) error {
	var stored eventRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	*event = Event{
		ID:          stored.ID,
		EntityID:    stored.EntityID,
		Title:       stored.Title,
		Description: stored.Description,
		OccurredAt:  cloneTimePtr(stored.OccurredAt),
		ValidFrom:   cloneTimePtr(stored.ValidFrom),
		ValidUntil:  cloneTimePtr(stored.ValidUntil),
		FactRefs:    factRefsFromRecords(stored.FactRefs),
		SourceRefs:  sourceRefsFromRecords(stored.SourceRefs),
		Signature:   cloneViewSignature(stored.Signature),
		CreatedAt:   stored.CreatedAt,
		UpdatedAt:   stored.UpdatedAt,
		Metadata:    cloneAnyMap(stored.Metadata),
	}
	return nil
}

func slotRecords(slots []Slot) []slotRecord {
	if slots == nil {
		return nil
	}
	records := make([]slotRecord, len(slots))
	for i, slot := range slots {
		records[i] = slotRecord{
			Name:       slot.Name,
			Value:      slot.Value,
			Confidence: slot.Confidence,
			FactRefs:   factRefRecords(slot.FactRefs),
			Metadata:   cloneAnyMap(slot.Metadata),
		}
	}
	return records
}

func slotsFromRecords(records []slotRecord) []Slot {
	if records == nil {
		return nil
	}
	slots := make([]Slot, len(records))
	for i, record := range records {
		slots[i] = Slot{
			Name:       record.Name,
			Value:      record.Value,
			Confidence: record.Confidence,
			FactRefs:   factRefsFromRecords(record.FactRefs),
			Metadata:   cloneAnyMap(record.Metadata),
		}
	}
	return slots
}

func factRefRecords(refs []fact.FactRef) []factRefRecord {
	if refs == nil {
		return nil
	}
	records := make([]factRefRecord, len(refs))
	for i, ref := range refs {
		records[i] = factRefRecord{
			FactID: ref.FactID,
			Role:   ref.Role,
		}
	}
	return records
}

func factRefsFromRecords(records []factRefRecord) []fact.FactRef {
	if records == nil {
		return nil
	}
	refs := make([]fact.FactRef, len(records))
	for i, record := range records {
		refs[i] = fact.FactRef{
			FactID: record.FactID,
			Role:   record.Role,
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
