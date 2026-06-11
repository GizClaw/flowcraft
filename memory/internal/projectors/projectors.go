package projectors

import (
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const errPrefix = "memory/internal/projectors"

const (
	MetadataViewKindKey       = "projector.view_kind"
	MetadataRecordTypeKey     = "projector.record_type"
	MetadataRecordMetadataKey = "projector.record_metadata"

	MetadataDatasetIDKey      = "projector.dataset_id"
	MetadataDocumentIDKey     = "projector.document_id"
	MetadataChunkIDKey        = "projector.chunk_id"
	MetadataConversationIDKey = "projector.conversation_id"
	MetadataRuntimeIDKey      = "projector.runtime_id"
	MetadataUserIDKey         = "projector.user_id"
	MetadataAgentIDKey        = "projector.agent_id"
	MetadataNodeIDKey         = "projector.node_id"
	MetadataObservationIDKey  = "projector.observation_id"
	MetadataFactIDKey         = "projector.fact_id"
	MetadataEdgeIDKey         = "projector.edge_id"
	MetadataProfileIDKey      = "projector.profile_id"
	MetadataEventIDKey        = "projector.event_id"
	MetadataEntityIDKey       = "projector.entity_id"
	MetadataNodeKindKey       = "projector.node_kind"
	MetadataFromKey           = "projector.from"
	MetadataToKey             = "projector.to"
	MetadataPredicateKey      = "projector.predicate"
	MetadataStatusKey         = "projector.status"
)

const (
	RecordTypeDocumentChunk = "document_chunk"
	RecordTypeSummaryNode   = "summary_node"
	RecordTypeObservation   = "observation"
	RecordTypeFact          = "fact"
	RecordTypeFactNode      = "fact_node"
	RecordTypeFactEdge      = "fact_edge"
	RecordTypeEntityProfile = "entity_profile"
	RecordTypeEntityEvent   = "entity_event"
)

// DocumentChunk converts a document chunk semantic view record into an indexed record.
func DocumentChunk(chunk document.Chunk) (indexed.Record, error) {
	if err := chunk.Validate(); err != nil {
		return indexed.Record{}, errdefs.Validationf("%s: invalid document chunk: %w", errPrefix, err)
	}
	if strings.TrimSpace(chunk.Text) == "" {
		return indexed.Record{}, errdefs.Validationf("%s: document chunk text is required", errPrefix)
	}

	return validateIndexedRecord(indexed.Record{
		ID:   recordID(RecordTypeDocumentChunk, chunk.Scope.DatasetID, chunk.DocumentID, string(chunk.ID)),
		Text: chunk.Text,
		Metadata: metadata(
			views.KindDocumentChunks,
			RecordTypeDocumentChunk,
			chunk.Metadata,
			withScopeMetadata(chunk.Scope, map[string]any{
				MetadataDocumentIDKey: chunk.DocumentID,
				MetadataChunkIDKey:    string(chunk.ID),
			}),
		),
		SourceRefs: []views.SourceRef{cloneSourceRef(chunk.SourceRef)},
		Signature:  cloneViewSignature(chunk.Signature),
	})
}

// SummaryNode converts a recent summary DAG node into an indexed record.
func SummaryNode(node recent.SummaryNode) (indexed.Record, error) {
	if err := validateSummaryNode(node); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   recordID(RecordTypeSummaryNode, node.Scope.ConversationID, string(node.ID)),
		Text: node.Summary,
		Metadata: metadata(views.KindSummaryDAG, RecordTypeSummaryNode, node.Metadata, withScopeMetadata(node.Scope, map[string]any{
			MetadataNodeIDKey: string(node.ID),
		})),
		SourceRefs: cloneSourceRefs(node.SourceRefs),
		Signature:  cloneViewSignature(node.Signature),
	})
}

// Observation converts an observation ledger record into an indexed record.
func Observation(obs observation.Observation) (indexed.Record, error) {
	if err := validateObservation(obs); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   "observation:" + obs.ID,
		Text: observationText(obs),
		Metadata: metadata(views.KindObservationLedger, RecordTypeObservation, obs.Metadata, withScopeMetadata(obs.Scope, map[string]any{
			MetadataObservationIDKey: obs.ID,
			MetadataPredicateKey:     obs.Predicate,
		})),
		SourceRefs: cloneSourceRefs(obs.SourceRefs),
		Signature:  cloneViewSignature(obs.Signature),
	})
}

