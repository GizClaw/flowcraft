package host

import (
	"context"
	"errors"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/policy"
)

// This file is the model half of a session's authority.
//
// Tools are enforced by the binding a call carries: the projection decides what may be
// shown and dispatch narrows the capability pocket per tool. Models have no such
// parameter — a driver reaches them through one field of its request — so the
// enforcement point is the provider the turn hands the driver: a router restricted to
// what the session's grant lists.
//
// The restriction lives in route rather than in a gate here because that is where the
// question is answered anyway: to pick a target the router must already know what the
// request names, so it is also the layer that can say "not granted" without a second
// implementation of the same rule. What this file adds is the translation back into the
// vocabulary the operator and the model read.

// modelAccess is the model access one session may use.
//
// An empty grant's model set means "every model the process serves", so the session
// gets the process's provider unchanged: a restriction that permits everything would be
// a copy pretending to be a decision.
func (a *Agents) modelAccess(binding policy.Grant) *inference.Provider {
	if len(binding.Models) == 0 || a.res.router == nil {
		return a.res.models
	}
	provider := a.res.router.Restrict(allowOf(binding.Models)).Provider()
	return denyAsPolicy(&provider)
}

// allowOf projects the grant's model entries onto the router's vocabulary.
//
// The two types are separate on purpose: policy states what a session may do, route
// states what a fleet can serve, and a conversion is cheaper than teaching the routing
// layer the permission vocabulary.
func allowOf(granted []policy.ModelGrant) []route.Allow {
	allow := make([]route.Allow, 0, len(granted))
	for _, grant := range granted {
		allow = append(allow, route.Allow{Model: grant.ModelRef, Modes: grant.Modes})
	}
	return allow
}

// denyAsPolicy restates the router's refusal in the vocabulary the rest of the platform
// speaks.
//
// A model outside the session's authority is a policy denial, and the sentence behind it
// — which model, what is allowed — is the part a reader can act on. The router cannot
// speak that vocabulary (it sits below policy), so the host translates at the one seam
// both sides share: the provider the turn hands the driver.
//
// A refusal that is not the router's — a provider failure, a timeout — passes through
// untouched, so nothing gains a classification it did not have.
func denyAsPolicy(provider *inference.Provider) *inference.Provider {
	out := *provider
	if provider.Generator != nil {
		out.Generator = policyGenerator{next: provider.Generator}
	}
	if provider.Decider != nil {
		out.Decider = policyDecider{next: provider.Decider}
	}
	return &out
}

// policyGenerator restates a refusal from either generation seam.
type policyGenerator struct {
	next inference.Generator
}

func (g policyGenerator) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	response, err := g.next.Generate(ctx, request)
	return response, asPolicyDenial(err)
}

func (g policyGenerator) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	stream, err := g.next.GenerateStream(ctx, request)
	return stream, asPolicyDenial(err)
}

// policyDecider restates a refusal from the judgment seam.
type policyDecider struct {
	next inference.Decider
}

func (d policyDecider) Decide(ctx context.Context, request inference.DecisionRequest) (inference.DecisionResponse, error) {
	response, err := d.next.Decide(ctx, request)
	return response, asPolicyDenial(err)
}

// asPolicyDenial turns the router's classification into a [policy.Denial] carrying the
// sentence that explains it.
//
// The check is [policy.CheckModels] because that is the rule: the session's model set.
// The original error stays as the cause, so a caller can still tell "refused" from
// "could not be consulted" and can read the classification the router produced.
func asPolicyDenial(err error) error {
	if err == nil || !inference.IsKind(err, inference.PolicyDenied) {
		return err
	}
	var classified *inference.Error
	if !errors.As(err, &classified) {
		return err
	}
	reason := "the model call was refused"
	if cause := errors.Unwrap(classified); cause != nil {
		// The router prefixes its sentences with the package name; a model reads the
		// statement, not the path that produced it.
		reason = strings.TrimPrefix(cause.Error(), "route: ")
	}
	return &policy.Denial{Check: policy.CheckModels, Reason: reason, Cause: err}
}
