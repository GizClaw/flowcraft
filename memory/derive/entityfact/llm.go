package entityfact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	sdkllm "github.com/GizClaw/flowcraft/sdk/llm"
)

const (
	llmAlgorithm = "entity_fact_llm"
	llmVersion   = "v3"

	defaultMaxMessagesPerCall = 16
)

func (e AgentExtractor) extractEntityChunk(ctx context.Context, scope views.Scope, messages []sourcemessage.Message) (llmEntityFactResponse, error) {
	return e.extractChunk(ctx, scope, messages, llmPromptPayload{
		ConversationID: scope.ConversationID,
		Task:           "conversation_source_message_entity_extraction",
		MessageCount:   len(messages),
		SourceIDs:      sourceMessageIDs(messages),
	}, llmEntitySystemPrompt())
}

func (e AgentExtractor) extractFactChunk(ctx context.Context, scope views.Scope, messages []sourcemessage.Message, catalog []llmEntityCatalogEntry) (llmEntityFactResponse, error) {
	if len(catalog) == 0 {
		return llmEntityFactResponse{}, nil
	}
	return e.extractChunk(ctx, scope, messages, llmPromptPayload{
		ConversationID: scope.ConversationID,
		Task:           "conversation_source_message_fact_extraction",
		MessageCount:   len(messages),
		SourceIDs:      sourceMessageIDs(messages),
		EntityCatalog:  catalog,
	}, llmFactSystemPrompt())
}

func (e AgentExtractor) extractCoverageRepairChunk(ctx context.Context, scope views.Scope, messages []sourcemessage.Message, catalog []llmEntityCatalogEntry) (llmEntityFactResponse, error) {
	if len(catalog) == 0 || len(messages) == 0 {
		return llmEntityFactResponse{}, nil
	}
	return e.extractChunk(ctx, scope, messages, llmPromptPayload{
		ConversationID: scope.ConversationID,
		Task:           "conversation_source_message_fact_coverage_repair",
		MessageCount:   len(messages),
		SourceIDs:      sourceMessageIDs(messages),
		EntityCatalog:  catalog,
	}, llmCoverageRepairSystemPrompt())
}

func (e AgentExtractor) extractChunk(ctx context.Context, scope views.Scope, messages []sourcemessage.Message, payload llmPromptPayload, systemPrompt string) (llmEntityFactResponse, error) {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}
	if payload.ConversationID == "" {
		payload.ConversationID = scope.ConversationID
	}
	if payload.MessageCount == 0 {
		payload.MessageCount = len(messages)
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return llmEntityFactResponse{}, err
	}
	llmMessages := []sdkllm.Message{
		sdkllm.NewTextMessage(sdkllm.RoleSystem, systemPrompt),
		sdkllm.NewTextMessage(sdkllm.RoleUser, string(data)),
	}
	for _, msg := range messages {
		llmMessages = append(llmMessages, sourcemessage.PromptMessageFromSource(msg))
	}
	resp, _, err := e.LLM.Generate(ctx, llmMessages, sdkllm.WithJSONMode(true), sdkllm.WithTemperature(0))
	if err != nil {
		return llmEntityFactResponse{}, nil
	}
	parsed, err := parseLLMEntityFactResponse(resp.Content())
	if err != nil {
		return llmEntityFactResponse{}, nil
	}
	return parsed, nil
}

func (e AgentExtractor) materializeEntities(proposals []llmEntity, refByMessageID map[string]views.SourceRef, resolver *entityResolver) []viewentityfact.Entity {
	now := time.Now().UTC()
	var out []viewentityfact.Entity
	for _, proposal := range proposals {
		refs := refsForSourceIDs(proposal.SourceIDs, refByMessageID)
		if len(refs) == 0 {
			continue
		}
		entity := resolver.resolve(proposal.Name, proposal, refs, now)
		if entity == nil {
			continue
		}
		if resolver.isNew(entity.ID) {
			out = append(out, *entity)
			resolver.markStored(entity.ID)
		}
	}
	return out
}