// FactRecord converts a fact ledger record into an indexed record.
func FactRecord(record fact.Fact) (indexed.Record, error) {
	record = normalizeFact(record)
	if err := validateFact(record); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   "fact:" + string(record.ID),
		Text: factText(record),
		Metadata: metadata(views.KindFactLedger, RecordTypeFact, record.Metadata, withScopeMetadata(record.Scope, map[string]any{
			MetadataFactIDKey:    string(record.ID),
			MetadataPredicateKey: record.Predicate,
			MetadataStatusKey:    string(record.Status),
		})),
		SourceRefs: cloneSourceRefs(record.SourceRefs),
		Signature:  cloneViewSignature(record.Signature),
	})
}

// FactNode converts a fact graph node into an indexed record.
func FactNode(node fact.Node) (indexed.Record, error) {
	node = normalizeNode(node)
	if err := validateFactNode(node); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   "fact_node:" + string(node.ID),
		Text: factNodeText(node),
		Metadata: metadata(views.KindFactGraph, RecordTypeFactNode, node.Metadata, withScopeMetadata(node.Scope, map[string]any{
			MetadataNodeIDKey:   string(node.ID),
			MetadataNodeKindKey: string(node.Kind),
		})),
		SourceRefs: cloneSourceRefs(node.SourceRefs),
		Signature:  cloneViewSignature(node.Signature),
	})
}

// FactEdge converts a fact graph edge into an indexed record.
func FactEdge(edge fact.Edge) (indexed.Record, error) {
	edge = normalizeEdge(edge)
	if err := validateFactEdge(edge); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   "fact_edge:" + string(edge.ID),
		Text: factEdgeText(edge),
		Metadata: metadata(views.KindFactGraph, RecordTypeFactEdge, edge.Metadata, withScopeMetadata(edge.Scope, map[string]any{
			MetadataEdgeIDKey:    string(edge.ID),
			MetadataFromKey:      string(edge.From),
			MetadataToKey:        string(edge.To),
			MetadataPredicateKey: edge.Predicate,
			MetadataStatusKey:    string(edge.Status),
		})),
		SourceRefs: cloneSourceRefs(edge.SourceRefs),
		Signature:  cloneViewSignature(edge.Signature),
	})
}

// EntityProfile converts an entity profile record into an indexed record.
func EntityProfile(record entity.ProfileRecord) (indexed.Record, error) {
	if err := validateEntityProfile(record); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   "entity_profile:" + string(record.ID),
		Text: entityProfileText(record),
		Metadata: metadata(views.KindEntityProfile, RecordTypeEntityProfile, record.Metadata, withScopeMetadata(record.Scope, map[string]any{
			MetadataProfileIDKey: string(record.ID),
		})),
		SourceRefs: cloneSourceRefs(record.SourceRefs),
		Signature:  cloneViewSignature(record.Signature),
	})
}

// EntityEvent converts an entity timeline event into an indexed record.
func EntityEvent(event entity.Event) (indexed.Record, error) {
	if err := validateEntityEvent(event); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   "entity_event:" + string(event.ID),
		Text: entityEventText(event),
		Metadata: metadata(views.KindEntityTimeline, RecordTypeEntityEvent, event.Metadata, withScopeMetadata(event.Scope, map[string]any{
			MetadataEventIDKey: string(event.ID),
		})),
		SourceRefs: cloneSourceRefs(event.SourceRefs),
		Signature:  cloneViewSignature(event.Signature),
	})
}

func validateIndexedRecord(record indexed.Record) (indexed.Record, error) {
	if err := record.Validate(); err != nil {
		return indexed.Record{}, errdefs.Validationf("%s: invalid indexed record: %w", errPrefix, err)
	}
	return record, nil
}

func recordID(prefix string, parts ...string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, part := range parts {
		b.WriteByte(':')
		b.WriteString(encodedIDPart(part))
	}
	return b.String()
}

