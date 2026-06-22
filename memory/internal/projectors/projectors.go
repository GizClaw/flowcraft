package projectors

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/document"
	"github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const errPrefix = "memory/internal/projectors"

const (
	MetadataViewKindKey       = "projector.view_kind"
	MetadataRecordTypeKey     = "projector.record_type"
	MetadataRecordMetadataKey = "projector.record_metadata"

	MetadataDatasetIDKey       = "projector.dataset_id"
	MetadataDocumentIDKey      = "projector.document_id"
	MetadataChunkIDKey         = "projector.chunk_id"
	MetadataConversationIDKey  = "projector.conversation_id"
	MetadataRuntimeIDKey       = "projector.runtime_id"
	MetadataUserIDKey          = "projector.user_id"
	MetadataAgentIDKey         = "projector.agent_id"
	MetadataNodeIDKey          = "projector.node_id"
	MetadataFactIDKey          = "projector.fact_id"
	MetadataSubjectEntityIDKey = "projector.subject_entity_id"
	MetadataObjectEntityIDsKey = "projector.object_entity_ids"
	MetadataRelationTypeKey    = "projector.relation_type"
	MetadataFactTimeTextKey    = "projector.fact_time_text"
	MetadataMessageIDKey       = "projector.message_id"
	MetadataMessageSeqKey      = "projector.message_seq"
	MetadataCreatedAtKey       = "projector.created_at"
	MetadataMessageChunkIndex  = "projector.message_chunk_index"
	MetadataMessageChunkCount  = "projector.message_chunk_count"
	MetadataMessageChunkStart  = "projector.message_chunk_start"
	MetadataMessageChunkEnd    = "projector.message_chunk_end"
)

const (
	RecordTypeDocumentChunk = "document_chunk"
	RecordTypeSummaryNode   = "summary_node"
	RecordTypeSourceMessage = "source_message"
	RecordTypeEntityFact    = "entity_fact"
)

const (
	sourceMessageChunkTargetRunes  = 4000
	sourceMessageChunkOverlapRunes = 400
	sourceMessageChunkMaxRunes     = 7600
)