func (e AgentExtractor) materializeFacts(input derive.EntityFactInput, proposals []llmFact, refByMessageID map[string]views.SourceRef, sourceByID map[string]sourcemessage.Message, resolver *entityResolver, seenFactIDs map[viewentityfact.FactID]bool) []viewentityfact.Fact {
	now := time.Now().UTC()
	var out []viewentityfact.Fact
	for _, proposal := range proposals {
		refs := refsForSourceIDs(proposal.SourceIDs, refByMessageID)
		if len(refs) == 0 {
			continue
		}
		subject := resolver.lookup(proposal.Subject)
		if subject == nil {
			continue
		}
		var objects []viewentityfact.EntityID
		for _, name := range proposal.ObjectNames {
			object := resolver.lookup(name)
			if object == nil {
				continue
			}
			objects = append(objects, object.ID)
		}

		factText := strings.TrimSpace(proposal.FactText)
		if factText == "" {
			factText = renderFactText(proposal.Subject, proposal.PredicateText, proposal.ObjectNames, proposal.TimeText)
		}
		if factText == "" {
			continue
		}
		objectSpans := validatedObjectSpans(proposal.ObjectSpans, proposal.SourceIDs, sourceByID)
		fact := viewentityfact.Fact{
			ID:              stableFactID(input.Scope, factText, refs),
			Scope:           input.Scope,
			SubjectEntityID: subject.ID,
			ObjectEntityIDs: uniqueEntityIDs(objects),
			RelationType:    normalizeRelationType(proposal.RelationType),
			PredicateText:   strings.TrimSpace(proposal.PredicateText),
			FactText:        factText,
			TimeText:        strings.TrimSpace(proposal.TimeText),
			SourceRefs:      refs,
			Confidence:      clampConfidence(proposal.Confidence),
			CreatedAt:       now,
			UpdatedAt:       now,
			Metadata: map[string]any{
				"algorithm":           llmAlgorithm,
				"version":             llmVersion,
				"source_message_ids":  append([]string(nil), proposal.SourceIDs...),
				"transform_signature": e.transformSignature(),
			},
		}
		if len(objectSpans) > 0 {
			fact.Metadata[viewentityfact.FactObjectSpansMetadataKey] = objectSpans
		}
		fact.Metadata[viewentityfact.FactGraphableMetadataKey] = viewentityfact.IsGraphableFact(fact)
		if seenFactIDs[fact.ID] {
			continue
		}
		if err := viewentityfact.ValidateFact(fact); err != nil {
			continue
		}
		seenFactIDs[fact.ID] = true
		out = append(out, fact)
	}
	return out
}

func (e AgentExtractor) messageChunks(messages []sourcemessage.Message) [][]sourcemessage.Message {
	max := e.MaxMessagesPerCall
	if max <= 0 {
		max = defaultMaxMessagesPerCall
	}
	var out [][]sourcemessage.Message
	for start := 0; start < len(messages); start += max {
		end := min(start+max, len(messages))
		out = append(out, messages[start:end])
	}
	return out
}

func (e AgentExtractor) transformSignature() string {
	return fmt.Sprintf("%s:%s:max_messages=%d", llmAlgorithm, llmVersion, maxPositive(e.MaxMessagesPerCall, defaultMaxMessagesPerCall))
}

type entityResolver struct {
	scope      views.Scope
	byAlias    map[string]*viewentityfact.Entity
	stored     map[viewentityfact.EntityID]bool
	material   map[viewentityfact.EntityID]*viewentityfact.Entity
	newAliases map[string]viewentityfact.EntityID
}

func newEntityResolver(scope views.Scope, current []viewentityfact.Entity) *entityResolver {
	r := &entityResolver{
		scope:      scope,
		byAlias:    map[string]*viewentityfact.Entity{},
		stored:     map[viewentityfact.EntityID]bool{},
		material:   map[viewentityfact.EntityID]*viewentityfact.Entity{},
		newAliases: map[string]viewentityfact.EntityID{},
	}
	for _, entity := range current {
		entity := viewentityfact.CloneEntity(entity)
		r.stored[entity.ID] = true
		r.material[entity.ID] = &entity
		for _, key := range entity.AliasKeys() {
			r.byAlias[key] = &entity
		}
	}
	return r
}