func encodedIDPart(part string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(part))
}

func metadata(viewKind views.Kind, recordType string, recordMetadata map[string]any, fields map[string]any) map[string]any {
	out := map[string]any{
		MetadataViewKindKey:   string(viewKind),
		MetadataRecordTypeKey: recordType,
	}
	maps.Copy(out, fields)
	if recordMetadata != nil {
		out[MetadataRecordMetadataKey] = cloneAnyMap(recordMetadata)
	}
	return out
}

func withScopeMetadata(scope views.Scope, fields map[string]any) map[string]any {
	out := maps.Clone(fields)
	out[MetadataRuntimeIDKey] = scope.RuntimeID
	out[MetadataUserIDKey] = scope.UserID
	out[MetadataAgentIDKey] = scope.AgentID
	out[MetadataConversationIDKey] = scope.ConversationID
	out[MetadataDatasetIDKey] = scope.DatasetID
	out[MetadataEntityIDKey] = scope.EntityID
	return out
}

func observationText(obs observation.Observation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Observation: %s %s %s", obs.Subject, obs.Predicate, obs.Object)
	fmt.Fprintf(&b, "\nScope: runtime=%s user=%s", obs.Scope.RuntimeID, obs.Scope.UserID)
	if obs.Scope.AgentID != "" {
		fmt.Fprintf(&b, " agent=%s", obs.Scope.AgentID)
	}
	if obs.Confidence != 0 {
		fmt.Fprintf(&b, "\nConfidence: %g", obs.Confidence)
	}
	return b.String()
}

func factText(record fact.Fact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fact: %s %s %s", record.Subject, record.Predicate, record.Object)
	fmt.Fprintf(&b, "\nStatus: %s", record.Status)
	if record.Confidence != 0 {
		fmt.Fprintf(&b, "\nConfidence: %g", record.Confidence)
	}
	appendValidity(&b, record.ValidFrom, record.ValidUntil)
	return b.String()
}

func factNodeText(node fact.Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Node: %s", node.Label)
	fmt.Fprintf(&b, "\nKind: %s", node.Kind)
	if len(node.Aliases) > 0 {
		fmt.Fprintf(&b, "\nAliases: %s", strings.Join(node.Aliases, ", "))
	}
	return b.String()
}

func factEdgeText(edge fact.Edge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Edge: %s %s %s", edge.From, edge.Predicate, edge.To)
	fmt.Fprintf(&b, "\nStatus: %s", edge.Status)
	if edge.Confidence != 0 {
		fmt.Fprintf(&b, "\nConfidence: %g", edge.Confidence)
	}
	appendValidity(&b, edge.ValidFrom, edge.ValidUntil)
	return b.String()
}

func entityProfileText(record entity.ProfileRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Profile: %s", record.Label)
	if record.Summary != "" {
		fmt.Fprintf(&b, "\nSummary: %s", record.Summary)
	}
	if len(record.Slots) > 0 {
		b.WriteString("\nSlots:")
		for _, slot := range record.Slots {
			fmt.Fprintf(&b, "\n%s: %s", slot.Name, slot.Value)
		}
	}
	return b.String()
}

func entityEventText(event entity.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Event: %s", event.Title)
	if event.Description != "" {
		fmt.Fprintf(&b, "\nDescription: %s", event.Description)
	}
	if event.OccurredAt != nil {
		fmt.Fprintf(&b, "\nOccurredAt: %s", formatTime(*event.OccurredAt))
	}
	appendValidity(&b, event.ValidFrom, event.ValidUntil)
	return b.String()
}

