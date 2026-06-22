package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/derive"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewrecent "github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkllm "github.com/GizClaw/flowcraft/sdk/llm"
)

const (
	llmAlgorithm = "summary_llm"
	llmVersion   = "v2"

	defaultLLMMaxSourceRefsPerNode = 3
	defaultLLMCondenseFanout       = 4
	defaultLLMMaxLeafChunkChars    = 6000
)

// LLMSummarizer derives a deterministic layered compaction SummaryDAG.
//
// The topology is fixed locally: raw message chunks become depth-0 leaf nodes,
// and contiguous same-depth summary chunks become depth+1 condensed nodes. The
// LLM only writes summary text; SourceRefs, ParentIDs, levels, and IDs are
// deterministic and locally validated.
type LLMSummarizer struct {
	LLM sdkllm.LLM

	Policy               derive.SummaryPolicy
	MaxSourceRefsPerNode int
	CondenseFanout       int
	MaxLeafChunkChars    int
	MaxMessagesPerCall   int
	Timeout              time.Duration
}

var _ derive.Summarizer = LLMSummarizer{}

func (s LLMSummarizer) Summarize(ctx context.Context, input derive.SummaryInput) ([]viewrecent.SummaryNode, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if s.LLM == nil {
		return nil, errdefs.NotAvailablef("summary llm: LLM is not configured")
	}

	policy := normalizePolicy(mergePolicy(s.Policy, input.Policy))
	transform := s.transformSignature(policy)
	allNodes := scopedSummaryNodes(input.Scope, input.Current)
	var out []viewrecent.SummaryNode

	foldBoundary := len(input.Window.Messages) - policy.MaxRawMessages
	if foldBoundary > 0 {
		foldCandidates := input.Window.Messages[:foldBoundary]
		covered := coveredSourceRefsFromNodes(input.Scope, input.Current)
		folded := make([]sourcemessage.Message, 0, len(foldCandidates))
		foldedRefs := make([]views.SourceRef, 0, len(foldCandidates))
		for i, msg := range foldCandidates {
			ref := sourceRefForWindowMessage(input.Window.SourceRefs, i, msg)
			key, err := ref.StableKeyE()
			if err != nil {
				return nil, err
			}
			if covered[key] {
				continue
			}
			folded = append(folded, msg)
			foldedRefs = append(foldedRefs, ref)
		}

		for _, chunk := range s.leafChunks(folded, foldedRefs) {
			summaryText, fallback, err := s.summarizeLeafChunk(ctx, input.Scope, chunk.messages)
			if err != nil {
				return nil, err
			}
			node, err := s.buildLeafNode(input, chunk.messages, chunk.refs, summaryText, transform, policy, fallback)
			if err != nil {
				return nil, err
			}
			out = append(out, node)
			allNodes = append(allNodes, node)
		}
	}

	condensed, err := s.deriveCondensedNodes(ctx, input, allNodes, transform, policy)
	if err != nil {
		return nil, err
	}
	out = append(out, condensed...)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

type llmLeafChunk struct {
	messages []sourcemessage.Message
	refs     []views.SourceRef
}

func (s LLMSummarizer) leafChunks(messages []sourcemessage.Message, refs []views.SourceRef) []llmLeafChunk {
	maxRefs := s.maxSourceRefsPerNode()
	maxChars := s.maxLeafChunkChars()
	chunks := make([]llmLeafChunk, 0, len(messages)/maxRefs)
	for start := 0; start < len(messages); {
		end := start
		chars := 0
		endedByChars := false
		for end < len(messages) && end-start < maxRefs {
			nextChars := len(renderMessage(messages[end].Message))
			if end > start && maxChars > 0 && chars+nextChars > maxChars {
				endedByChars = true
				break
			}
			chars += nextChars
			end++
		}
		if end == start {
			end++
			endedByChars = true
		}
		if end == len(messages) && end-start < maxRefs && !endedByChars {
			break
		}
		chunks = append(chunks, llmLeafChunk{
			messages: messages[start:end],
			refs:     refs[start:end],
		})
		start = end
	}
	return chunks
}

func (s LLMSummarizer) deriveCondensedNodes(ctx context.Context, input derive.SummaryInput, nodes []viewrecent.SummaryNode, transform string, policy derive.SummaryPolicy) ([]viewrecent.SummaryNode, error) {
	var out []viewrecent.SummaryNode
	fanout := s.condenseFanout()
	for depth := 0; depth <= maxSummaryDepth(nodes); depth++ {
		candidates := compactableNodesAtDepth(input.Scope, input.Window, nodes, depth)
		for len(candidates) >= fanout {
			children := append([]viewrecent.SummaryNode(nil), candidates[:fanout]...)
			candidates = candidates[fanout:]
			parentIDs := childSummaryIDs(children)
			if condensedNodeExists(input.Scope, nodes, depth+1, parentIDs) {
				continue
			}
			summaryText, fallback, err := s.summarizeCondensedChunk(ctx, input.Scope, depth+1, children)
			if err != nil {
				return nil, err
			}
			node, err := s.buildCondensedNode(input, children, summaryText, transform, policy, fallback)
			if err != nil {
				return nil, err
			}
			out = append(out, node)
			nodes = append(nodes, node)
		}
	}
	return out, nil
}

func (s LLMSummarizer) summarizeLeafChunk(ctx context.Context, scope views.Scope, messages []sourcemessage.Message) (string, bool, error) {
	payload := llmSummaryPromptPayload{
		ConversationID: scope.ConversationID,
		Task:           "leaf_summary",
		Depth:          0,
		MessageCount:   len(messages),
		SourceIDs:      sourceMessageIDs(messages),
	}
	return s.generateSummaryText(ctx, payload, messages, fallbackLeafSummaryText(messages))
}

func (s LLMSummarizer) summarizeCondensedChunk(ctx context.Context, scope views.Scope, depth int, children []viewrecent.SummaryNode) (string, bool, error) {
	payload := llmSummaryPromptPayload{
		ConversationID: scope.ConversationID,
		Task:           "condensed_summary",
		Depth:          depth,
		SummaryNodes:   llmSummaryInputNodes(children),
	}
	return s.generateSummaryText(ctx, payload, nil, fallbackCondensedSummaryText(children))
}

func (s LLMSummarizer) generateSummaryText(ctx context.Context, payload llmSummaryPromptPayload, sourceMessages []sourcemessage.Message, fallback string) (string, bool, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", false, err
	}
	llmMessages := []sdkllm.Message{
		sdkllm.NewTextMessage(sdkllm.RoleSystem, llmSummarySystemPrompt()),
		sdkllm.NewTextMessage(sdkllm.RoleUser, string(data)),
	}
	for _, msg := range sourceMessages {
		llmMessages = append(llmMessages, sourcemessage.PromptMessageFromSource(msg))
	}
	resp, _, err := s.LLM.Generate(ctx, llmMessages, sdkllm.WithJSONMode(true), sdkllm.WithTemperature(0))
	if err != nil {
		return fallback, true, nil
	}
	if summary, ok := llmSummaryText(resp.Content()); ok {
		return summary, false, nil
	}
	return fallback, true, nil
}