func (r *entityResolver) resolve(name string, proposal llmEntity, refs []views.SourceRef, now time.Time) *viewentityfact.Entity {
	name = strings.TrimSpace(name)
	key := viewentityfact.NormalizeAlias(name)
	if key == "" {
		return nil
	}
	if existing := r.byAlias[key]; existing != nil {
		return existing
	}
	if id, ok := r.newAliases[key]; ok {
		return r.material[id]
	}
	entity := viewentityfact.Entity{
		ID:          stableEntityID(r.scope, normalizeEntityType(proposal.Type), name),
		Scope:       r.scope,
		Type:        normalizeEntityType(proposal.Type),
		Name:        name,
		Aliases:     uniqueStrings(proposal.Aliases),
		Summary:     strings.TrimSpace(proposal.Summary),
		MentionRefs: refs,
		Confidence:  clampConfidence(proposal.Confidence),
		CreatedAt:   now,
		UpdatedAt:   now,
		Metadata: map[string]any{
			"algorithm": llmAlgorithm,
			"version":   llmVersion,
		},
	}
	if entity.Type == "" {
		entity.Type = viewentityfact.EntityUnknown
	}
	if !isGraphAnchorEntityType(entity.Type) {
		return nil
	}
	if err := viewentityfact.ValidateEntity(entity); err != nil {
		return nil
	}
	r.material[entity.ID] = &entity
	r.newAliases[key] = entity.ID
	for _, alias := range entity.AliasKeys() {
		r.byAlias[alias] = &entity
	}
	return &entity
}

func (r *entityResolver) lookup(name string) *viewentityfact.Entity {
	key := viewentityfact.NormalizeAlias(name)
	if key == "" {
		return nil
	}
	return r.byAlias[key]
}

