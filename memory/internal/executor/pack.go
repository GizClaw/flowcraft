package executor

import (
	"context"
	"strings"

	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// PackContext loads a recent message window and appends any explicitly
// requested retrieval results in deterministic order.
func (r *Executor) PackContext(ctx context.Context, req PackContextRequest) (*ContextPack, error) {
	if r.recentWindow == nil {
		return nil, errdefs.NotAvailablef("%s: recent window is not configured", errPrefix)
	}

	window, err := r.recentWindow.Load(ctx, req.Window)
	if err != nil {
		return nil, err
	}

	pack := &ContextPack{
		Window: window,
		Items:  make([]ContextItem, 0, len(window.Messages)),
	}

	for i := range window.Messages {
		msg := window.Messages[i]
		text := renderMessageText(msg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:    ContextItemRecentMessage,
			Text:    text,
			Message: &msg,
		})
	}

	if req.SummarySearch != nil {
		resp, err := r.SearchSummaryNodes(ctx, *req.SummarySearch, req.SummaryNamespace)
		if err != nil {
			return nil, err
		}
		pack.SummaryHits = resp.Hits
		for i := range resp.Hits {
			node := resp.Hits[i].Node
			hit := resp.Hits[i].Retrieval
			text := node.Summary
			if strings.TrimSpace(text) == "" {
				continue
			}
			pack.Items = append(pack.Items, ContextItem{
				Kind:        ContextItemSummaryNode,
				Text:        text,
				SummaryNode: &node,
				Retrieval:   &hit,
			})
		}
	}

	if req.DocumentSearch != nil {
		resp, err := r.SearchDocumentChunks(ctx, *req.DocumentSearch, req.DocumentNamespace)
		if err != nil {
			return nil, err
		}
		pack.DocumentHits = resp.Hits
		for i := range resp.Hits {
			chunk := resp.Hits[i].Chunk
			hit := resp.Hits[i].Retrieval
			text := chunk.Text
			if strings.TrimSpace(text) == "" {
				continue
			}
			pack.Items = append(pack.Items, ContextItem{
				Kind:          ContextItemDocumentChunk,
				Text:          text,
				DocumentChunk: &chunk,
				Retrieval:     &hit,
			})
		}
	}

	if req.ObservationSearch != nil {
		resp, err := r.searchObservations(ctx, *req.ObservationSearch, req.ObservationNamespace)
		if err != nil {
			return nil, err
		}
		pack.ObservationHits = resp.Hits
		for i := range resp.Hits {
			observation := resp.Hits[i].Observation
			hit := resp.Hits[i].Retrieval
			text := renderObservationText(observation)
			if strings.TrimSpace(text) == "" {
				continue
			}
			pack.Items = append(pack.Items, ContextItem{
				Kind:        ContextItemObservation,
				Text:        text,
				Observation: &observation,
				Retrieval:   &hit,
			})
		}
	}

	if req.FactSearch != nil {
		resp, err := r.searchFacts(ctx, *req.FactSearch, req.FactNamespace)
		if err != nil {
			return nil, err
		}
		pack.FactHits = resp.Hits
		for i := range resp.Hits {
			record := resp.Hits[i].Fact
			hit := resp.Hits[i].Retrieval
			text := renderFactText(record)
			if strings.TrimSpace(text) == "" {
				continue
			}
			pack.Items = append(pack.Items, ContextItem{
				Kind:      ContextItemFact,
				Text:      text,
				Fact:      &record,
				Retrieval: &hit,
			})
		}
	}

	if req.FactGraphSearch != nil {
		resp, err := r.searchFactGraph(ctx, *req.FactGraphSearch, req.FactGraphNamespace)
		if err != nil {
			return nil, err
		}
		pack.FactGraphHits = resp.Hits
		for i := range resp.Hits {
			hit := resp.Hits[i].Retrieval
			if resp.Hits[i].Node != nil {
				node := *resp.Hits[i].Node
				text := renderFactGraphNodeText(node)
				if strings.TrimSpace(text) != "" {
					pack.Items = append(pack.Items, ContextItem{
						Kind:          ContextItemFactGraphNode,
						Text:          text,
						FactGraphNode: &node,
						Retrieval:     &hit,
					})
				}
			}
			if resp.Hits[i].Edge != nil {
				edge := *resp.Hits[i].Edge
				text := renderFactGraphEdgeText(edge)
				if strings.TrimSpace(text) != "" {
					pack.Items = append(pack.Items, ContextItem{
						Kind:          ContextItemFactGraphEdge,
						Text:          text,
						FactGraphEdge: &edge,
						Retrieval:     &hit,
					})
				}
			}
		}
	}

	return pack, nil
}

func renderMessageText(msg sourcemessage.Message) string {
	content := msg.Content()
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return string(msg.Role) + ": " + content
}

func renderObservationText(observation viewobservation.Observation) string {
	return renderTriple(observation.Subject, observation.Predicate, observation.Object)
}

func renderFactText(record fact.Fact) string {
	return renderTriple(record.Subject, record.Predicate, record.Object)
}

func renderFactGraphNodeText(node fact.Node) string {
	return strings.TrimSpace(node.Label)
}

func renderFactGraphEdgeText(edge fact.Edge) string {
	return renderTriple(string(edge.From), edge.Predicate, string(edge.To))
}

func renderTriple(subject, predicate, object string) string {
	parts := []string{
		strings.TrimSpace(subject),
		strings.TrimSpace(predicate),
		strings.TrimSpace(object),
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