func (s LLMSummarizer) buildLeafNode(input derive.SummaryInput, messages []sourcemessage.Message, refs []views.SourceRef, summaryText string, transform string, policy derive.SummaryPolicy, fallback bool) (viewrecent.SummaryNode, error) {
	revisions, err := mergedSourceRevisions(nil, messages, refs)
	if err != nil {
		return viewrecent.SummaryNode{}, err
	}
	createdAt := summaryTime(nil, messages)
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	text := renderLLMSummaryNodeText(summaryText, 0, "leaf", messages)
	text = truncateBytes(text, policy.MaxSummaryBytes)
	metadata := map[string]any{
		"algorithm":                  llmAlgorithm,
		"version":                    llmVersion,
		"dag_topology":               "layered_compaction",
		"node_kind":                  "leaf",
		"depth":                      0,
		"max_raw_messages":           policy.MaxRawMessages,
		"preserve_recent_messages":   policy.PreserveRecentMessages,
		"max_summary_bytes":          policy.MaxSummaryBytes,
		"leaf_max_source_refs":       s.maxSourceRefsPerNode(),
		"leaf_max_chars":             s.maxLeafChunkChars(),
		"condense_fanout":            s.condenseFanout(),
		"source_message_count":       len(messages),
		"source_ref_count":           len(refs),
		"transform_signature":        transform,
		"llm_fallback_deterministic": fallback,
	}
	node := viewrecent.SummaryNode{
		ID:         llmStableNodeID(input.Scope, transform, 0, refs, nil),
		Scope:      input.Scope,
		ParentIDs:  nil,
		SourceRefs: cloneSourceRefs(refs),
		Summary:    text,
		Level:      0,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    revisions,
			TransformSignature: transform,
			DiagnosticSignatures: map[string]string{
				"algorithm": llmAlgorithm,
				"version":   llmVersion,
			},
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		Metadata:  metadata,
	}
	return node, nil
}

