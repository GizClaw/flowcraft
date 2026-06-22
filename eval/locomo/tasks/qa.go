package tasks

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

const (
	DefaultQATopK                 = 30
	DefaultQARecentWindowMessages = 0

	qaSummaryRefsPerNode       = 3
	qaSummaryExpandedMaxSource = 8
	qaEntityRefsPerFact        = 3
	qaEntityExpandedMaxSource  = 4

	qaEntityDirectBoostWeight       = 0.35
	qaEntitySupplementMinConfidence = 0.5
	qaEntitySupplementMinRelative   = 0.5

	qaRetrievalOriginMetadataKey = "retrieval_origin"
	qaRetrievalOriginDirect      = "source_direct"
	qaRetrievalOriginSummary     = "summary_expanded"
	qaRetrievalOriginEntity      = "entity_fact_expanded"
)

func RunQA(ctx context.Context, mem *memory.System, answer llm.LLM, judge llm.LLM, scope memory.Scope, item dataset.QAItem, qaTopK int, timeout time.Duration) locomoreport.QAResult {
	row := locomoreport.QAResult{ID: item.ID, Category: item.Category, Question: item.Question, Gold: item.Answer}
	pack, err := RetrieveQAContextForDiagnostics(ctx, mem, scope, item, qaTopK)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.HitCounts = contextHitCounts(pack)
	pred, err := generateTextMessages(ctx, answer, qaAnswerMessages(item, pack), timeout)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Predicted = pred
	row.F1 = scoreQA(item, pred)
	row.EvidenceRecall = evidenceRecall(observedDiaIDs(pack), item.Evidence)
	if judge != nil {
		row.Judge = judgeQA(ctx, judge, item, pred, timeout)
	}
	return row
}

// RetrieveQAContextForDiagnostics returns the exact QA context pack used by RunQA.
// It is intended for eval analysis commands that need runtime-aligned retrieval metrics.
func RetrieveQAContextForDiagnostics(ctx context.Context, mem *memory.System, scope memory.Scope, item dataset.QAItem, qaTopK int) (*memory.ContextPack, error) {
	if qaTopK <= 0 {
		qaTopK = DefaultQATopK
	}
	searchTopK := qaTopK + qaSummaryExpandedMaxSource + qaEntityExpandedMaxSource
	pack, err := mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: strings.TrimSpace(item.Question),
		TopK:  searchTopK,
		Window: recent.WindowRequest{Budget: &recent.WindowBudget{
			MaxMessages: DefaultQARecentWindowMessages,
		}},
	})
	if err != nil {
		return nil, err
	}
	return expandQASummarySourceMessages(ctx, mem, pack, qaTopK)
}

func expandQASummarySourceMessages(ctx context.Context, mem *memory.System, pack *memory.ContextPack, qaTopK int) (*memory.ContextPack, error) {
	if pack == nil {
		return nil, nil
	}
	if qaTopK <= 0 {
		qaTopK = DefaultQATopK
	}

	recentItems := make([]derive.ContextItem, 0, len(pack.Window.Messages))
	seenRecent := map[string]bool{}
	for _, msg := range pack.Window.Messages {
		item := qaSourceMessageContextItem(msg, nil, "", "")
		key := qaSourceMessageDedupeKey(item.Message)
		if key != "" {
			if seenRecent[key] {
				continue
			}
			seenRecent[key] = true
		}
		recentItems = append(recentItems, item)
	}

	directCandidates := make([]derive.ContextItem, 0, len(pack.MessageHits))
	for _, hit := range pack.MessageHits {
		item := qaSourceMessageContextItem(hit.Message, &hit.Retrieval, qaRetrievalOriginDirect, "")
		directCandidates = append(directCandidates, item)
	}

	summaryCandidates, err := hydrateQASummaryExpandedItems(ctx, mem, pack, seenRecent)
	if err != nil {
		return nil, err
	}
	entitySignals := collectQAEntitySourceSignals(pack, seenRecent)
	rerankQADirectCandidates(directCandidates, entitySignals)

	finalItems := make([]derive.ContextItem, 0, DefaultQARecentWindowMessages+qaSummaryExpandedMaxSource+qaEntityExpandedMaxSource+qaTopK)
	seenFinal := map[string]bool{}
	appendItem := func(item derive.ContextItem) bool {
		key := qaSourceMessageDedupeKey(item.Message)
		if key != "" {
			if seenFinal[key] {
				return false
			}
			seenFinal[key] = true
		}
		finalItems = append(finalItems, item)
		return true
	}
	for _, item := range recentItems {
		appendItem(item)
	}
	summaryAdded := 0
	for _, item := range summaryCandidates {
		if summaryAdded >= qaSummaryExpandedMaxSource {
			break
		}
		if appendItem(item) {
			summaryAdded++
		}
	}
	directAdded := 0
	for _, item := range directCandidates {
		if directAdded >= qaTopK {
			break
		}
		if appendItem(item) {
			directAdded++
		}
	}
	entityCandidates, err := hydrateQAEntitySupplementItems(ctx, mem, entitySignals, seenFinal)
	if err != nil {
		return nil, err
	}
	entityAdded := 0
	for _, item := range entityCandidates {
		if entityAdded >= qaEntityExpandedMaxSource {
			break
		}
		if appendItem(item) {
			entityAdded++
		}
	}
	for _, item := range pack.Items {
		if isQASourceBackedReserveItem(item) {
			appendItem(item)
		}
	}

	out := *pack
	out.Items = finalItems
	return &out, nil
}

