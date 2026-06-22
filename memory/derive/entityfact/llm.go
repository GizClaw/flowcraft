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
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkllm "github.com/GizClaw/flowcraft/sdk/llm"
)

const (
	llmAlgorithm = "entity_fact_llm"
	llmVersion   = "v1"

	defaultMaxMessagesPerCall = 16
)

// LLMExtractor derives a conservative entity-linked fact index from messages.
//
// LLM output is treated as proposal text only. Entity IDs, fact IDs, source
// refs, and alias linking are deterministic and locally validated.
type LLMExtractor struct {
	LLM sdkllm.LLM

	MaxMessagesPerCall int
	Timeout            time.Duration
}

var _ derive.EntityFactExtractor = LLMExtractor{}

func (e LLMExtractor) ExtractEntityFacts(ctx context.Context, input derive.EntityFactInput) (derive.EntityFactOutput, error) {
	select {
	case <-ctx.Done():
		return derive.EntityFactOutput{}, ctx.Err()
	default:
	}
	if e.LLM == nil {
		return derive.EntityFactOutput{}, errdefs.NotAvailablef("entity fact llm: LLM is not configured")
	}

	refByMessageID := sourceRefsByMessageID(input.Window)
	pending := uncoveredMessages(input.Window, input.CurrentEntities, input.CurrentFacts)
	if len(pending) == 0 {
		return derive.EntityFactOutput{}, nil
	}

	resolver := newEntityResolver(input.Scope, input.CurrentEntities)
	seenFactIDs := map[viewentityfact.FactID]bool{}
	for _, fact := range input.CurrentFacts {
		seenFactIDs[fact.ID] = true
	}

	var out derive.EntityFactOutput
	for _, chunk := range e.messageChunks(pending) {
		resp, err := e.extractChunk(ctx, input.Scope, chunk)
		if err != nil {
			return derive.EntityFactOutput{}, err
		}
		chunkOut, err := e.materializeResponse(input, resp, refByMessageID, resolver, seenFactIDs)
		if err != nil {
			return derive.EntityFactOutput{}, err
		}
		out.Entities = append(out.Entities, chunkOut.Entities...)
		out.Facts = append(out.Facts, chunkOut.Facts...)
	}
	return out, nil
}

func (e LLMExtractor) extractChunk(ctx context.Context, scope views.Scope, messages []sourcemessage.Message) (llmEntityFactResponse, error) {
	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}

	payload := llmPromptPayload{
		ConversationID: scope.ConversationID,
		Task:           "conversation_source_message_entity_fact_extraction",
		MessageCount:   len(messages),
		SourceIDs:      sourceMessageIDs(messages),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return llmEntityFactResponse{}, err
	}
	llmMessages := []sdkllm.Message{
		sdkllm.NewTextMessage(sdkllm.RoleSystem, llmEntityFactSystemPrompt()),
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

func (e LLMExtractor) materializeResponse(input derive.EntityFactInput, resp llmEntityFactResponse, refByMessageID map[string]views.SourceRef, resolver *entityResolver, seenFactIDs map[viewentityfact.FactID]bool) (derive.EntityFactOutput, error) {
	now := time.Now().UTC()
	var out derive.EntityFactOutput
	entityProposals := make(map[string]llmEntity)
	for _, proposal := range resp.Entities {
		key := viewentityfact.NormalizeAlias(proposal.Name)
		if key == "" {
			continue
		}
		entityProposals[key] = proposal
	}
	for _, proposal := range resp.Facts {
		refs := refsForSourceIDs(proposal.SourceIDs, refByMessageID)
		if len(refs) == 0 {
			continue
		}
		subject := resolver.resolve(proposal.Subject, entityProposals[viewentityfact.NormalizeAlias(proposal.Subject)], refs, now)
		if subject == nil {
			continue
		}
		if resolver.isNew(subject.ID) {
			out.Entities = append(out.Entities, *subject)
			resolver.markStored(subject.ID)
		}
		var objects []viewentityfact.EntityID
		for _, name := range proposal.ObjectNames {
			object := resolver.resolve(name, entityProposals[viewentityfact.NormalizeAlias(name)], refs, now)
			if object == nil {
				continue
			}
			objects = append(objects, object.ID)
			if resolver.isNew(object.ID) {
				out.Entities = append(out.Entities, *object)
				resolver.markStored(object.ID)
			}
		}

		factText := strings.TrimSpace(proposal.FactText)
		if factText == "" {
			factText = renderFactText(proposal.Subject, proposal.PredicateText, proposal.ObjectNames, proposal.TimeText)
		}
		if factText == "" {
			continue
		}
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
		if seenFactIDs[fact.ID] {
			continue
		}
		if err := viewentityfact.ValidateFact(fact); err != nil {
			continue
		}
		seenFactIDs[fact.ID] = true
		out.Facts = append(out.Facts, fact)
	}
	return out, nil
}

func (e LLMExtractor) messageChunks(messages []sourcemessage.Message) [][]sourcemessage.Message {
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

func (e LLMExtractor) transformSignature() string {
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

func (r *entityResolver) isNew(id viewentityfact.EntityID) bool {
	return id != "" && !r.stored[id]
}

func (r *entityResolver) markStored(id viewentityfact.EntityID) {
	if id != "" {
		r.stored[id] = true
	}
}

type llmPromptPayload struct {
	ConversationID string   `json:"conversation_id,omitempty"`
	Task           string   `json:"task"`
	MessageCount   int      `json:"message_count"`
	SourceIDs      []string `json:"source_ids,omitempty"`
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

type llmFact struct {
	Subject       string   `json:"subject"`
	RelationType  string   `json:"relation_type"`
	PredicateText string   `json:"predicate_text,omitempty"`
	ObjectNames   []string `json:"object_names,omitempty"`
	TimeText      string   `json:"time_text,omitempty"`
	FactText      string   `json:"fact_text"`
	SourceIDs     []string `json:"source_ids"`
	Confidence    float64  `json:"confidence,omitempty"`
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

func llmEntityFactSystemPrompt() string {
	return `Extract a conservative entity-linked fact index for long conversation memory.

Return JSON only with this shape:
{"entities":[{"name":"...","type":"person|place|organization|object|event|concept|date|unknown","aliases":["..."],"summary":"...","source_ids":["message-id"],"confidence":0.0}],"facts":[{"subject":"entity name","relation_type":"attribute|preference|activity|relationship|location|possession|plan|event|time|other","predicate_text":"short predicate","object_names":["entity name"],"time_text":"date/time if explicit","fact_text":"one grounded sentence","source_ids":["message-id"],"confidence":0.0}]}

Rules:
- Use only the source messages that follow the JSON control message. Do not infer unsupported facts.
- Source messages keep their original roles and multimodal parts; each has a final data part with MIME type application/vnd.flowcraft.source-message+json containing source_id, seq, metadata, and span_refs.
- Use the source_id from that source metadata data part as every returned source_ids value.
- Extract durable facts useful for later retrieval: names, relationships, preferences, plans, places, objects, dates/times, outcomes, and notable events.
- Keep facts atomic. Split unrelated facts into separate items.
- Preserve negations and subject boundaries.
- Use source_ids from the provided source message metadata only.
- Omit uncertain facts instead of guessing.
- Use the fixed entity and relation type labels only.`
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
	return llmEntityFactResponse{}, fmt.Errorf("entity fact llm: invalid JSON response")
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