func (s LLMSummarizer) buildCondensedNode(input derive.SummaryInput, children []viewrecent.SummaryNode, summaryText string, transform string, policy derive.SummaryPolicy, fallback bool) (viewrecent.SummaryNode, error) {
	if len(children) == 0 {
		return viewrecent.SummaryNode{}, fmt.Errorf("summary llm: condensed node requires child summaries")
	}
	depth := children[0].Level + 1
	parentIDs := childSummaryIDs(children)
	sourceRefs, err := mergedChildSourceRefs(input.Window, children)
	if err != nil {
		return viewrecent.SummaryNode{}, err
	}
	revisions, err := mergedChildSourceRevisions(input.Window, children, sourceRefs)
	if err != nil {
		return viewrecent.SummaryNode{}, err
	}
	createdAt := summaryTimeFromChildren(children)
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	text := renderLLMCondensedSummaryNodeText(summaryText, depth, children)
	text = truncateBytes(text, policy.MaxSummaryBytes)
	metadata := map[string]any{
		"algorithm":                  llmAlgorithm,
		"version":                    llmVersion,
		"dag_topology":               "layered_compaction",
		"node_kind":                  "condensed",
		"depth":                      depth,
		"parent_ids_semantics":       "compaction_input_summary_node_ids",
		"input_summary_node_count":   len(children),
		"source_ref_count":           len(sourceRefs),
		"max_raw_messages":           policy.MaxRawMessages,
		"preserve_recent_messages":   policy.PreserveRecentMessages,
		"max_summary_bytes":          policy.MaxSummaryBytes,
		"leaf_max_source_refs":       s.maxSourceRefsPerNode(),
		"leaf_max_chars":             s.maxLeafChunkChars(),
		"condense_fanout":            s.condenseFanout(),
		"transform_signature":        transform,
		"llm_fallback_deterministic": fallback,
	}
	node := viewrecent.SummaryNode{
		ID:         llmStableNodeID(input.Scope, transform, depth, sourceRefs, parentIDs),
		Scope:      input.Scope,
		ParentIDs:  parentIDs,
		SourceRefs: sourceRefs,
		Summary:    text,
		Level:      depth,
		Signature: views.ViewSignature{
			ViewID:             input.View.ID,
			SourceRevisions:    revisions,
			TransformSignature: transform,
			DiagnosticSignatures: map[string]string{
				"algorithm": llmAlgorithm,
				"version":   llmVersion,
			},
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
		Metadata:  metadata,
	}
	return node, nil
}

func (s LLMSummarizer) maxSourceRefsPerNode() int {
	if s.MaxSourceRefsPerNode > 0 {
		return s.MaxSourceRefsPerNode
	}
	return defaultLLMMaxSourceRefsPerNode
}

func (s LLMSummarizer) condenseFanout() int {
	if s.CondenseFanout > 0 {
		return s.CondenseFanout
	}
	return defaultLLMCondenseFanout
}

func (s LLMSummarizer) maxLeafChunkChars() int {
	if s.MaxLeafChunkChars > 0 {
		return s.MaxLeafChunkChars
	}
	return defaultLLMMaxLeafChunkChars
}

func (s LLMSummarizer) transformSignature(policy derive.SummaryPolicy) string {
	return fmt.Sprintf(
		"%s:%s:topology=layered_compaction:max_raw=%d:preserve=%d:max_bytes=%d:leaf_refs=%d:leaf_chars=%d:fanout=%d",
		llmAlgorithm,
		llmVersion,
		policy.MaxRawMessages,
		policy.PreserveRecentMessages,
		policy.MaxSummaryBytes,
		s.maxSourceRefsPerNode(),
		s.maxLeafChunkChars(),
		s.condenseFanout(),
	)
}

type llmSummaryPromptPayload struct {
	ConversationID string                `json:"conversation_id,omitempty"`
	Task           string                `json:"task"`
	Depth          int                   `json:"depth"`
	MessageCount   int                   `json:"message_count,omitempty"`
	SourceIDs      []string              `json:"source_ids,omitempty"`
	SummaryNodes   []llmSummaryInputNode `json:"summary_nodes,omitempty"`
}

type llmSummaryResponse struct {
	Summary string           `json:"summary"`
	Nodes   []llmSummaryNode `json:"nodes,omitempty"`
}

type llmSummaryNode struct {
	SourceIDs []string `json:"source_ids"`
	Summary   string   `json:"summary"`
}

type llmSummaryInputNode struct {
	ID             string   `json:"id"`
	Depth          int      `json:"depth"`
	SourceRefCount int      `json:"source_ref_count"`
	SourceIDs      []string `json:"source_ids,omitempty"`
	Summary        string   `json:"summary"`
}

func llmSummaryInputNodes(nodes []viewrecent.SummaryNode) []llmSummaryInputNode {
	out := make([]llmSummaryInputNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, llmSummaryInputNode{
			ID:             string(node.ID),
			Depth:          node.Level,
			SourceRefCount: len(node.SourceRefs),
			SourceIDs:      sourceRefMessageIDs(node.SourceRefs),
			Summary:        node.Summary,
		})
	}
	return out
}

