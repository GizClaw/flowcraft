package entityfact

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/graph/runner"
	sdkllm "github.com/GizClaw/flowcraft/sdk/llm"
)

const (
	agentWorkflowName = "entity_fact_agent_extraction"

	agentNodePrepare        = "entity_fact_agent_prepare"
	agentNodeEntityExtract  = "entity_fact_agent_entity_extract"
	agentNodeEntityValidate = "entity_fact_agent_entity_validate"
	agentNodeFactExtract    = "entity_fact_agent_fact_extract"
	agentNodeFactValidate   = "entity_fact_agent_fact_validate"
	agentNodeCoverageRepair = "entity_fact_agent_coverage_repair"

	agentBoardInput           = "entity_fact.input"
	agentBoardPendingMessages = "entity_fact.pending_messages"
	agentBoardChunks          = "entity_fact.chunks"
	agentBoardRefsByMessageID = "entity_fact.refs_by_message_id"
	agentBoardResolver        = "entity_fact.resolver"
	agentBoardSeenFactIDs     = "entity_fact.seen_fact_ids"
	agentBoardEntityResponses = "entity_fact.entity_responses"
	agentBoardEntityCatalog   = "entity_fact.entity_catalog"
	agentBoardFactResponses   = "entity_fact.fact_responses"
	agentBoardOutput          = "entity_fact.output"
)

// AgentExtractor derives entity/fact proposals through a sdk/graph workflow.
//
// The graph is a write-time extraction DAG only: its nodes produce and pass LLM
// proposals, while stable IDs, source refs, span validation, and graphability
// metadata remain owned by the local Go materialization logic.
type AgentExtractor struct {
	LLM sdkllm.LLM

	MaxMessagesPerCall int
	Timeout            time.Duration
}

var _ derive.EntityFactExtractor = AgentExtractor{}

func (e AgentExtractor) ExtractEntityFacts(ctx context.Context, input derive.EntityFactInput) (derive.EntityFactOutput, error) {
	select {
	case <-ctx.Done():
		return derive.EntityFactOutput{}, ctx.Err()
	default:
	}
	if e.LLM == nil {
		return derive.EntityFactOutput{}, errdefs.NotAvailablef("entity fact agent: LLM is not configured")
	}

	pending := uncoveredMessages(input.Window, input.CurrentEntities, input.CurrentFacts)
	if len(pending) == 0 {
		return derive.EntityFactOutput{}, nil
	}

	r, err := e.newRunner()
	if err != nil {
		return derive.EntityFactOutput{}, err
	}
	board, err := r.Run(ctx, map[string]any{
		agentBoardInput:           input,
		agentBoardPendingMessages: pending,
	})
	if err != nil {
		return derive.EntityFactOutput{}, err
	}
	out, ok := graph.GetTyped[derive.EntityFactOutput](board, agentBoardOutput)
	if !ok {
		return derive.EntityFactOutput{}, errdefs.Validationf("entity fact agent: workflow did not produce output")
	}
	return out, nil
}

func (e AgentExtractor) newRunner() (*runner.Runner, error) {
	factory := node.NewFactory()
	for _, nodeType := range []string{
		agentNodePrepare,
		agentNodeEntityExtract,
		agentNodeEntityValidate,
		agentNodeFactExtract,
		agentNodeFactValidate,
		agentNodeCoverageRepair,
	} {
		factory.RegisterBuilder(nodeType, func(def graph.NodeDefinition) (graph.Node, error) {
			return agentWorkflowNode{id: def.ID, typ: nodeType, extractor: e}, nil
		})
	}

	def := &graph.GraphDefinition{
		Name:  agentWorkflowName,
		Entry: "prepare",
		Nodes: []graph.NodeDefinition{
			{ID: "prepare", Type: agentNodePrepare},
			{ID: "entity_extract", Type: agentNodeEntityExtract},
			{ID: "entity_validate", Type: agentNodeEntityValidate},
			{ID: "fact_extract", Type: agentNodeFactExtract},
			{ID: "fact_validate", Type: agentNodeFactValidate},
			{ID: "coverage_repair", Type: agentNodeCoverageRepair},
		},
		Edges: []graph.EdgeDefinition{
			{From: "prepare", To: "entity_extract"},
			{From: "entity_extract", To: "entity_validate"},
			{From: "entity_validate", To: "fact_extract"},
			{From: "fact_extract", To: "fact_validate"},
			{From: "fact_validate", To: "coverage_repair"},
			{From: "coverage_repair", To: graph.END},
		},
	}
	return runner.New(def, factory)
}

