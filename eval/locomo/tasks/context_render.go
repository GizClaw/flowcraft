package tasks

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentityfact "github.com/GizClaw/flowcraft/memory/views/entityfact"
)

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func renderContext(pack *memory.ContextPack) string {
	if pack == nil {
		return "(no source-message memory context)"
	}
	retrieved, recent := groupedContextItems(pack)
	var sections []string
	if len(retrieved) > 0 {
		sections = append(sections, renderContextSection("Retrieved source messages", retrieved, ""))
	}
	if len(recent) > 0 {
		sections = append(sections, renderContextSection("Recent source messages", recent, "R"))
	}
	if len(sections) == 0 {
		return "(no source-message memory context)"
	}
	return strings.Join(sections, "\n\n")
}

type contextRenderEntry struct {
	item derive.ContextItem
	text string
}

func groupedContextItems(pack *memory.ContextPack) ([]contextRenderEntry, []contextRenderEntry) {
	items := renderableContextItems(pack)
	seenDiaIDs := map[string]bool{}
	retrieved := make([]contextRenderEntry, 0, len(items))
	recent := make([]contextRenderEntry, 0, len(items))
	for _, item := range items {
		if item.Retrieval == nil {
			continue
		}
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		retrieved = append(retrieved, contextRenderEntry{item: item, text: text})
		markDiaIDs(seenDiaIDs, contextItemDiaIDs(item))
	}
	for _, item := range items {
		if item.Retrieval != nil || item.Kind != derive.ContextItemRecentMessage {
			continue
		}
		text := strings.TrimSpace(item.Text)
		if text == "" || hasSeenDiaID(seenDiaIDs, contextItemDiaIDs(item)) {
			continue
		}
		recent = append(recent, contextRenderEntry{item: item, text: text})
		markDiaIDs(seenDiaIDs, contextItemDiaIDs(item))
	}
	return retrieved, recent
}

