package delegation_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/correlation"
	"github.com/GizClaw/flowcraft/core/delegation"
)

// The lineage is a view over the run identity rather than a second context
// value, so two properties have to hold: stamping it must not erase the identity
// a deployment attached, and a run that was never delegated must report no
// lineage at all (CallFrom's "absent means the root caller").
func TestCallIsAViewOverTheRunIdentity(t *testing.T) {
	plain := correlation.WithRun(context.Background(), correlation.Run{
		SessionID: "s1",
		AgentName: "lead",
	})
	if call, ok := delegation.CallFrom(plain); ok || call != (delegation.Call{}) {
		t.Fatalf("CallFrom(undelegated run) = %+v, %v", call, ok)
	}
	if depth := delegation.DepthFromContext(plain); depth != 0 {
		t.Fatalf("DepthFromContext(undelegated run) = %d", depth)
	}

	stamped := delegation.WithCall(plain, delegation.Call{Depth: 1, ChainID: "chain-1"})
	run, ok := correlation.From(stamped)
	if !ok || run.SessionID != "s1" || run.AgentName != "lead" {
		t.Fatalf("WithCall erased the identity: %+v", run)
	}
	call, ok := delegation.CallFrom(stamped)
	if !ok || call.Depth != 1 || call.ChainID != "chain-1" {
		t.Fatalf("CallFrom = %+v, %v", call, ok)
	}
	if depth := delegation.DepthFromContext(stamped); depth != 1 {
		t.Fatalf("DepthFromContext = %d", depth)
	}

	// A depth-only stamp is still lineage: the chain id is filled in by the
	// service when the chain starts, not by the caller that only knows the depth.
	depthOnly := delegation.WithCall(plain, delegation.Call{Depth: 3})
	if call, ok := delegation.CallFrom(depthOnly); !ok || call.Depth != 3 {
		t.Fatalf("CallFrom(depth only) = %+v, %v", call, ok)
	}
}

// The direction the fold depends on: a host stamps the session, tenant and
// manifest on a context that already carries lineage (a delegated child is
// opened by the deployment like any other session), and neither side may erase
// the other.
func TestIdentityStampKeepsTheInheritedLineage(t *testing.T) {
	child := delegation.WithCall(context.Background(), delegation.Call{Depth: 2, ChainID: "chain-1"})
	ctx := correlation.WithRun(child, correlation.Run{
		SessionID: "s-child",
		TenantID:  "acme",
		AgentName: "worker",
	})

	run, ok := correlation.From(ctx)
	if !ok || run.SessionID != "s-child" || run.TenantID != "acme" || run.AgentName != "worker" {
		t.Fatalf("identity = %+v, ok = %v", run, ok)
	}
	if run.DelegationDepth != 2 || run.ChainID != "chain-1" {
		t.Fatalf("lineage erased by the identity stamp: depth %d chain %q",
			run.DelegationDepth, run.ChainID)
	}
	if call, ok := delegation.CallFrom(ctx); !ok || call.Depth != 2 || call.ChainID != "chain-1" {
		t.Fatalf("CallFrom after the identity stamp = %+v, %v", call, ok)
	}
}