type agentWorkflowNode struct {
	id        string
	typ       string
	extractor AgentExtractor
}

func (n agentWorkflowNode) ID() string   { return n.id }
func (n agentWorkflowNode) Type() string { return n.typ }

func (n agentWorkflowNode) ExecuteBoard(ctx graph.ExecutionContext, board *graph.Board) error {
	switch n.typ {
	case agentNodePrepare:
		return n.prepare(board)
	case agentNodeEntityExtract:
		return n.extractEntities(ctx.Context, board)
	case agentNodeEntityValidate:
		return n.validateEntities(board)
	case agentNodeFactExtract:
		return n.extractFacts(ctx.Context, board)
	case agentNodeFactValidate:
		return n.validateFacts(board)
	case agentNodeCoverageRepair:
		return n.repairCoverage(ctx.Context, board)
	default:
		return errdefs.Validationf("entity fact agent: unknown workflow node type %q", n.typ)
	}
}

func (n agentWorkflowNode) prepare(board *graph.Board) error {
	input, ok := graph.GetTyped[derive.EntityFactInput](board, agentBoardInput)
	if !ok {
		return missingAgentBoardVar(agentBoardInput)
	}
	pending, ok := graph.GetTyped[[]sourcemessage.Message](board, agentBoardPendingMessages)
	if !ok {
		return missingAgentBoardVar(agentBoardPendingMessages)
	}

	resolver := newEntityResolver(input.Scope, input.CurrentEntities)
	seenFactIDs := map[viewentityfact.FactID]bool{}
	for _, fact := range input.CurrentFacts {
		seenFactIDs[fact.ID] = true
	}

	board.SetVar(agentBoardRefsByMessageID, sourceRefsByMessageID(input.Window))
	board.SetVar(agentBoardChunks, n.extractor.messageChunks(pending))
	board.SetVar(agentBoardResolver, resolver)
	board.SetVar(agentBoardSeenFactIDs, seenFactIDs)
	board.SetVar(agentBoardOutput, derive.EntityFactOutput{})
	return nil
}

func (n agentWorkflowNode) extractEntities(ctx context.Context, board *graph.Board) error {
	input, ok := graph.GetTyped[derive.EntityFactInput](board, agentBoardInput)
	if !ok {
		return missingAgentBoardVar(agentBoardInput)
	}
	chunks, ok := graph.GetTyped[[][]sourcemessage.Message](board, agentBoardChunks)
	if !ok {
		return missingAgentBoardVar(agentBoardChunks)
	}

	responses := make([]agentEntityResponse, 0, len(chunks))
	for _, chunk := range chunks {
		resp, err := n.extractor.extractEntityChunk(ctx, input.Scope, chunk)
		if err != nil {
			return err
		}
		responses = append(responses, agentEntityResponse{Entities: resp.Entities})
	}
	board.SetVar(agentBoardEntityResponses, responses)
	return nil
}

func (n agentWorkflowNode) validateEntities(board *graph.Board) error {
	refByMessageID, ok := graph.GetTyped[map[string]views.SourceRef](board, agentBoardRefsByMessageID)
	if !ok {
		return missingAgentBoardVar(agentBoardRefsByMessageID)
	}
	resolver, ok := graph.GetTyped[*entityResolver](board, agentBoardResolver)
	if !ok {
		return missingAgentBoardVar(agentBoardResolver)
	}
	responses, ok := graph.GetTyped[[]agentEntityResponse](board, agentBoardEntityResponses)
	if !ok {
		return missingAgentBoardVar(agentBoardEntityResponses)
	}
	out, ok := graph.GetTyped[derive.EntityFactOutput](board, agentBoardOutput)
	if !ok {
		return missingAgentBoardVar(agentBoardOutput)
	}

	for _, response := range responses {
		out.Entities = append(out.Entities, n.extractor.materializeEntities(response.Entities, refByMessageID, resolver)...)
	}
	board.SetVar(agentBoardOutput, out)
	board.SetVar(agentBoardEntityCatalog, resolver.catalog())
	return nil
}

