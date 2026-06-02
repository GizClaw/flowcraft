package observation

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/retrieval"
)

const (
	MetaObservationID = "observation_id"
	MetaSpanID        = "observation_span_id"
	MetaScopeRT       = domain.MetaScopeRT
	MetaScopeUser     = domain.MetaScopeUser
	MetaScopeAgent    = domain.MetaScopeAgent
)

type Projection struct {
	index retrieval.Index
}

func NewProjection(index retrieval.Index) *Projection {
	return &Projection{index: index}
}

func (p *Projection) Name() string { return "observation" }

func (p *Projection) ProjectObservations(ctx context.Context, observations []domain.Observation) error {
	if p == nil || p.index == nil || len(observations) == 0 {
		return nil
	}
	byNS := map[string][]retrieval.Doc{}
	for _, obs := range observations {
		if obs.ID == "" || obs.Scope.RuntimeID == "" {
			continue
		}
		byNS[NamespaceFor(obs.Scope)] = append(byNS[NamespaceFor(obs.Scope)], docsForObservation(obs)...)
	}
	for ns, docs := range byNS {
		if len(docs) == 0 {
			continue
		}
		if err := p.index.Upsert(ctx, ns, docs); err != nil {
			return fmt.Errorf("observation projection upsert %s: %w", ns, err)
		}
	}
	return nil
}

func (p *Projection) RebuildObservations(ctx context.Context, scope domain.Scope, observations []domain.Observation) error {
	if p == nil || p.index == nil {
		return nil
	}
	if err := p.ClearObservationScope(ctx, scope); err != nil {
		return err
	}
	return p.ProjectObservations(ctx, observations)
}

func (p *Projection) ForgetObservations(ctx context.Context, scope domain.Scope, observationIDs []string) error {
	if p == nil || p.index == nil || len(observationIDs) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(observationIDs))
	filterValues := make([]any, 0, len(observationIDs))
	for _, id := range observationIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		filterValues = append(filterValues, id)
	}
	if len(ids) == 0 {
		return nil
	}
	deleteIDs := append([]string(nil), ids...)
	pageToken := ""
	for {
		resp, err := p.index.List(ctx, NamespaceFor(scope), retrieval.ListRequest{
			Filter:    retrieval.Filter{In: map[string][]any{MetaObservationID: filterValues}},
			PageSize:  1000,
			PageToken: pageToken,
			Project:   []string{MetaObservationID, MetaSpanID},
		})
		if err != nil {
			return err
		}
		if resp == nil {
			break
		}
		for _, doc := range resp.Items {
			if doc.ID == "" {
				continue
			}
			if _, ok := seen[doc.ID]; ok {
				continue
			}
			seen[doc.ID] = struct{}{}
			deleteIDs = append(deleteIDs, doc.ID)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return p.index.Delete(ctx, NamespaceFor(scope), deleteIDs)
}

func (p *Projection) ClearObservationScope(ctx context.Context, scope domain.Scope) error {
	if p == nil || p.index == nil {
		return nil
	}
	if dropper, ok := p.index.(interface {
		Drop(context.Context, string) error
	}); ok {
		return dropper.Drop(ctx, NamespaceFor(scope))
	}
	var ids []string
	pageToken := ""
	for {
		resp, err := p.index.List(ctx, NamespaceFor(scope), retrieval.ListRequest{PageToken: pageToken})
		if err != nil {
			return err
		}
		for _, doc := range resp.Items {
			if doc.ID != "" {
				ids = append(ids, doc.ID)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	if len(ids) == 0 {
		return nil
	}
	return p.index.Delete(ctx, NamespaceFor(scope), ids)
}

func docsForObservation(obs domain.Observation) []retrieval.Doc {
	text := strings.TrimSpace(obs.Text)
	if text == "" {
		return nil
	}
	meta := map[string]any{
		MetaObservationID: obs.ID,
		MetaScopeRT:       obs.Scope.RuntimeID,
		MetaScopeUser:     obs.Scope.UserID,
		MetaScopeAgent:    obs.Scope.AgentID,
		"kind":            string(obs.Kind),
		"source_id":       obs.SourceID,
		"session_id":      obs.SessionID,
		"message_id":      obs.MessageID,
		"role":            obs.Role,
		"speaker":         obs.Speaker,
	}
	ts := obs.ObservedAt
	if ts.IsZero() {
		ts = obs.ReceivedAt
	}
	docs := []retrieval.Doc{{
		ID:        obs.ID,
		Content:   text,
		Metadata:  meta,
		Timestamp: ts,
	}}
	for _, span := range obs.Spans {
		if span.ID == "" || strings.TrimSpace(span.Text) == "" {
			continue
		}
		spanMeta := map[string]any{}
		for k, v := range meta {
			spanMeta[k] = v
		}
		spanMeta[MetaSpanID] = span.ID
		docs = append(docs, retrieval.Doc{
			ID:        span.ID,
			Content:   span.Text,
			Metadata:  spanMeta,
			Timestamp: ts,
		})
	}
	return docs
}