func renderContextSection(title string, entries []contextRenderEntry, labelPrefix string) string {
	lines := []string{title + ":"}
	for i, entry := range entries {
		label := fmt.Sprintf("[%d]", i+1)
		if labelPrefix != "" {
			label = fmt.Sprintf("[%s%d]", labelPrefix, i+1)
		}
		lines = append(lines, label+" "+renderContextProvenance(entry.item))
		lines = append(lines, "Text: "+singleLineContextText(entry.text))
		if i+1 < len(entries) {
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func renderContextProvenance(item derive.ContextItem) string {
	parts := []string{
		renderDiaIDProvenance(contextItemDiaIDs(item)),
	}
	for _, key := range []string{"session", "session_date_time", "session_datetime", "speaker"} {
		if value := formatContextMetadataValue(contextItemMetadataValue(item, key)); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	if seq := contextItemSeq(item); seq != "" {
		parts = append(parts, "seq="+seq)
	}
	return strings.Join(parts, " | ")
}

func renderDiaIDProvenance(ids []string) string {
	switch len(ids) {
	case 0:
		return "dia_id=unknown"
	case 1:
		return "dia_id=" + ids[0]
	default:
		return "dia_ids=" + renderDiaIDList(ids)
	}
}

func contextItemMetadataValue(item derive.ContextItem, key string) any {
	if item.Message != nil {
		if value := metadataValue(item.Message.Metadata, key); value != nil {
			return value
		}
	}
	if item.Retrieval != nil {
		if value := metadataValue(item.Retrieval.Doc.Metadata, key); value != nil {
			return value
		}
	}
	return nil
}

func contextItemSeq(item derive.ContextItem) string {
	if item.Message != nil && item.Message.Seq > 0 {
		return fmt.Sprintf("%d", item.Message.Seq)
	}
	if value := formatContextMetadataValue(contextItemMetadataValue(item, "seq")); value != "" {
		return value
	}
	return ""
}

func hasSeenDiaID(seen map[string]bool, ids []string) bool {
	for _, id := range ids {
		if seen[id] {
			return true
		}
	}
	return false
}

func markDiaIDs(seen map[string]bool, ids []string) {
	for _, id := range ids {
		seen[id] = true
	}
}

func renderDiaIDList(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	return "[" + strings.Join(ids, ",") + "]"
}

func singleLineContextText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func observedDiaIDs(pack *memory.ContextPack) map[string]bool {
	out := map[string]bool{}
	if pack == nil {
		return out
	}
	for _, item := range renderableContextItems(pack) {
		addDiaIDs(out, contextItemDiaIDs(item))
	}
	return out
}

func renderableContextItems(pack *memory.ContextPack) []derive.ContextItem {
	if pack == nil {
		return nil
	}
	out := make([]derive.ContextItem, 0, len(pack.Items))
	for _, item := range pack.Items {
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		if item.Kind == derive.ContextItemRecentMessage {
			out = append(out, item)
		}
	}
	return out
}

func contextItemSourceRefs(item derive.ContextItem) []views.SourceRef {
	switch {
	case item.SummaryNode != nil:
		return item.SummaryNode.SourceRefs
	case item.DocumentChunk != nil:
		return []views.SourceRef{item.DocumentChunk.SourceRef}
	case item.EntityFact != nil:
		return item.EntityFact.SourceRefs
	default:
		return nil
	}
}

func contextItemDiaIDs(item derive.ContextItem) []string {
	ids := map[string]bool{}
	addDiaIDs(ids, sourceRefsDiaIDs(contextItemSourceRefs(item)))
	if item.Message != nil {
		addDiaIDValue(ids, item.Message.ID)
		for _, key := range []string{
			"dia_id",
			"dia_ids",
			"source_dia_id",
			"source_dia_ids",
			"message_id",
			"message_ids",
			"source_message_id",
			"source_message_ids",
		} {
			addDiaIDValue(ids, metadataValue(item.Message.Metadata, key))
		}
	}
	if item.Retrieval != nil {
		for _, key := range []string{
			"dia_id",
			"dia_ids",
			"source_dia_id",
			"source_dia_ids",
			"message_id",
			"message_ids",
			"source_message_id",
			"source_message_ids",
		} {
			addDiaIDValue(ids, metadataValue(item.Retrieval.Doc.Metadata, key))
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func contextHitCounts(pack *memory.ContextPack) *locomoreport.QAHitCounts {
	counts := &locomoreport.QAHitCounts{}
	if pack != nil {
		counts.SummaryNode = len(pack.SummaryHits)
		counts.EntityFact = len(pack.EntityHits)
		counts.DocumentChunk = len(pack.DocumentHits)
	}
	graphFactIDs := map[string]bool{}
	graphSeedEntityIDs := map[string]bool{}
	for _, item := range renderableContextItems(pack) {
		switch item.Kind {
		case derive.ContextItemRecentMessage:
			counts.SourceMessages++
			switch strings.ToLower(formatContextMetadataValue(contextItemMetadataValue(item, qaRetrievalOriginMetadataKey))) {
			case qaRetrievalOriginDirect:
				counts.SourceDirect++
			case qaRetrievalOriginSummary:
				counts.SourceSummaryExpanded++
			case qaRetrievalOriginNeighbor:
				counts.SourceNeighborhoodExpanded++
			case qaRetrievalOriginEntity:
				counts.SourceEntityExpanded++
			case qaRetrievalOriginGraph:
				counts.SourceGraphExpanded++
				for _, id := range metadataStringValues(contextItemMetadataValue(item, qaGraphFactIDsMetadataKey)) {
					graphFactIDs[id] = true
				}
				for _, id := range metadataStringValues(contextItemMetadataValue(item, qaGraphSeedIDsMetadataKey)) {
					graphSeedEntityIDs[id] = true
				}
				paths := metadataStringValues(contextItemMetadataValue(item, qaGraphPathMetadataKey))
				counts.GraphPaths += len(paths)
				if strings.ToLower(formatContextMetadataValue(contextItemMetadataValue(item, qaGraphOriginMetadataKey))) == viewentityfact.GraphOriginBridge {
					counts.GraphBridge++
				}
			}
		case derive.ContextItemSummaryNode:
			if pack == nil || len(pack.SummaryHits) == 0 {
				counts.SummaryNode++
			}
		case derive.ContextItemDocumentChunk:
			if pack == nil || len(pack.DocumentHits) == 0 {
				counts.DocumentChunk++
			}
		case derive.ContextItemEntityFact:
			if pack == nil || len(pack.EntityHits) == 0 {
				counts.EntityFact++
			}
		}
	}
	counts.GraphFact = len(graphFactIDs)
	counts.GraphSeedEntity = len(graphSeedEntityIDs)
	return counts
}

func metadataValue(metadata map[string]any, key string) any {
	if len(metadata) == 0 {
		return nil
	}
	if value, ok := metadata[key]; ok {
		return value
	}
	for _, nestedKey := range []string{"record_metadata", "projector.record_metadata"} {
		nested, ok := metadata[nestedKey].(map[string]any)
		if !ok {
			continue
		}
		if value, ok := nested[key]; ok {
			return value
		}
	}
	return nil
}

func formatContextMetadataValue(value any) string {
	values := metadataStringValues(value)
	if len(values) == 0 {
		return ""
	}
	for i, value := range values {
		values[i] = strings.Join(strings.Fields(value), " ")
	}
	return strings.Join(values, "|")
}

func metadataStringValues(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{strings.TrimSpace(v)}
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if strings.TrimSpace(item) != "" {
				out = append(out, strings.TrimSpace(item))
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, metadataStringValues(item)...)
		}
		return out
	default:
		return []string{fmt.Sprint(v)}
	}
}

func sourceRefsDiaIDs(refs []views.SourceRef) []string {
	var out []string
	for _, ref := range refs {
		if ref.Message == nil {
			continue
		}
		if ref.Message.MessageID != "" {
			out = append(out, ref.Message.MessageID)
		}
	}
	return dedupeStrings(out)
}

func addDiaIDs(out map[string]bool, ids []string) {
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
}

func addDiaIDValue(out map[string]bool, value any) {
	for _, v := range metadataStringValues(value) {
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ';' || r == '|' || unicode.IsSpace(r)
		}) {
			if part = strings.TrimSpace(part); part != "" {
				out[part] = true
			}
		}
	}
}
