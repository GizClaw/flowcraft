package deploy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/deploy/sources"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// escalationDoc is an agent that declares what it may be widened to: a second write root inside the
// read set it already has, and a lifetime and budget for the widening.
func escalationDoc() string {
	return `
version: 1
agents:
  - name: helper
    program: { impl: graph, settings: { graph: "main" } }
    grant:
      sandbox: local
      files: { read: ["/srv/w"], write: ["/srv/w/out"] }
      limits: { idle: 30m }
      escalate:
        files: { write: ["/srv/w/out", "/srv/w/tmp"] }
        ttl: 60s
        max_per_session: 3
`
}

// bounded tells the loader that the process's "local" backend bounds writes, which is the one fact
// a declaration of escalation is checked against.
func bounded(plan deploy.ProcessPlan, bounds bool) deploy.Capabilities {
	caps := capabilities(plan)
	caps.Enforcement = map[string]sandbox.Enforcement{
		"local": {FilesystemBounds: bounds},
	}
	return caps
}

// loadPlan is the process document resolved, which every case here starts from.
func loadPlan(t *testing.T) deploy.ProcessPlan {
	t.Helper()
	plan, err := deploy.NewLoader(impls(&builds{})).Process(context.Background(),
		sources.Bytes("runtime.yaml", []byte(platformRoots(processDoc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return plan
}

// TestABackendDeclarationReachesThePlan is the registration's half of the check: a backend says what
// it can enforce, the plan carries it, and the process reports it as a capability. Without this the
// loader could not tell a boundary from a backend that has none.
func TestABackendDeclarationReachesThePlan(t *testing.T) {
	table := impls(&builds{}).Backend(
		deploy.BackendImpl("fenced", func(backendSettings, sandbox.Confine) (sandbox.Runner, error) {
			return nil, nil
		}).Enforcing(sandbox.Enforcement{FilesystemBounds: true}))
	doc := strings.Replace(processDoc, "impl: local", "impl: fenced", 1)
	plan, err := deploy.NewLoader(table).Process(context.Background(),
		sources.Bytes("runtime.yaml", []byte(platformRoots(doc))))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !plan.Backends[0].Enforcement.FilesystemBounds {
		t.Fatalf("plan = %+v, want the registration's declaration", plan.Backends[0])
	}
}

// TestADocumentThatDeclaresAnEscalationLoads is the document surface: the block is parsed into the
// grant the host reads at run time, and the loader checks it against the backend it selects.
func TestADocumentThatDeclaresAnEscalationLoads(t *testing.T) {
	plan := loadPlan(t)
	deployments, err := deploy.NewLoader(impls(&builds{})).Deployments(context.Background(),
		bounded(plan, true), sources.Bytes("agent.yaml", []byte(platformRoots(escalationDoc()))))
	if err != nil {
		t.Fatalf("Deployments: %v", err)
	}
	grant := deployments[0].Grant
	if grant.Escalate == nil {
		t.Fatal("the escalation block was dropped")
	}
	if len(grant.Escalate.Files.Write) != 2 || grant.Escalate.MaxPerSession != 3 ||
		grant.Escalate.TTL.Std().String() != "1m0s" {
		t.Fatalf("escalation = %+v, want the documented block", grant.Escalate)
	}
}

// TestAnEscalationOnABackendWithoutABoundaryIsRefused is the load-time rule that keeps the feature
// from lying: with no OS boundary there is nothing to exceed, so a widening would only tell the
// model its refused command now runs.
func TestAnEscalationOnABackendWithoutABoundaryIsRefused(t *testing.T) {
	plan := loadPlan(t)
	_, err := deploy.NewLoader(impls(&builds{})).Deployments(context.Background(),
		bounded(plan, false), sources.Bytes("agent.yaml", []byte(platformRoots(escalationDoc()))))
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable: this process cannot widen a boundary it does not draw", err)
	}
	if !strings.Contains(err.Error(), "escalate") || !strings.Contains(err.Error(), "local") {
		t.Fatalf("error %q does not name the declaration and the backend", err)
	}
}

// TestAnEscalationNeedsAReportedBackend is the same rule for a process that reports nothing: "nobody
// said it bounds writes" is not "it bounds writes".
func TestAnEscalationNeedsAReportedBackend(t *testing.T) {
	plan := loadPlan(t)
	caps := capabilities(plan) // no Enforcement at all
	_, err := deploy.NewLoader(impls(&builds{})).Deployments(context.Background(),
		caps, sources.Bytes("agent.yaml", []byte(platformRoots(escalationDoc()))))
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable for an unreported backend", err)
	}
}

// TestTheDocumentRulesOfEscalationAreValidation is the other error class: these are documents that
// are wrong on their own, so they are refused wherever they are loaded.
func TestTheDocumentRulesOfEscalationAreValidation(t *testing.T) {
	cases := map[string]struct {
		change func(string) string
		reason string
	}{
		"a declaration narrower than the grant": {
			change: func(doc string) string {
				return strings.Replace(doc, `files: { write: ["/srv/w/out", "/srv/w/tmp"] }`,
					`files: { write: ["/srv/w/tmp"] }`, 1)
			},
			reason: "does not cover the grant's own write set",
		},
		"a dimension that is not widenable": {
			change: func(doc string) string {
				return strings.Replace(doc, "ttl: 60s", "models: [{ id: { provider: \"openai\", name: \"gpt-5\" } }]\n        ttl: 60s", 1)
			},
			reason: "models",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			plan := loadPlan(t)
			doc := tc.change(escalationDoc())
			_, err := deploy.NewLoader(impls(&builds{})).Deployments(context.Background(),
				bounded(plan, true), sources.Bytes("agent.yaml", []byte(platformRoots(doc))))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("err = %v, want a validation error", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error %q, want it to mention %q", err, tc.reason)
			}
		})
	}
}

// The declaration the loader hands the host is the same value the policy layer validates, so the two
// layers cannot disagree about what "widenable" means.
var _ = policy.Escalation{}