func (n agentWorkflowNode) extractFacts(ctx context.Context, board *graph.Board) error {
	input, ok := graph.GetTyped[derive.EntityFactInput](board, agentBoardInput)
	if !ok {
		return missingAgentBoardVar(agentBoardInput)
	}
	chunks, ok := graph.GetTyped[[][]sourcemessage.Message](board, agentBoardChunks)
	if !ok {
		return missingAgentBoardVar(agentBoardChunks)
	}
	catalog, ok := graph.GetTyped[[]llmEntityCatalogEntry](board, agentBoardEntityCatalog)
	if !ok {
		return missingAgentBoardVar(agentBoardEntityCatalog)
	}

	responses := make([]agentFactResponse, 0, len(chunks))
	for _, chunk := range chunks {
		resp, err := n.extractor.extractFactChunk(ctx, input.Scope, chunk, catalog)
		if err != nil {
			return err
		}
		responses = append(responses, agentFactResponse{
			Facts:      resp.Facts,
			SourceByID: sourceMessagesByID(chunk),
		})
	}
	board.SetVar(agentBoardFactResponses, responses)
	return nil
}

func (n agentWorkflowNode) validateFacts(board *graph.Board) error {
	input, ok := graph.GetTyped[derive.EntityFactInput](board, agentBoardInput)
	if !ok {
		return missingAgentBoardVar(agentBoardInput)
	}
	refByMessageID, ok := graph.GetTyped[map[string]views.SourceRef](board, agentBoardRefsByMessageID)
	if !ok {
		return missingAgentBoardVar(agentBoardRefsByMessageID)
	}
	resolver, ok := graph.GetTyped[*entityResolver](board, agentBoardResolver)
	if !ok {
		return missingAgentBoardVar(agentBoardResolver)
	}
	seenFactIDs, ok := graph.GetTyped[map[viewentityfact.FactID]bool](board, agentBoardSeenFactIDs)
	if !ok {
		return missingAgentBoardVar(agentBoardSeenFactIDs)
	}
	responses, ok := graph.GetTyped[[]agentFactResponse](board, agentBoardFactResponses)
	if !ok {
		return missingAgentBoardVar(agentBoardFactResponses)
	}
	out, ok := graph.GetTyped[derive.EntityFactOutput](board, agentBoardOutput)
	if !ok {
		return missingAgentBoardVar(agentBoardOutput)
	}

	for _, response := range responses {
		proposals := filterResolvableFactProposals(response.Facts, resolver)
		out.Facts = append(out.Facts, n.extractor.materializeFacts(input, proposals, refByMessageID, response.SourceByID, resolver, seenFactIDs)...)
	}
	board.SetVar(agentBoardOutput, out)
	return nil
}

func (n agentWorkflowNode) repairCoverage(ctx context.Context, board *graph.Board) error {
	input, ok := graph.GetTyped[derive.EntityFactInput](board, agentBoardInput)
	if !ok {
		return missingAgentBoardVar(agentBoardInput)
	}
	pending, ok := graph.GetTyped[[]sourcemessage.Message](board, agentBoardPendingMessages)
	if !ok {
		return missingAgentBoardVar(agentBoardPendingMessages)
	}
	refByMessageID, ok := graph.GetTyped[map[string]views.SourceRef](board, agentBoardRefsByMessageID)
	if !ok {
		return missingAgentBoardVar(agentBoardRefsByMessageID)
	}
	resolver, ok := graph.GetTyped[*entityResolver](board, agentBoardResolver)
	if !ok {
		return missingAgentBoardVar(agentBoardResolver)
	}
	seenFactIDs, ok := graph.GetTyped[map[viewentityfact.FactID]bool](board, agentBoardSeenFactIDs)
	if !ok {
		return missingAgentBoardVar(agentBoardSeenFactIDs)
	}
	catalog, ok := graph.GetTyped[[]llmEntityCatalogEntry](board, agentBoardEntityCatalog)
	if !ok {
		return missingAgentBoardVar(agentBoardEntityCatalog)
	}
	out, ok := graph.GetTyped[derive.EntityFactOutput](board, agentBoardOutput)
	if !ok {
		return missingAgentBoardVar(agentBoardOutput)
	}

	gaps := sourceCoverageGaps(pending, out.Facts)
	if len(gaps) == 0 {
		return nil
	}
	repairMessages := make([]sourcemessage.Message, 0, len(gaps))
	for _, gap := range gaps {
		repairMessages = append(repairMessages, gap.Message)
	}

	for _, chunk := range n.extractor.messageChunks(repairMessages) {
		resp, err := n.extractor.extractCoverageRepairChunk(ctx, input.Scope, chunk, catalog)
		if err != nil {
			return err
		}
		proposals := oneFactPerSource(filterResolvableFactProposals(resp.Facts, resolver))
		out.Facts = append(out.Facts, n.extractor.materializeFacts(input, proposals, refByMessageID, sourceMessagesByID(chunk), resolver, seenFactIDs)...)
	}
	board.SetVar(agentBoardOutput, out)
	return nil
}

