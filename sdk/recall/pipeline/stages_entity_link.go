package pipeline

import (
	"context"

	base "github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

const entityLinkLookupDefaultCap = 50

// EntityLinkResolver is implemented by recall's entity side store adapter.
type EntityLinkResolver interface {
	ResolveLinks(ctx context.Context, namespace string, entities []string, perEntityCap int) ([]string, error)
}

// EntityLinkLookup expands query entities into candidate memory IDs.
type EntityLinkLookup struct {
	Resolver     EntityLinkResolver
	PerEntityCap int
}

func (s EntityLinkLookup) Name() string { return "EntityLinkLookup" }

func (s EntityLinkLookup) Run(ctx context.Context, st *base.State) error {
	if s.Resolver == nil || len(st.QueryEntities) == 0 {
		return nil
	}
	ids, err := s.Resolver.ResolveLinks(ctx, st.Namespace, st.QueryEntities, s.PerEntityCap)
	if err != nil {
		return err
	}
	st.CandidateEntityIDs = ids
	return nil
}