func (r *entityResolver) catalog() []llmEntityCatalogEntry {
	if r == nil {
		return nil
	}
	out := make([]llmEntityCatalogEntry, 0, len(r.material))
	for _, entity := range r.material {
		if entity == nil {
			continue
		}
		out = append(out, llmEntityCatalogEntry{
			Name:    entity.Name,
			Type:    string(entity.Type),
			Aliases: uniqueStrings(entity.Aliases),
			Summary: strings.TrimSpace(entity.Summary),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Type < out[j].Type
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (r *entityResolver) isNew(id viewentityfact.EntityID) bool {
	return id != "" && !r.stored[id]
}

func (r *entityResolver) markStored(id viewentityfact.EntityID) {
	if id != "" {
		r.stored[id] = true
	}
}

type llmPromptPayload struct {
	ConversationID string                  `json:"conversation_id,omitempty"`
	Task           string                  `json:"task"`
	MessageCount   int                     `json:"message_count"`
	SourceIDs      []string                `json:"source_ids,omitempty"`
	EntityCatalog  []llmEntityCatalogEntry `json:"entity_catalog,omitempty"`
}

type llmEntityFactResponse struct {
	Entities []llmEntity `json:"entities"`
	Facts    []llmFact   `json:"facts"`
}

type llmEntity struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Aliases    []string `json:"aliases,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	SourceIDs  []string `json:"source_ids,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
}

type llmEntityCatalogEntry struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Aliases []string `json:"aliases,omitempty"`
	Summary string   `json:"summary,omitempty"`
}

type llmFact struct {
	Subject       string          `json:"subject"`
	RelationType  string          `json:"relation_type"`
	PredicateText string          `json:"predicate_text,omitempty"`
	ObjectNames   []string        `json:"object_names,omitempty"`
	ObjectSpans   []llmObjectSpan `json:"object_spans,omitempty"`
	TimeText      string          `json:"time_text,omitempty"`
	FactText      string          `json:"fact_text"`
	SourceIDs     []string        `json:"source_ids"`
	Confidence    float64         `json:"confidence,omitempty"`
}

type llmObjectSpan struct {
	Text     string `json:"text"`
	SourceID string `json:"source_id"`
	Type     string `json:"type,omitempty"`
}

func sourceMessageIDs(messages []sourcemessage.Message) []string {
	out := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.ID != "" {
			out = append(out, msg.ID)
		}
	}
	return out
}

func sourceMessagesByID(messages []sourcemessage.Message) map[string]sourcemessage.Message {
	out := make(map[string]sourcemessage.Message, len(messages))
	for _, msg := range messages {
		if msg.ID != "" {
			out[msg.ID] = msg
		}
	}
	return out
}

func llmEntitySystemPrompt() string {
	return `Extract conservative graph-anchor entities for long conversation memory.

Return JSON only with this shape:
{"entities":[{"name":"...","type":"person|place|organization|object|event|concept|date","aliases":["..."],"summary":"...","source_ids":["message-id"],"confidence":0.0}],"facts":[]}

Rules:
- Use only the source messages that follow the JSON control message. Do not infer unsupported facts.
- Source messages keep their original roles and multimodal parts; each has a final data part with MIME type application/vnd.flowcraft.source-message+json containing source_id, seq, metadata, and span_refs.
- Use the source_id from that source metadata data part as every returned source_ids value.
- Entities are graph anchors, not arbitrary noun phrases. Do not emit generic nouns, sensory descriptions, one-off descriptions, or event-like phrases as entities.
- Do not use an "unknown" entity type. If an entity cannot be typed as one of the fixed labels, omit it.
- Extract only named people, stable places/organizations, durable personal objects, stable concepts, dates/times, and stable named/identifiable events.
- Do not extract facts in this pass. Return an empty facts array.
- Omit uncertain entities instead of guessing.
- Use the fixed entity type labels only.`
}

func llmFactSystemPrompt() string {
	return `Extract conservative source-backed facts for long conversation memory using only the provided entity catalog.

Return JSON only with this shape:
{"entities":[],"facts":[{"subject":"entity catalog name","relation_type":"attribute|preference|activity|relationship|location|possession|plan|event|time|other","predicate_text":"short predicate","object_names":["entity catalog name"],"object_spans":[{"text":"exact source substring","source_id":"message-id","type":"literal|generic|object|event|time"}],"time_text":"date/time if explicit","fact_text":"one grounded sentence","source_ids":["message-id"],"confidence":0.0}]}

Rules:
- Use only the source messages that follow the JSON control message and the entity_catalog in that control message. Do not infer unsupported facts.
- Source messages keep their original roles and multimodal parts; each has a final data part with MIME type application/vnd.flowcraft.source-message+json containing source_id, seq, metadata, and span_refs.
- Use the source_id from that source metadata data part as every returned source_ids value.
- Extract durable facts useful for later retrieval: relationships, preferences, activities, plans, locations, possessions, dates/times, attributes, and notable events.
- Preserve answer-bearing details that often support multi-hop recall: activities and hobbies, books/media/artifacts, places and moves, family/friend relationships, events participated in, shared interests, plans, and temporal anchors.
- Every fact subject must exactly match an entity catalog name or alias. object_names may only contain entity catalog names or aliases.
- Use object_spans for literal values, generic objects, or object-like phrases that should stay source-backed instead of becoming entities.
- Every object_spans item must quote an exact substring from the source message identified by source_id. The source_id must also appear in that fact's source_ids.
- Leave object_spans empty when no exact source substring supports the object.
- If a source message contains a durable answer-bearing detail, emit a fact for that source_id instead of relying on neighboring messages to carry the memory.
- Use event entities only when they already appear in entity_catalog; otherwise keep notable happenings in fact_text and source_ids.
- Keep facts atomic. Split unrelated facts into separate items.
- Preserve negations and subject boundaries.
- Use source_ids from the provided source message metadata only.
- Omit uncertain facts instead of guessing.
- Do not emit new entities in this pass. Return an empty entities array.
- Use the fixed relation type labels only.`
}

func llmCoverageRepairSystemPrompt() string {
	return `Repair missing source coverage for an entity-linked fact graph.

Return JSON only with this shape:
{"entities":[],"facts":[{"subject":"entity catalog name","relation_type":"attribute|preference|activity|relationship|location|possession|plan|event|time|other","predicate_text":"short predicate","object_names":["entity catalog name"],"object_spans":[{"text":"exact source substring","source_id":"message-id","type":"literal|generic|object|event|time"}],"time_text":"date/time if explicit","fact_text":"one grounded sentence","source_ids":["message-id"],"confidence":0.0}]}

Rules:
- Use only the source messages that follow the JSON control message and the entity_catalog in that control message. Do not infer unsupported facts.
- Return at most one fact per source_id.
- Do not emit new entities. Return an empty entities array.
- Every fact subject must exactly match an entity catalog name or alias. object_names may only contain entity catalog names or aliases.
- Prefer graphable facts: use a non-other relation type and include an object entity, exact object_span, or explicit time/event/plan anchor.
- Use object_spans for answer-bearing literal values, generic objects, activities, books/media/artifacts, places, events, or other phrases that should remain source-backed instead of becoming entities.
- Every object_spans item must quote an exact substring from the source message identified by source_id. The source_id must also appear in that fact's source_ids.
- Preserve negations and subject boundaries.
- Omit a source_id when it has no durable answer-bearing detail rather than inventing a fact.
- Use the fixed relation type labels only.`
}

func parseLLMEntityFactResponse(content string) (llmEntityFactResponse, error) {
	content = strings.TrimSpace(content)
	var out llmEntityFactResponse
	if err := json.Unmarshal([]byte(content), &out); err == nil {
		return out, nil
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(content[start:end+1]), &out); err == nil {
			return out, nil
		}
	}
	return llmEntityFactResponse{}, fmt.Errorf("entity fact agent: invalid JSON response")
}