func appendValidity(b *strings.Builder, validFrom, validUntil *time.Time) {
	if validFrom != nil {
		fmt.Fprintf(b, "\nValidFrom: %s", formatTime(*validFrom))
	}
	if validUntil != nil {
		fmt.Fprintf(b, "\nValidUntil: %s", formatTime(*validUntil))
	}
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func validateSummaryNode(node recent.SummaryNode) error {
	if node.ID == "" {
		return errdefs.Validationf("%s: summary node id is required", errPrefix)
	}
	if err := node.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid summary scope: %w", errPrefix, err)
	}
	if node.Scope.ConversationID == "" {
		return errdefs.Validationf("%s: conversation_id is required", errPrefix)
	}
	if strings.TrimSpace(node.Summary) == "" {
		return errdefs.Validationf("%s: summary is required", errPrefix)
	}
	if node.Level < 0 {
		return errdefs.Validationf("%s: summary level must be non-negative", errPrefix)
	}
	if len(node.SourceRefs) == 0 {
		return errdefs.Validationf("%s: summary source_refs are required", errPrefix)
	}
	for i, ref := range node.SourceRefs {
		if err := ref.Validate(); err != nil {
			return errdefs.Validationf("%s: invalid summary source_refs[%d]: %w", errPrefix, i, err)
		}
		if ref.Kind != views.SourceMessage {
			return errdefs.Validationf("%s: summary source_refs[%d] must reference messages", errPrefix, i)
		}
	}
	if len(node.Signature.SourceRevisions) == 0 {
		return errdefs.Validationf("%s: summary source revisions are required", errPrefix)
	}
	for i, rev := range node.Signature.SourceRevisions {
		if rev.Kind != views.SourceMessage {
			return errdefs.Validationf("%s: summary source revisions[%d] must reference messages", errPrefix, i)
		}
	}
	if len(node.Signature.UpstreamViewRefs) > 0 {
		return errdefs.Validationf("%s: summary upstream view refs are not part of lineage", errPrefix)
	}
	return validateSignature("summary", node.Signature)
}

func validateObservation(obs observation.Observation) error {
	if obs.ID == "" {
		return errdefs.Validationf("%s: observation id is required", errPrefix)
	}
	if err := obs.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid observation scope: %w", errPrefix, err)
	}
	if strings.TrimSpace(obs.Subject) == "" {
		return errdefs.Validationf("%s: observation subject is required", errPrefix)
	}
	if strings.TrimSpace(obs.Predicate) == "" {
		return errdefs.Validationf("%s: observation predicate is required", errPrefix)
	}
	if strings.TrimSpace(obs.Object) == "" {
		return errdefs.Validationf("%s: observation object is required", errPrefix)
	}
	if obs.Confidence != 0 && (obs.Confidence < 0 || obs.Confidence > 1) {
		return errdefs.Validationf("%s: observation confidence must be between 0 and 1", errPrefix)
	}
	if len(obs.SourceRefs) == 0 {
		return errdefs.Validationf("%s: observation source_refs are required", errPrefix)
	}
	if err := validateSourceRefs("observation", obs.SourceRefs); err != nil {
		return err
	}
	if len(obs.Signature.SourceRevisions) == 0 {
		return errdefs.Validationf("%s: observation source revisions are required", errPrefix)
	}
	if len(obs.Signature.UpstreamViewRefs) > 0 {
		return errdefs.Validationf("%s: observation upstream view refs are not part of lineage", errPrefix)
	}
	return validateSignature("observation", obs.Signature)
}

func validateFact(record fact.Fact) error {
	if record.ID == "" {
		return errdefs.Validationf("%s: fact id is required", errPrefix)
	}
	if err := record.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact scope: %w", errPrefix, err)
	}
	if strings.TrimSpace(record.Subject) == "" {
		return errdefs.Validationf("%s: fact subject is required", errPrefix)
	}
	if strings.TrimSpace(record.Predicate) == "" {
		return errdefs.Validationf("%s: fact predicate is required", errPrefix)
	}
	if strings.TrimSpace(record.Object) == "" {
		return errdefs.Validationf("%s: fact object is required", errPrefix)
	}
	if err := record.Status.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact status: %w", errPrefix, err)
	}
	if record.Confidence != 0 && (record.Confidence < 0 || record.Confidence > 1) {
		return errdefs.Validationf("%s: fact confidence must be between 0 and 1", errPrefix)
	}
	if err := validateValidity("fact", record.ValidFrom, record.ValidUntil); err != nil {
		return err
	}
	if len(record.ObservationRefs) == 0 {
		return errdefs.Validationf("%s: fact observation_refs are required", errPrefix)
	}
	for i, ref := range record.ObservationRefs {
		if ref.ObservationID == "" {
			return errdefs.Validationf("%s: fact observation_refs[%d] observation id is required", errPrefix, i)
		}
		if (ref.ScopeKind == "") != (ref.ScopeID == "") {
			return errdefs.Validationf("%s: fact observation_refs[%d] scope kind and id must be provided together", errPrefix, i)
		}
	}
	if err := validateSourceRefs("fact", record.SourceRefs); err != nil {
		return err
	}
	return validateUpstreamSignature("fact", record.Signature)
}