func llmSummarySystemPrompt() string {
	return `Create retrieval-bridge summary text for long conversation memory.

Return JSON only with this shape:
{"summary":"concise grounded summary"}

Rules:
- Use only the provided messages and child summaries. Do not invent facts.
- Do not choose, omit, or rewrite source IDs. The caller fixes SourceRefs and DAG structure.
- For task=leaf_summary, summarize the source messages that follow the JSON control message as one retrieval node.
- Source messages keep their original roles and multimodal parts; each has a final data part with MIME type application/vnd.flowcraft.source-message+json containing source_id, seq, metadata, and span_refs.
- For task=condensed_summary, summarize the provided child summary nodes as one higher-level retrieval node.
- Include names, relationships, objects, dates/times, places, preferences, plans, and outcomes when present.
- Preserve negations and ownership/subject boundaries.
- Write summaries as retrieval text, not final answers.`
}

func parseLLMSummaryResponse(content string) (llmSummaryResponse, error) {
	content = strings.TrimSpace(content)
	var out llmSummaryResponse
	if err := json.Unmarshal([]byte(content), &out); err == nil {
		out.Summary = firstLLMSummary(out)
		return out, nil
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(content[start:end+1]), &out); err == nil {
			out.Summary = firstLLMSummary(out)
			return out, nil
		}
	}
	return llmSummaryResponse{}, fmt.Errorf("summary llm: invalid JSON response")
}

func llmSummaryText(content string) (string, bool) {
	if parsed, err := parseLLMSummaryResponse(content); err == nil {
		if summary := strings.TrimSpace(parsed.Summary); summary != "" {
			return summary, true
		}
	}
	content = strings.TrimSpace(strings.Trim(content, "`"))
	if content == "" || strings.HasPrefix(content, "{") {
		return "", false
	}
	return content, true
}