// SourceMessageRecords converts a canonical source message into one or more
// indexed chunk records. Each chunk embeds independently, while metadata and
// source refs preserve the original message identity for hydration.
func SourceMessageRecords(scope views.Scope, msg sourcemessage.Message) ([]indexed.Record, error) {
	if err := scope.Validate(); err != nil {
		return nil, errdefs.Validationf("%s: invalid message scope: %w", errPrefix, err)
	}
	if strings.TrimSpace(msg.ConversationID) == "" {
		return nil, errdefs.Validationf("%s: message conversation_id is required", errPrefix)
	}
	if strings.TrimSpace(msg.ID) == "" {
		return nil, errdefs.Validationf("%s: message id is required", errPrefix)
	}
	text := sourceMessageText(msg)
	if strings.TrimSpace(text) == "" {
		return nil, errdefs.Validationf("%s: source message text is required", errPrefix)
	}

	sourceRef := views.SourceRef{
		Kind: views.SourceMessage,
		Message: &views.MessageSourceRef{
			ConversationID: msg.ConversationID,
			MessageID:      msg.ID,
		},
	}
	fields := sourceMessageMetadata(scope, msg)
	chunks := sourceMessageChunks(text)
	records := make([]indexed.Record, 0, len(chunks))
	for i, chunk := range chunks {
		chunkFields := cloneAnyMap(fields)
		chunkFields[MetadataMessageChunkIndex] = i
		chunkFields[MetadataMessageChunkCount] = len(chunks)
		chunkFields[MetadataMessageChunkStart] = chunk.Start
		chunkFields[MetadataMessageChunkEnd] = chunk.End
		record, err := validateIndexedRecord(indexed.Record{
			ID:         SourceMessageChunkRecordID(scope.DatasetID, scope.AgentID, msg.ConversationID, msg.ID, i),
			Text:       chunk.Text,
			Metadata:   metadata(views.KindMessageIndex, RecordTypeSourceMessage, msg.Metadata, chunkFields),
			SourceRefs: []views.SourceRef{sourceRef},
		})
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// SourceMessageChunkRecordID returns the stable projection record id for a
// source message chunk.
func SourceMessageChunkRecordID(datasetID, agentID, conversationID, messageID string, chunkIndex int) string {
	return recordID(RecordTypeSourceMessage, datasetID, agentID, conversationID, messageID, "chunk", fmt.Sprintf("%06d", chunkIndex))
}

// DocumentChunk converts a document chunk view record into an indexed record.
func DocumentChunk(chunk document.Chunk) (indexed.Record, error) {
	if err := chunk.Validate(); err != nil {
		return indexed.Record{}, errdefs.Validationf("%s: invalid document chunk: %w", errPrefix, err)
	}
	if strings.TrimSpace(chunk.Text) == "" {
		return indexed.Record{}, errdefs.Validationf("%s: document chunk text is required", errPrefix)
	}

	return validateIndexedRecord(indexed.Record{
		ID:   DocumentChunkRecordID(chunk.Scope.DatasetID, chunk.DocumentID, chunk.ID),
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

// DocumentChunkRecordID returns the projection record id for a document chunk.
// It intentionally depends only on the stable identity fields, so callers can
// clean up stale projections without revalidating the full chunk payload.
func DocumentChunkRecordID(datasetID, documentID string, chunkID document.ChunkID) string {
	return recordID(RecordTypeDocumentChunk, datasetID, documentID, string(chunkID))
}

// SummaryNode converts a recent summary DAG node into an indexed record.
func SummaryNode(node recent.SummaryNode) (indexed.Record, error) {
	if err := validateSummaryNode(node); err != nil {
		return indexed.Record{}, err
	}

	return validateIndexedRecord(indexed.Record{
		ID:   recordID(RecordTypeSummaryNode, node.Scope.AgentID, node.Scope.ConversationID, string(node.ID)),
		Text: node.Summary,
		Metadata: metadata(views.KindSummaryDAG, RecordTypeSummaryNode, node.Metadata, withScopeMetadata(node.Scope, map[string]any{
			MetadataNodeIDKey: string(node.ID),
		})),
		SourceRefs: cloneSourceRefs(node.SourceRefs),
		Signature:  cloneViewSignature(node.Signature),
	})
}

// EntityFact converts one entity-linked fact into an indexed recall record.
func EntityFact(fact entityfact.Fact) (indexed.Record, error) {
	if err := entityfact.ValidateFact(fact); err != nil {
		return indexed.Record{}, errdefs.Validationf("%s: invalid entity fact: %w", errPrefix, err)
	}
	text := entityFactText(fact)
	if strings.TrimSpace(text) == "" {
		return indexed.Record{}, errdefs.Validationf("%s: entity fact text is required", errPrefix)
	}
	return validateIndexedRecord(indexed.Record{
		ID:   EntityFactRecordID(fact.Scope.AgentID, fact.Scope.ConversationID, fact.ID),
		Text: text,
		Metadata: metadata(views.KindEntityFacts, RecordTypeEntityFact, fact.Metadata, withScopeMetadata(fact.Scope, map[string]any{
			MetadataFactIDKey:          string(fact.ID),
			MetadataSubjectEntityIDKey: string(fact.SubjectEntityID),
			MetadataObjectEntityIDsKey: entityIDStrings(fact.ObjectEntityIDs),
			MetadataRelationTypeKey:    string(fact.RelationType),
			MetadataFactTimeTextKey:    fact.TimeText,
		})),
		SourceRefs: cloneSourceRefs(fact.SourceRefs),
	})
}

// EntityFactRecordID returns the stable projection record id for a fact.
func EntityFactRecordID(agentID, conversationID string, factID entityfact.FactID) string {
	return recordID(RecordTypeEntityFact, agentID, conversationID, string(factID))
}

func validateIndexedRecord(record indexed.Record) (indexed.Record, error) {
	if err := record.Validate(); err != nil {
		return indexed.Record{}, errdefs.Validationf("%s: invalid indexed record: %w", errPrefix, err)
	}
	return record, nil
}

func sourceMessageText(msg sourcemessage.Message) string {
	var lines []string
	if role := strings.TrimSpace(string(msg.Role)); role != "" {
		lines = append(lines, "role: "+role)
	}
	if content := strings.TrimSpace(msg.Content()); content != "" {
		lines = append(lines, "content: "+content)
	}
	for _, part := range msg.Parts {
		if part.Data == nil || len(part.Data.Value) == 0 {
			continue
		}
		summary := jsonObjectSummary(part.Data.Value)
		if summary == "" {
			continue
		}
		label := "data"
		if mime := strings.TrimSpace(part.Data.MimeType); mime != "" {
			label += "(" + mime + ")"
		}
		lines = append(lines, label+": "+summary)
	}
	if summary := jsonObjectSummary(msg.Metadata); summary != "" {
		lines = append(lines, "metadata: "+summary)
	}
	return strings.Join(lines, "\n")
}

func entityFactText(fact entityfact.Fact) string {
	var lines []string
	lines = append(lines, "fact: "+fact.FactText)
	lines = append(lines, "subject_entity_id: "+string(fact.SubjectEntityID))
	if fact.PredicateText != "" {
		lines = append(lines, "predicate: "+fact.PredicateText)
	}
	if len(fact.ObjectEntityIDs) > 0 {
		lines = append(lines, "object_entity_ids: "+strings.Join(entityIDStrings(fact.ObjectEntityIDs), ", "))
	}
	if fact.RelationType != "" {
		lines = append(lines, "relation_type: "+string(fact.RelationType))
	}
	if fact.TimeText != "" {
		lines = append(lines, "time: "+fact.TimeText)
	}
	return strings.Join(lines, "\n")
}

func entityIDStrings(ids []entityfact.EntityID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, string(id))
		}
	}
	return out
}

type sourceMessageChunk struct {
	Text       string
	Start, End int
}

func sourceMessageChunks(text string) []sourceMessageChunk {
	runes := []rune(text)
	if len(runes) <= sourceMessageChunkTargetRunes {
		return []sourceMessageChunk{{Text: text, Start: 0, End: len(runes)}}
	}
	var chunks []sourceMessageChunk
	for start := 0; start < len(runes); {
		maxEnd := min(start+sourceMessageChunkMaxRunes, len(runes))
		targetEnd := min(start+sourceMessageChunkTargetRunes, maxEnd)
		end := sourceMessageChunkBoundary(runes, start, targetEnd, maxEnd)
		if end <= start {
			end = targetEnd
		}
		chunkText := strings.TrimSpace(string(runes[start:end]))
		if chunkText != "" {
			chunks = append(chunks, sourceMessageChunk{Text: chunkText, Start: start, End: end})
		}
		if end >= len(runes) {
			break
		}
		nextStart := end - sourceMessageChunkOverlapRunes
		if nextStart <= start {
			nextStart = end
		}
		start = nextStart
	}
	return chunks
}

func sourceMessageChunkBoundary(runes []rune, start, targetEnd, maxEnd int) int {
	minEnd := start + sourceMessageChunkTargetRunes/2
	if minEnd > targetEnd {
		minEnd = targetEnd
	}
	for _, boundary := range []string{"\n\n", "\n"} {
		if end := lastBoundary(runes, minEnd, targetEnd, boundary); end > start {
			return end
		}
	}
	for end := targetEnd; end > minEnd; end-- {
		switch runes[end-1] {
		case '.', '!', '?', '。', '！', '？':
			return end
		}
	}
	for end := targetEnd; end > minEnd; end-- {
		if runes[end-1] == ' ' || runes[end-1] == '\t' {
			return end
		}
	}
	return min(targetEnd, maxEnd)
}

func lastBoundary(runes []rune, minEnd, maxEnd int, boundary string) int {
	needle := []rune(boundary)
	if len(needle) == 0 || maxEnd-len(needle) < minEnd {
		return 0
	}
	for end := maxEnd; end >= minEnd+len(needle); end-- {
		start := end - len(needle)
		matched := true
		for i := range needle {
			if runes[start+i] != needle[i] {
				matched = false
				break
			}
		}
		if matched {
			return end
		}
	}
	return 0
}

func sourceMessageMetadata(scope views.Scope, msg sourcemessage.Message) map[string]any {
	fields := cloneAnyMap(msg.Metadata)
	if fields == nil {
		fields = map[string]any{}
	}
	mergeSourceMessageDataMetadata(fields, msg)
	fields[MetadataMessageIDKey] = msg.ID
	fields[MetadataMessageSeqKey] = msg.Seq
	fields[MetadataConversationIDKey] = msg.ConversationID
	fields["message_id"] = msg.ID
	fields["seq"] = msg.Seq
	if !msg.CreatedAt.IsZero() {
		createdAt := msg.CreatedAt.UTC().Format(time.RFC3339Nano)
		fields[MetadataCreatedAtKey] = createdAt
		fields["created_at"] = createdAt
	}
	return withScopeMetadata(scope, fields)
}

func mergeSourceMessageDataMetadata(fields map[string]any, msg sourcemessage.Message) {
	for _, part := range msg.Parts {
		if part.Data == nil {
			continue
		}
		for key, value := range part.Data.Value {
			if _, exists := fields[key]; exists {
				continue
			}
			fields[key] = cloneAny(value)
		}
	}
	copyMetadataAlias(fields, "session_index", "session")
	copyMetadataAlias(fields, "speaker_name", "speaker")
	copyMetadataAlias(fields, "image_caption", "blip_caption")
	copyMetadataAlias(fields, "image_caption", "caption")
	copyMetadataAlias(fields, "image_query", "query")
}

func copyMetadataAlias(fields map[string]any, from, to string) {
	if _, exists := fields[to]; exists {
		return
	}
	value, ok := fields[from]
	if !ok {
		return
	}
	fields[to] = cloneAny(value)
}

func jsonObjectSummary(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	clean := cloneAnyMap(in)
	if len(clean) == 0 {
		return ""
	}
	raw, err := json.Marshal(clean)
	if err == nil {
		return string(raw)
	}
	keys := make([]string, 0, len(clean))
	for key := range clean {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+fmt.Sprint(clean[key]))
	}
	return strings.Join(parts, " ")
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
	return out
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

func validateSignature(name string, signature views.ViewSignature) error {
	if err := signature.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid %s signature: %w", errPrefix, name, err)
	}
	return nil
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
		out.SourceRevisions = append([]views.SourceRevision(nil), in.SourceRevisions...)
	}
	if in.UpstreamViewRefs != nil {
		out.UpstreamViewRefs = append([]views.UpstreamViewRef(nil), in.UpstreamViewRefs...)
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