func validateFactNode(node fact.Node) error {
	if node.ID == "" {
		return errdefs.Validationf("%s: fact node id is required", errPrefix)
	}
	if err := node.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact node scope: %w", errPrefix, err)
	}
	switch node.Kind {
	case fact.NodeEntity, fact.NodeValue:
	default:
		return errdefs.Validationf("%s: unsupported fact node kind %q", errPrefix, node.Kind)
	}
	if strings.TrimSpace(node.Label) == "" {
		return errdefs.Validationf("%s: fact node label is required", errPrefix)
	}
	if err := validateFactRefs("fact node", node.FactRefs); err != nil {
		return err
	}
	if err := validateSourceRefs("fact node", node.SourceRefs); err != nil {
		return err
	}
	return validateUpstreamSignature("fact node", node.Signature)
}

func validateFactEdge(edge fact.Edge) error {
	if edge.ID == "" {
		return errdefs.Validationf("%s: fact edge id is required", errPrefix)
	}
	if err := edge.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact edge scope: %w", errPrefix, err)
	}
	if edge.From == "" {
		return errdefs.Validationf("%s: fact edge from is required", errPrefix)
	}
	if edge.To == "" {
		return errdefs.Validationf("%s: fact edge to is required", errPrefix)
	}
	if strings.TrimSpace(edge.Predicate) == "" {
		return errdefs.Validationf("%s: fact edge predicate is required", errPrefix)
	}
	if err := edge.Status.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid fact edge status: %w", errPrefix, err)
	}
	if edge.Confidence != 0 && (edge.Confidence < 0 || edge.Confidence > 1) {
		return errdefs.Validationf("%s: fact edge confidence must be between 0 and 1", errPrefix)
	}
	if err := validateValidity("fact edge", edge.ValidFrom, edge.ValidUntil); err != nil {
		return err
	}
	if err := validateFactRefs("fact edge", edge.FactRefs); err != nil {
		return err
	}
	if err := validateSourceRefs("fact edge", edge.SourceRefs); err != nil {
		return err
	}
	return validateUpstreamSignature("fact edge", edge.Signature)
}

func validateEntityProfile(record entity.ProfileRecord) error {
	if record.ID == "" {
		return errdefs.Validationf("%s: entity profile id is required", errPrefix)
	}
	if err := record.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid entity profile scope: %w", errPrefix, err)
	}
	if record.Scope.EntityID == "" {
		return errdefs.Validationf("%s: entity profile entity id is required", errPrefix)
	}
	if strings.TrimSpace(record.Label) == "" {
		return errdefs.Validationf("%s: entity profile label is required", errPrefix)
	}
	if err := validateFactRefs("entity profile", record.FactRefs); err != nil {
		return err
	}
	for i, slot := range record.Slots {
		if strings.TrimSpace(slot.Name) == "" {
			return errdefs.Validationf("%s: entity profile slots[%d] name is required", errPrefix, i)
		}
		if strings.TrimSpace(slot.Value) == "" {
			return errdefs.Validationf("%s: entity profile slots[%d] value is required", errPrefix, i)
		}
		if slot.Confidence != 0 && (slot.Confidence < 0 || slot.Confidence > 1) {
			return errdefs.Validationf("%s: entity profile slots[%d] confidence must be between 0 and 1", errPrefix, i)
		}
		if len(slot.FactRefs) > 0 {
			if err := validateFactRefs(fmt.Sprintf("entity profile slots[%d]", i), slot.FactRefs); err != nil {
				return err
			}
		}
	}
	if err := validateSourceRefs("entity profile", record.SourceRefs); err != nil {
		return err
	}
	return validateUpstreamSignature("entity profile", record.Signature)
}