func firstLLMSummary(resp llmSummaryResponse) string {
	if summary := strings.TrimSpace(resp.Summary); summary != "" {
		return summary
	}
	for _, node := range resp.Nodes {
		if summary := strings.TrimSpace(node.Summary); summary != "" {
			return summary
		}
	}
	return ""
}

func renderLLMSummaryNodeText(summaryText string, depth int, kind string, messages []sourcemessage.Message) string {
	var b strings.Builder
	b.WriteString("Semantic summary (")
	b.WriteString(kind)
	b.WriteString(", depth=")
	b.WriteString(strconv.Itoa(depth))
	b.WriteString("):\n")
	b.WriteString(strings.TrimSpace(summaryText))
	b.WriteString("\n\nSource anchors:")
	for _, msg := range messages {
		b.WriteString("\n- ")
		b.WriteString(sourceAnchor(msg))
	}
	return b.String()
}

func renderLLMCondensedSummaryNodeText(summaryText string, depth int, children []viewrecent.SummaryNode) string {
	var b strings.Builder
	b.WriteString("Semantic summary (condensed, depth=")
	b.WriteString(strconv.Itoa(depth))
	b.WriteString("):\n")
	b.WriteString(strings.TrimSpace(summaryText))
	b.WriteString("\n\nCondensed child summaries:")
	for _, child := range children {
		b.WriteString("\n- id=")
		b.WriteString(string(child.ID))
		b.WriteString(" depth=")
		b.WriteString(strconv.Itoa(child.Level))
		b.WriteString(" sources=")
		b.WriteString(strconv.Itoa(len(child.SourceRefs)))
		if summary := strings.TrimSpace(singleLine(child.Summary)); summary != "" {
			b.WriteString(" summary=")
			b.WriteString(summary)
		}
	}
	return b.String()
}

func sourceAnchor(msg sourcemessage.Message) string {
	var parts []string
	if msg.ID != "" {
		parts = append(parts, "id="+msg.ID)
	}
	if msg.Seq > 0 {
		parts = append(parts, "seq="+strconv.FormatUint(msg.Seq, 10))
	}
	for _, key := range []string{"dia_id", "session", "session_date_time", "session_datetime", "speaker"} {
		if value, ok := msg.Metadata[key]; ok {
			if rendered := renderMetadataValue(value); rendered != "" {
				parts = append(parts, key+"="+rendered)
			}
		}
	}
	if text := strings.TrimSpace(msg.Content()); text != "" {
		parts = append(parts, "text="+singleLine(text))
	}
	return strings.Join(parts, " | ")
}

func renderMetadataValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return strings.Trim(string(data), `"`)
	}
}

func singleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func fallbackSummaryText(msg sourcemessage.Message) string {
	anchor := sourceAnchor(msg)
	if anchor == "" {
		return "Conversation message."
	}
	return "Conversation message: " + anchor
}

func fallbackLeafSummaryText(messages []sourcemessage.Message) string {
	if len(messages) == 1 {
		return fallbackSummaryText(messages[0])
	}
	var b strings.Builder
	b.WriteString("Conversation messages:")
	for _, msg := range messages {
		b.WriteString("\n- ")
		b.WriteString(sourceAnchor(msg))
	}
	return b.String()
}

func fallbackCondensedSummaryText(children []viewrecent.SummaryNode) string {
	var b strings.Builder
	b.WriteString("Condensed summaries:")
	for _, child := range children {
		b.WriteString("\n- ")
		b.WriteString(string(child.ID))
		if summary := strings.TrimSpace(singleLine(child.Summary)); summary != "" {
			b.WriteString(": ")
			b.WriteString(summary)
		}
	}
	return b.String()
}