func hydrateQASummaryExpandedItems(ctx context.Context, mem *memory.System, pack *memory.ContextPack, seenBlocked map[string]bool) ([]derive.ContextItem, error) {
	if len(pack.SummaryHits) == 0 {
		return nil, nil
	}
	store := mem.MessageStore()
	if store == nil {
		return nil, fmt.Errorf("locomo qa: summary expansion requires message store")
	}
	items := make([]derive.ContextItem, 0, qaSummaryExpandedMaxSource)
	seenExpanded := map[string]bool{}
	for _, hit := range pack.SummaryHits {
		perNode := 0
		for _, ref := range hit.Node.SourceRefs {
			if len(items) >= qaSummaryExpandedMaxSource {
				return items, nil
			}
			if perNode >= qaSummaryRefsPerNode {
				break
			}
			if ref.Kind != views.SourceMessage || ref.Message == nil {
				continue
			}
			msg, ok, err := store.Get(ctx, ref.Message.ConversationID, ref.Message.MessageID)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("locomo qa: hydrate summary source ref %q/%q: message not found", ref.Message.ConversationID, ref.Message.MessageID)
			}
			key := qaSourceMessageDedupeKey(&msg)
			if key != "" {
				if seenBlocked[key] || seenExpanded[key] {
					continue
				}
				seenExpanded[key] = true
			}
			items = append(items, qaSourceMessageContextItem(msg, &hit.Retrieval, qaRetrievalOriginSummary, string(hit.Node.ID)))
			perNode++
		}
	}
	return items, nil
}

type qaEntitySourceSignal struct {
	SourceRef    views.SourceRef
	Retrieval    retrieval.Hit
	FactID       string
	Score        float64
	FirstHitRank int
}

func collectQAEntitySourceSignals(pack *memory.ContextPack, seenBlocked map[string]bool) map[string]qaEntitySourceSignal {
	if pack == nil || len(pack.EntityHits) == 0 {
		return nil
	}
	signals := map[string]qaEntitySourceSignal{}
	for hitRank, hit := range pack.EntityHits {
		confidence := hit.Fact.Confidence
		if confidence <= 0 {
			confidence = 0.8
		}
		if confidence > 1 {
			confidence = 1
		}
		if confidence < qaEntitySupplementMinConfidence {
			continue
		}
		baseScore := confidence / float64(hitRank+1)
		for refRank, ref := range hit.Fact.SourceRefs {
			if refRank >= qaEntityRefsPerFact {
				break
			}
			if ref.Kind != views.SourceMessage || ref.Message == nil {
				continue
			}
			key := qaSourceMessageRefDedupeKey(ref.Message)
			if key == "" || seenBlocked[key] {
				continue
			}
			score := baseScore / float64(refRank+1)
			existing, ok := signals[key]
			if ok && (existing.Score > score || (existing.Score == score && existing.FirstHitRank <= hitRank)) {
				continue
			}
			signals[key] = qaEntitySourceSignal{
				SourceRef:    ref,
				Retrieval:    retrieval.CloneHit(hit.Retrieval),
				FactID:       string(hit.Fact.ID),
				Score:        score,
				FirstHitRank: hitRank,
			}
		}
	}
	return signals
}

func rerankQADirectCandidates(candidates []derive.ContextItem, entitySignals map[string]qaEntitySourceSignal) {
	if len(candidates) < 2 || len(entitySignals) == 0 {
		return
	}
	type rankedCandidate struct {
		item  derive.ContextItem
		rank  int
		score float64
	}
	ranked := make([]rankedCandidate, len(candidates))
	for i, item := range candidates {
		ranked[i] = rankedCandidate{
			item:  item,
			rank:  i,
			score: qaDirectCandidateFusionScore(item, i, entitySignals),
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i]
		right := ranked[j]
		if left.score == right.score {
			return left.rank < right.rank
		}
		return left.score > right.score
	})
	for i, candidate := range ranked {
		candidates[i] = candidate.item
	}
}

func qaDirectCandidateFusionScore(item derive.ContextItem, rank int, entitySignals map[string]qaEntitySourceSignal) float64 {
	score := 1 / float64(rank+1)
	key := qaSourceMessageDedupeKey(item.Message)
	if signal, ok := entitySignals[key]; ok {
		score += qaEntityDirectBoostWeight * signal.Score
	}
	return score
}