func validateEntityEvent(event entity.Event) error {
	if event.ID == "" {
		return errdefs.Validationf("%s: entity event id is required", errPrefix)
	}
	if err := event.Scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid entity event scope: %w", errPrefix, err)
	}
	if event.Scope.EntityID == "" {
		return errdefs.Validationf("%s: entity event entity id is required", errPrefix)
	}
	if strings.TrimSpace(event.Title) == "" {
		return errdefs.Validationf("%s: entity event title is required", errPrefix)
	}
	if err := validateValidity("entity event", event.ValidFrom, event.ValidUntil); err != nil {
		return err
	}
	if err := validateFactRefs("entity event", event.FactRefs); err != nil {
		return err
	}
	if err := validateSourceRefs("entity event", event.SourceRefs); err != nil {
		return err
	}
	return validateUpstreamSignature("entity event", event.Signature)
}

func validateFactRefs(name string, refs []fact.FactRef) error {
	if len(refs) == 0 {
		return errdefs.Validationf("%s: %s fact_refs are required", errPrefix, name)
	}
	for i, ref := range refs {
		if ref.FactID == "" {
			return errdefs.Validationf("%s: %s fact_refs[%d] fact id is required", errPrefix, name, i)
		}
	}
	return nil
}

func validateSourceRefs(name string, refs []views.SourceRef) error {
	for i, ref := range refs {
		if err := ref.Validate(); err != nil {
			return errdefs.Validationf("%s: invalid %s source_refs[%d]: %w", errPrefix, name, i, err)
		}
	}
	return nil
}

func validateSignature(name string, signature views.ViewSignature) error {
	if err := signature.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid %s signature: %w", errPrefix, name, err)
	}
	return nil
}

func validateUpstreamSignature(name string, signature views.ViewSignature) error {
	if signature.IsZero() {
		return errdefs.Validationf("%s: %s signature is required", errPrefix, name)
	}
	if len(signature.UpstreamViewRefs) == 0 {
		return errdefs.Validationf("%s: %s upstream view refs are required", errPrefix, name)
	}
	return validateSignature(name, signature)
}

func validateValidity(name string, validFrom, validUntil *time.Time) error {
	if validFrom != nil && validUntil != nil && validUntil.Before(*validFrom) {
		return errdefs.Validationf("%s: %s valid_until must be greater than or equal to valid_from", errPrefix, name)
	}
	return nil
}

func normalizeFact(record fact.Fact) fact.Fact {
	if record.Status == "" {
		record.Status = fact.FactActive
	}
	return record
}

func normalizeNode(node fact.Node) fact.Node {
	if node.Kind == "" {
		node.Kind = fact.NodeEntity
	}
	return node
}

func normalizeEdge(edge fact.Edge) fact.Edge {
	if edge.Status == "" {
		edge.Status = fact.FactActive
	}
	return edge
}

func cloneSourceRefs(in []views.SourceRef) []views.SourceRef {
	if in == nil {
		return nil
	}
	out := make([]views.SourceRef, len(in))
	for i, ref := range in {
		out[i] = cloneSourceRef(ref)
	}
	return out
}

func cloneSourceRef(ref views.SourceRef) views.SourceRef {
	if ref.Message != nil {
		msg := *ref.Message
		if msg.Span != nil {
			span := *msg.Span
			msg.Span = &span
		}
		ref.Message = &msg
	}
	if ref.Document != nil {
		doc := *ref.Document
		if doc.Span != nil {
			span := *doc.Span
			doc.Span = &span
		}
		ref.Document = &doc
	}
	return ref
}

func cloneViewSignature(in views.ViewSignature) views.ViewSignature {
	out := in
	if in.SourceRevisions != nil {
		out.SourceRevisions = slices.Clone(in.SourceRevisions)
	}
	if in.UpstreamViewRefs != nil {
		out.UpstreamViewRefs = slices.Clone(in.UpstreamViewRefs)
	}
	if in.DiagnosticSignatures != nil {
		out.DiagnosticSignatures = maps.Clone(in.DiagnosticSignatures)
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneAny(value)
	}
	return out
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneAny(item)
		}
		return out
	default:
		return value
	}
}