func coveredSourceRefsFromNodes(scope views.Scope, nodes []viewrecent.SummaryNode) map[string]bool {
	covered := map[string]bool{}
	for _, node := range nodes {
		if node.Scope != scope {
			continue
		}
		for _, ref := range node.SourceRefs {
			key, err := ref.StableKeyE()
			if err != nil {
				continue
			}
			covered[key] = true
		}
	}
	return covered
}

func scopedSummaryNodes(scope views.Scope, nodes []viewrecent.SummaryNode) []viewrecent.SummaryNode {
	out := make([]viewrecent.SummaryNode, 0, len(nodes))
	for _, node := range nodes {
		if node.Scope == scope {
			out = append(out, node)
		}
	}
	return out
}

func compactableNodesAtDepth(scope views.Scope, window viewrecent.WindowResult, nodes []viewrecent.SummaryNode, depth int) []viewrecent.SummaryNode {
	compacted := map[viewrecent.NodeID]bool{}
	for _, node := range nodes {
		if node.Scope != scope || node.Level != depth+1 {
			continue
		}
		for _, id := range node.ParentIDs {
			compacted[id] = true
		}
	}
	out := make([]viewrecent.SummaryNode, 0, len(nodes))
	for _, node := range nodes {
		if node.Scope != scope || node.Level != depth || compacted[node.ID] {
			continue
		}
		out = append(out, node)
	}
	sortSummaryNodes(window, out)
	return out
}

func condensedNodeExists(scope views.Scope, nodes []viewrecent.SummaryNode, depth int, parentIDs []viewrecent.NodeID) bool {
	for _, node := range nodes {
		if node.Scope != scope || node.Level != depth {
			continue
		}
		if nodeIDsEqual(node.ParentIDs, parentIDs) {
			return true
		}
	}
	return false
}

func nodeIDsEqual(left, right []viewrecent.NodeID) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func childSummaryIDs(children []viewrecent.SummaryNode) []viewrecent.NodeID {
	out := make([]viewrecent.NodeID, 0, len(children))
	for _, child := range children {
		out = append(out, child.ID)
	}
	return out
}

func maxSummaryDepth(nodes []viewrecent.SummaryNode) int {
	maxDepth := -1
	for _, node := range nodes {
		if node.Level > maxDepth {
			maxDepth = node.Level
		}
	}
	return maxDepth
}