func uncoveredMessages(window viewrecent.WindowResult, currentEntities []viewentityfact.Entity, currentFacts []viewentityfact.Fact) []sourcemessage.Message {
	covered := map[string]bool{}
	for _, entity := range currentEntities {
		for _, ref := range entity.MentionRefs {
			if ref.Message != nil {
				covered[ref.Message.MessageID] = true
			}
		}
	}
	for _, fact := range currentFacts {
		for _, ref := range fact.SourceRefs {
			if ref.Message != nil {
				covered[ref.Message.MessageID] = true
			}
		}
	}
	out := make([]sourcemessage.Message, 0, len(window.Messages))
	for _, msg := range window.Messages {
		if msg.ID == "" || covered[msg.ID] {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func sourceRefsByMessageID(window viewrecent.WindowResult) map[string]views.SourceRef {
	out := make(map[string]views.SourceRef, len(window.Messages))
	for i, msg := range window.Messages {
		ref := views.SourceRef{
			Kind: views.SourceMessage,
			Message: &views.MessageSourceRef{
				ConversationID: msg.ConversationID,
				MessageID:      msg.ID,
			},
		}
		if i < len(window.SourceRefs) && window.SourceRefs[i].Message != nil {
			ref = window.SourceRefs[i]
		}
		out[msg.ID] = ref
	}
	return out
}

func refsForSourceIDs(ids []string, byID map[string]views.SourceRef) []views.SourceRef {
	seen := map[string]bool{}
	var out []views.SourceRef
	for _, id := range ids {
		ref, ok := byID[id]
		if !ok {
			continue
		}
		key, err := ref.StableKeyE()
		if err != nil || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

func validatedObjectSpans(spans []llmObjectSpan, sourceIDs []string, sourceByID map[string]sourcemessage.Message) []viewentityfact.ObjectSpan {
	if len(spans) == 0 {
		return nil
	}
	allowedSources := map[string]bool{}
	for _, id := range sourceIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			allowedSources[id] = true
		}
	}
	seen := map[string]bool{}
	out := make([]viewentityfact.ObjectSpan, 0, len(spans))
	for _, span := range spans {
		text := strings.TrimSpace(span.Text)
		sourceID := strings.TrimSpace(span.SourceID)
		if text == "" || sourceID == "" || !allowedSources[sourceID] {
			continue
		}
		msg, ok := sourceByID[sourceID]
		if !ok || !containsExact(msg.Content(), text) {
			continue
		}
		objectSpan := viewentityfact.ObjectSpan{
			Text:     text,
			SourceID: sourceID,
			Type:     strings.TrimSpace(span.Type),
		}
		key := sourceID + "\x00" + strings.ToLower(text) + "\x00" + strings.ToLower(objectSpan.Type)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, objectSpan)
	}
	return out
}

func containsExact(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func stableEntityID(scope views.Scope, entityType viewentityfact.EntityType, name string) viewentityfact.EntityID {
	key := strings.Join([]string{scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, string(entityType), viewentityfact.NormalizeAlias(name)}, "\x00")
	return viewentityfact.EntityID("ent_" + shortHash(key))
}

func stableFactID(scope views.Scope, text string, refs []views.SourceRef) viewentityfact.FactID {
	var keys []string
	for _, ref := range refs {
		key, err := ref.StableKeyE()
		if err == nil {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	key := strings.Join(append([]string{scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, strings.ToLower(strings.TrimSpace(text))}, keys...), "\x00")
	return viewentityfact.FactID("fact_" + shortHash(key))
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:24]
}

func normalizeEntityType(value string) viewentityfact.EntityType {
	switch viewentityfact.EntityType(strings.ToLower(strings.TrimSpace(value))) {
	case viewentityfact.EntityPerson:
		return viewentityfact.EntityPerson
	case viewentityfact.EntityPlace:
		return viewentityfact.EntityPlace
	case viewentityfact.EntityOrganization:
		return viewentityfact.EntityOrganization
	case viewentityfact.EntityObject:
		return viewentityfact.EntityObject
	case viewentityfact.EntityEvent:
		return viewentityfact.EntityEvent
	case viewentityfact.EntityConcept:
		return viewentityfact.EntityConcept
	case viewentityfact.EntityDate:
		return viewentityfact.EntityDate
	default:
		return viewentityfact.EntityUnknown
	}
}

func isGraphAnchorEntityType(entityType viewentityfact.EntityType) bool {
	switch entityType {
	case viewentityfact.EntityPerson, viewentityfact.EntityPlace, viewentityfact.EntityOrganization, viewentityfact.EntityObject, viewentityfact.EntityEvent, viewentityfact.EntityConcept, viewentityfact.EntityDate:
		return true
	default:
		return false
	}
}

func normalizeRelationType(value string) viewentityfact.RelationType {
	switch viewentityfact.RelationType(strings.ToLower(strings.TrimSpace(value))) {
	case viewentityfact.RelationAttribute:
		return viewentityfact.RelationAttribute
	case viewentityfact.RelationPreference:
		return viewentityfact.RelationPreference
	case viewentityfact.RelationActivity:
		return viewentityfact.RelationActivity
	case viewentityfact.RelationRelationship:
		return viewentityfact.RelationRelationship
	case viewentityfact.RelationLocation:
		return viewentityfact.RelationLocation
	case viewentityfact.RelationPossession:
		return viewentityfact.RelationPossession
	case viewentityfact.RelationPlan:
		return viewentityfact.RelationPlan
	case viewentityfact.RelationEvent:
		return viewentityfact.RelationEvent
	case viewentityfact.RelationTime:
		return viewentityfact.RelationTime
	default:
		return viewentityfact.RelationOther
	}
}

func renderFactText(subject, predicate string, objects []string, timeText string) string {
	parts := []string{strings.TrimSpace(subject), strings.TrimSpace(predicate)}
	if len(objects) > 0 {
		parts = append(parts, strings.Join(uniqueStrings(objects), ", "))
	}
	if strings.TrimSpace(timeText) != "" {
		parts = append(parts, strings.TrimSpace(timeText))
	}
	return strings.TrimSpace(strings.Join(nonEmpty(parts), " "))
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range in {
		value = strings.TrimSpace(value)
		key := viewentityfact.NormalizeAlias(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func uniqueEntityIDs(in []viewentityfact.EntityID) []viewentityfact.EntityID {
	seen := map[viewentityfact.EntityID]bool{}
	var out []viewentityfact.EntityID
	for _, id := range in {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func nonEmpty(in []string) []string {
	out := in[:0]
	for _, value := range in {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func clampConfidence(value float64) float64 {
	if value <= 0 {
		return 0.8
	}
	if value > 1 {
		return 1
	}
	return value
}

func maxPositive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