type agentEntityResponse struct {
	Entities []llmEntity
}

type agentFactResponse struct {
	Facts      []llmFact
	SourceByID map[string]sourcemessage.Message
}

func filterResolvableFactProposals(proposals []llmFact, resolver *entityResolver) []llmFact {
	if len(proposals) == 0 || resolver == nil {
		return nil
	}
	out := make([]llmFact, 0, len(proposals))
	for _, proposal := range proposals {
		if resolver.lookup(proposal.Subject) == nil {
			continue
		}
		if hasUnresolvedObjectName(proposal.ObjectNames, resolver) {
			continue
		}
		out = append(out, proposal)
	}
	return out
}

func hasUnresolvedObjectName(names []string, resolver *entityResolver) bool {
	for _, name := range names {
		if strings.TrimSpace(name) == "" || resolver.lookup(name) == nil {
			return true
		}
	}
	return false
}

type sourceCoverageGap struct {
	Message sourcemessage.Message
	Reason  string
}

const (
	sourceCoverageMissingFact      = "missing_fact"
	sourceCoverageMissingGraphable = "missing_graphable_fact"
	sourceCoverageWeakFact         = "weak_fact"
)

func sourceCoverageGaps(messages []sourcemessage.Message, facts []viewentityfact.Fact) []sourceCoverageGap {
	if len(messages) == 0 {
		return nil
	}
	bySource := map[string][]viewentityfact.Fact{}
	for _, fact := range facts {
		for _, ref := range fact.SourceRefs {
			if ref.Message == nil || ref.Message.MessageID == "" {
				continue
			}
			bySource[ref.Message.MessageID] = append(bySource[ref.Message.MessageID], fact)
		}
	}
	gaps := make([]sourceCoverageGap, 0)
	for _, msg := range messages {
		if msg.ID == "" {
			continue
		}
		sourceFacts := bySource[msg.ID]
		if len(sourceFacts) == 0 {
			gaps = append(gaps, sourceCoverageGap{Message: msg, Reason: sourceCoverageMissingFact})
			continue
		}
		if hasGraphableFact(sourceFacts) {
			continue
		}
		reason := sourceCoverageMissingGraphable
		if hasOnlyWeakFacts(sourceFacts) {
			reason = sourceCoverageWeakFact
		}
		gaps = append(gaps, sourceCoverageGap{Message: msg, Reason: reason})
	}
	return gaps
}

func hasGraphableFact(facts []viewentityfact.Fact) bool {
	for _, fact := range facts {
		if viewentityfact.IsGraphableFact(fact) {
			return true
		}
	}
	return false
}

func hasOnlyWeakFacts(facts []viewentityfact.Fact) bool {
	if len(facts) == 0 {
		return false
	}
	for _, fact := range facts {
		if fact.RelationType != viewentityfact.RelationOther && (len(fact.ObjectEntityIDs) > 0 || len(viewentityfact.ObjectSpansFromMetadata(fact.Metadata)) > 0 || strings.TrimSpace(fact.TimeText) != "") {
			return false
		}
	}
	return true
}

func oneFactPerSource(proposals []llmFact) []llmFact {
	if len(proposals) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]llmFact, 0, len(proposals))
	for _, proposal := range proposals {
		sourceID := firstSourceID(proposal.SourceIDs)
		if sourceID == "" || seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		out = append(out, proposal)
	}
	return out
}

func firstSourceID(ids []string) string {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			return id
		}
	}
	return ""
}

func missingAgentBoardVar(key string) error {
	return errdefs.Validationf("entity fact agent: missing workflow board var %q", key)
}