func hydrateQAEntitySupplementItems(ctx context.Context, mem *memory.System, signals map[string]qaEntitySourceSignal, seenBlocked map[string]bool) ([]derive.ContextItem, error) {
	if len(signals) == 0 {
		return nil, nil
	}
	store := mem.MessageStore()
	if store == nil {
		return nil, fmt.Errorf("locomo qa: entity expansion requires message store")
	}
	items := make([]derive.ContextItem, 0, qaEntityExpandedMaxSource)
	seenExpanded := map[string]bool{}
	ranked := make([]qaEntitySourceSignal, 0, len(signals))
	maxScore := 0.0
	for _, signal := range signals {
		ranked = append(ranked, signal)
		if signal.Score > maxScore {
			maxScore = signal.Score
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].FirstHitRank < ranked[j].FirstHitRank
		}
		return ranked[i].Score > ranked[j].Score
	})
	minScore := maxScore * qaEntitySupplementMinRelative
	for _, signal := range ranked {
		if len(items) >= qaEntityExpandedMaxSource {
			return items, nil
		}
		if signal.Score < minScore {
			break
		}
		if signal.SourceRef.Message == nil {
			continue
		}
		msgRef := signal.SourceRef.Message
		msg, ok, err := store.Get(ctx, msgRef.ConversationID, msgRef.MessageID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("locomo qa: hydrate entity fact source ref %q/%q: message not found", msgRef.ConversationID, msgRef.MessageID)
		}
		key := qaSourceMessageDedupeKey(&msg)
		if key != "" {
			if seenBlocked[key] || seenExpanded[key] {
				continue
			}
			seenExpanded[key] = true
		}
		items = append(items, qaSourceMessageContextItem(msg, &signal.Retrieval, qaRetrievalOriginEntity, signal.FactID))
	}
	return items, nil
}

func isQASourceBackedReserveItem(item derive.ContextItem) bool {
	if item.Kind != derive.ContextItemRecentMessage || item.Message == nil {
		return false
	}
	value, ok := item.Message.Metadata["source_backed_reserve"]
	if !ok {
		return false
	}
	enabled, ok := value.(bool)
	return ok && enabled
}

func qaSourceMessageContextItem(msg sourcemessage.Message, hit *retrieval.Hit, origin, provenanceID string) derive.ContextItem {
	msg.Metadata = maps.Clone(msg.Metadata)
	if origin != "" {
		if msg.Metadata == nil {
			msg.Metadata = map[string]any{}
		}
		msg.Metadata[qaRetrievalOriginMetadataKey] = origin
	}
	if provenanceID != "" {
		if msg.Metadata == nil {
			msg.Metadata = map[string]any{}
		}
		switch origin {
		case qaRetrievalOriginSummary:
			msg.Metadata["summary_node_id"] = provenanceID
		case qaRetrievalOriginEntity:
			msg.Metadata["entity_fact_id"] = provenanceID
		}
	}
	item := derive.ContextItem{
		Kind:    derive.ContextItemRecentMessage,
		Text:    renderQASourceMessageText(msg),
		Message: &msg,
	}
	if hit != nil {
		cloned := retrieval.CloneHit(*hit)
		if cloned.Doc.Metadata == nil {
			cloned.Doc.Metadata = map[string]any{}
		}
		if origin != "" {
			cloned.Doc.Metadata[qaRetrievalOriginMetadataKey] = origin
		}
		if provenanceID != "" {
			switch origin {
			case qaRetrievalOriginSummary:
				cloned.Doc.Metadata["summary_node_id"] = provenanceID
			case qaRetrievalOriginEntity:
				cloned.Doc.Metadata["entity_fact_id"] = provenanceID
			}
		}
		item.Retrieval = &cloned
	}
	return item
}

func renderQASourceMessageText(msg sourcemessage.Message) string {
	content := strings.TrimSpace(msg.Content())
	if content == "" {
		return ""
	}
	return string(msg.Role) + ": " + content
}

func qaSourceMessageDedupeKey(msg *sourcemessage.Message) string {
	if msg == nil {
		return ""
	}
	if msg.ConversationID != "" && msg.ID != "" {
		return "message:" + msg.ConversationID + ":" + msg.ID
	}
	if msg.ID != "" {
		return "dia:" + msg.ID
	}
	for _, key := range []string{"dia_id", "message_id", "source_message_id"} {
		values := metadataStringValues(metadataValue(msg.Metadata, key))
		if len(values) > 0 {
			return "dia:" + values[0]
		}
	}
	return ""
}

func qaSourceMessageRefDedupeKey(ref *views.MessageSourceRef) string {
	if ref == nil {
		return ""
	}
	if ref.ConversationID != "" && ref.MessageID != "" {
		return "message:" + ref.ConversationID + ":" + ref.MessageID
	}
	if ref.MessageID != "" {
		return "dia:" + ref.MessageID
	}
	return ""
}