func sortSummaryNodes(window viewrecent.WindowResult, nodes []viewrecent.SummaryNode) {
	order := sourceRefOrder(window)
	sort.SliceStable(nodes, func(i, j int) bool {
		left, leftOK, leftKey := summaryNodeSourceOrder(nodes[i], order)
		right, rightOK, rightKey := summaryNodeSourceOrder(nodes[j], order)
		if leftOK != rightOK {
			return leftOK
		}
		if left != right {
			return left < right
		}
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		if !nodes[i].CreatedAt.Equal(nodes[j].CreatedAt) {
			return nodes[i].CreatedAt.Before(nodes[j].CreatedAt)
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func summaryNodeSourceOrder(node viewrecent.SummaryNode, order map[string]int) (int, bool, string) {
	best := int(^uint(0) >> 1)
	found := false
	bestKey := ""
	for _, ref := range node.SourceRefs {
		key, err := ref.StableKeyE()
		if err != nil {
			continue
		}
		if bestKey == "" || key < bestKey {
			bestKey = key
		}
		if idx, ok := order[key]; ok && idx < best {
			best = idx
			found = true
		}
	}
	return best, found, bestKey
}

func sourceRefOrder(window viewrecent.WindowResult) map[string]int {
	order := make(map[string]int, len(window.SourceRefs))
	for i, ref := range window.SourceRefs {
		key, err := ref.StableKeyE()
		if err != nil {
			continue
		}
		if _, ok := order[key]; !ok {
			order[key] = i
		}
	}
	return order
}

func mergedChildSourceRefs(window viewrecent.WindowResult, children []viewrecent.SummaryNode) ([]views.SourceRef, error) {
	seen := map[string]bool{}
	var refs []views.SourceRef
	for _, child := range children {
		for _, ref := range child.SourceRefs {
			if ref.Kind != views.SourceMessage {
				return nil, fmt.Errorf("summary llm: child source ref must reference message, got %q", ref.Kind)
			}
			key, err := ref.StableKeyE()
			if err != nil {
				return nil, err
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, cloneSourceRef(ref))
		}
	}
	sortSourceRefs(window, refs)
	return refs, nil
}

func sortSourceRefs(window viewrecent.WindowResult, refs []views.SourceRef) {
	order := sourceRefOrder(window)
	sort.SliceStable(refs, func(i, j int) bool {
		leftKey, _ := refs[i].StableKeyE()
		rightKey, _ := refs[j].StableKeyE()
		left, leftOK := order[leftKey]
		right, rightOK := order[rightKey]
		if leftOK != rightOK {
			return leftOK
		}
		if left != right {
			return left < right
		}
		return leftKey < rightKey
	})
}

func mergedChildSourceRevisions(window viewrecent.WindowResult, children []viewrecent.SummaryNode, refs []views.SourceRef) ([]views.SourceRevision, error) {
	byKey := map[string]views.SourceRevision{}
	for _, child := range children {
		for _, rev := range child.Signature.SourceRevisions {
			if rev.Kind != views.SourceMessage {
				return nil, fmt.Errorf("summary llm: child source revision must reference message, got %q", rev.Kind)
			}
			if _, ok := byKey[rev.SourceKey]; !ok {
				byKey[rev.SourceKey] = rev
			}
		}
	}
	for i, msg := range window.Messages {
		ref := sourceRefForWindowMessage(window.SourceRefs, i, msg)
		key, err := ref.StableKeyE()
		if err != nil {
			return nil, err
		}
		if _, ok := byKey[key]; !ok {
			byKey[key] = views.SourceRevision{
				Kind:        views.SourceMessage,
				SourceKey:   key,
				Revision:    strconv.FormatUint(msg.Seq, 10),
				ContentHash: MessageContentHash(msg),
				ObservedAt:  msg.CreatedAt,
			}
		}
	}
	revisions := make([]views.SourceRevision, 0, len(refs))
	for _, ref := range refs {
		key, err := ref.StableKeyE()
		if err != nil {
			return nil, err
		}
		rev, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("summary llm: missing source revision for %q", key)
		}
		revisions = append(revisions, rev)
	}
	return revisions, nil
}

func summaryTimeFromChildren(children []viewrecent.SummaryNode) time.Time {
	var latest time.Time
	for _, child := range children {
		if child.UpdatedAt.After(latest) {
			latest = child.UpdatedAt
		}
	}
	return latest
}

func sourceRefMessageIDs(refs []views.SourceRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Message == nil {
			continue
		}
		out = append(out, ref.Message.MessageID)
	}
	return out
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

func llmStableNodeID(scope views.Scope, transform string, depth int, refs []views.SourceRef, parentIDs []viewrecent.NodeID) viewrecent.NodeID {
	h := sha256.New()
	writeHashPart(h, scope.RuntimeID)
	writeHashPart(h, scope.UserID)
	writeHashPart(h, scope.AgentID)
	writeHashPart(h, scope.ConversationID)
	writeHashPart(h, scope.DatasetID)
	writeHashPart(h, llmAlgorithm)
	writeHashPart(h, llmVersion)
	writeHashPart(h, transform)
	writeHashPart(h, strconv.Itoa(depth))
	for _, id := range parentIDs {
		writeHashPart(h, string(id))
	}
	for _, ref := range refs {
		key, err := ref.StableKeyE()
		if err != nil {
			key = ""
		}
		writeHashPart(h, key)
	}
	sum := h.Sum(nil)
	return viewrecent.NodeID("summary-llm-" + hex.EncodeToString(sum[:16]))
}

func cloneSourceRefs(refs []views.SourceRef) []views.SourceRef {
	out := make([]views.SourceRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, cloneSourceRef(ref))
	}
	return out
}
