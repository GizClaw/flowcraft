package policy

import "context"

// OwnerOnly is the production policy for FlowCraft's single-owner model.
//
// Authorization model:
//   - The single owner principal (any user actor or super admin) is
//     allowed to append/subscribe/read every partition. There is exactly
//     one tenant; multi-realm enforcement is intentionally a no-op
//     because there are no other realms to isolate from.
//   - System / cron / webhook actors are allowed too — they originate
//     inside the process and are validated upstream by
//     ValidateSystemActor + the bootstrap wiring (cron scheduler,
//     webhook outbound sender, chat auto-ack).
//   - Agent / api_key actors are forwarded to DefaultPolicy because
//     even in single-owner mode they may carry scope restrictions; this
//     keeps the §11.1 "every subscribe/append goes through Policy"
//     invariant intact and gives api_keys a real authorization check
//     instead of a blanket allow.
//
// OwnerOnly is what bootstrap.wireHTTP injects into ServerDeps.Policy;
// its purpose is to make sure the wshub/ssehub policy hooks always have
// a non-nil decision-maker so handlers don't have to carry "if Policy
// != nil" branches (which used to silently bypass the check).
type OwnerOnly struct {
	// fallback handles agent / api_key actors when no scope or realm
	// store is configured. Defaults to a deny-all when nil.
	fallback Policy
}

// NewOwnerOnly returns the single-owner policy.
func NewOwnerOnly() *OwnerOnly { return &OwnerOnly{} }

// WithAgentFallback wires a secondary policy used for non-owner actors
// (agent / api_key). Optional — leave unset to default-deny those
// actor classes outright.
func (p *OwnerOnly) WithAgentFallback(fb Policy) *OwnerOnly {
	p.fallback = fb
	return p
}

var _ Policy = (*OwnerOnly)(nil)

func (p *OwnerOnly) ownerAllowed(actor Actor) bool {
	if actor.IsSuperAdmin() {
		return true
	}
	switch actor.Type {
	case ActorUser, ActorSystem, ActorCron, ActorWebhook:
		return true
	}
	return false
}

func (p *OwnerOnly) AllowAppend(ctx context.Context, actor Actor, env EnvelopeDraft) (Decision, error) {
	if p.ownerAllowed(actor) {
		return Allow, nil
	}
	if p.fallback != nil {
		return p.fallback.AllowAppend(ctx, actor, env)
	}
	return Deny("owner-only: actor type " + string(actor.Type) + " not permitted to append"), nil
}

func (p *OwnerOnly) AllowSubscribe(ctx context.Context, actor Actor, opts SubscribeOptions) (Decision, error) {
	if p.ownerAllowed(actor) {
		return Allow, nil
	}
	if p.fallback != nil {
		return p.fallback.AllowSubscribe(ctx, actor, opts)
	}
	return Deny("owner-only: actor type " + string(actor.Type) + " not permitted to subscribe"), nil
}

func (p *OwnerOnly) AllowRead(ctx context.Context, actor Actor, opts ReadOptions) (Decision, error) {
	if p.ownerAllowed(actor) {
		return Allow, nil
	}
	if p.fallback != nil {
		return p.fallback.AllowRead(ctx, actor, opts)
	}
	return Deny("owner-only: actor type " + string(actor.Type) + " not permitted to read"), nil
}
