package host

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

// This file is the model half of a session's authority.
//
// Tools are enforced by the binding a call carries: the projection decides what
// may be shown and dispatch narrows the capability pocket per tool. Models have
// no such parameter — a driver reaches them through one field of its request — so
// the enforcement point is the provider itself: the turn hands the driver a
// provider that already knows what this session may call.
//
// Wrapping is complete rather than partial because every seam of
// [inference.Provider] that spends a model carries one: a generation, an
// embedding and a judgment all name a [model.ModelRef], so none of them can reach
// a provider whose allow-list has not been consulted.

// modelAccess is the model access one session may use.
//
// An empty grant's model set means "every model the process serves", so the
// session gets the process's routed provider unwrapped: a wrapper that forwarded
// every call would be the cost of pretending otherwise.
func (a *Agents) modelAccess(binding policy.Grant) *inference.Provider {
	if len(binding.Models) == 0 {
		return a.res.models
	}
	return gateModels(a.res.models, binding)
}

// gateModels returns base with every model seam filtered by grant.
//
// It returns a copy: the process's provider is shared, and a session that
// reassigned its field would narrow every other session too.
func gateModels(base *inference.Provider, grant policy.Grant) *inference.Provider {
	if base == nil || len(grant.Models) == 0 {
		return base
	}
	out := *base
	if base.Generator != nil {
		out.Generator = gatedGenerator{next: base.Generator, grant: grant}
	}
	if base.Embedder != nil {
		out.Embedder = gatedEmbedder{next: base.Embedder, grant: grant}
	}
	if base.Decider != nil {
		out.Decider = gatedDecider{next: base.Decider, grant: grant}
	}
	if base.Catalog != nil {
		// The catalog is a permission too: what a session cannot call it also
		// cannot enumerate, or a driver would offer its model the names of
		// models the next call refuses.
		out.Catalog = gatedCatalog{next: base.Catalog, grant: grant}
	}
	return &out
}

// checkDeclaredModels refuses an open whose authority excludes something the program says it will
// call.
//
// The gate in this file already refuses every such call, but it refuses it in the middle of a turn:
// for a program whose default model is outside the session's authority, that is every turn, and the
// failure reads like a broken model rather than a permission that was never granted. Asking the
// program up front turns it into one honest error at open.
//
// A program that cannot say (no [agent.ModelDeclarer]) is not refused here; the gate remains.
func checkDeclaredModels(agentName string, program agent.Program, grant policy.Grant) error {
	if len(grant.Models) == 0 {
		return nil
	}
	declarer, ok := program.(agent.ModelDeclarer)
	if !ok {
		return nil
	}
	for _, ref := range declarer.DeclaredModels() {
		if grant.AllowsModel(ref) {
			continue
		}
		return &policy.Denial{
			Check: policy.CheckModels,
			Reason: "the program calls " + modelName(ref) + ", which this session is not granted" +
				" (it may call " + grantedModels(grant.Models) + ")",
		}
	}
	return nil
}

// gatedGenerator refuses a generation the session may not run.
type gatedGenerator struct {
	next  inference.Generator
	grant policy.Grant
}

func (g gatedGenerator) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	if err := modelAllowed(g.grant, request.Model); err != nil {
		return inference.GenerateResponse{}, err
	}
	return g.next.Generate(ctx, request)
}

func (g gatedGenerator) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	if err := modelAllowed(g.grant, request.Model); err != nil {
		return nil, err
	}
	return g.next.GenerateStream(ctx, request)
}

// gatedEmbedder refuses an embedding the session may not run.
type gatedEmbedder struct {
	next  inference.Embedder
	grant policy.Grant
}

func (g gatedEmbedder) Embed(ctx context.Context, request inference.EmbedRequest) (inference.EmbedResponse, error) {
	if err := modelAllowed(g.grant, request.Model); err != nil {
		return inference.EmbedResponse{}, err
	}
	return g.next.Embed(ctx, request)
}

// gatedDecider refuses a judgment the session may not run.
type gatedDecider struct {
	next  inference.Decider
	grant policy.Grant
}

func (g gatedDecider) Decide(ctx context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	if err := modelAllowed(g.grant, request.Model); err != nil {
		return inference.DecisionResponse{}, err
	}
	return g.next.Decide(ctx, request)
}

// gatedCatalog shows only the models the session may call.
type gatedCatalog struct {
	next  inference.Catalog
	grant policy.Grant
}

func (g gatedCatalog) Models() []model.ModelDescriptor {
	declared := g.next.Models()
	visible := make([]model.ModelDescriptor, 0, len(declared))
	for _, descriptor := range declared {
		if g.grant.AllowsModel(model.ModelRef{ID: descriptor.ID}) {
			visible = append(visible, descriptor)
		}
	}
	return visible
}

func (g gatedCatalog) Describe(id model.ModelID) (model.ModelDescriptor, bool) {
	descriptor, found := g.next.Describe(id)
	if !found || !g.grant.AllowsModel(model.ModelRef{ID: id}) {
		return model.ModelDescriptor{}, false
	}
	return descriptor, true
}

// modelAllowed is the refusal a filtered seam returns.
//
// It is a [policy.Denial] rather than a plain error so the classification is the
// same one a refused tool call gets: the turn fails as a policy refusal, and an
// adapter mapping it to a status does not have to invent a second vocabulary.
func modelAllowed(grant policy.Grant, ref model.ModelRef) error {
	if grant.AllowsModel(ref) {
		return nil
	}
	return &policy.Denial{
		Check:  policy.CheckModels,
		Reason: "model " + modelName(ref) + " is not granted; this session may call " + grantedModels(grant.Models),
	}
}

// grantedModels renders the allowed set for a model-facing message.
func grantedModels(refs []model.ModelRef) string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, modelName(ref))
	}
	return strings.Join(names, ", ")
}

// modelName renders one ref the way a document writes it.
func modelName(ref model.ModelRef) string {
	name := ref.ID.Provider + ":" + ref.ID.Name
	if ref.Profile != "" {
		name = ref.Profile + "/" + name
	}
	return name
}
