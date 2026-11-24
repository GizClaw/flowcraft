package deploy_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/policy"
)

// deployment is one agent as a document resolves it, with only the facts the fingerprint
// reads.
func deployment(name, program string, grant policy.Grant, hooks ...string) deploy.Deployment {
	dep := deploy.Deployment{
		Name:    name,
		Program: deploy.PlannedProgram{Impl: program},
		Grant:   grant,
	}
	for _, hook := range hooks {
		dep.Hooks = append(dep.Hooks, deploy.PlannedHook{Impl: hook})
	}
	return dep
}

// TestFingerprintIsReproducible is the property two processes need: the same deployment set,
// composed by anyone, hashes the same. Without it a checkpoint written by one deployment of a
// document is unreadable to the next deployment of the same document.
func TestFingerprintIsReproducible(t *testing.T) {
	set := func() []deploy.Deployment {
		return []deploy.Deployment{
			deployment("forge", "graph", policy.Grant{Sandbox: "local"}, "audit"),
			deployment("review", "a2a", policy.Grant{Sandbox: "local"}),
		}
	}
	first := deploy.Fingerprint(set())
	second := deploy.Fingerprint(set())
	if first == "" || first != second {
		t.Fatalf("fingerprints differ: %q vs %q", first, second)
	}
	if deploy.Fingerprint(nil) == first {
		t.Fatal("an empty set hashed like a deployment")
	}
}

// TestFingerprintCoversWhatResumeDependsOn pins one fact per part: a resume hands a task to a
// driver under a grant, so a change to either must be a different deployment. Each case is a
// real way to break a resumed run.
func TestFingerprintCoversWhatResumeDependsOn(t *testing.T) {
	base := func() []deploy.Deployment {
		return []deploy.Deployment{
			deployment("forge", "graph", policy.Grant{Sandbox: "local"}, "audit"),
			deployment("review", "a2a", policy.Grant{Sandbox: "local"}),
		}
	}
	reference := deploy.Fingerprint(base())
	cases := map[string]func([]deploy.Deployment) []deploy.Deployment{
		"program": func(deps []deploy.Deployment) []deploy.Deployment {
			deps[0].Program.Impl = "a2a"
			return deps
		},
		"grant": func(deps []deploy.Deployment) []deploy.Deployment {
			deps[0].Grant.Sandbox = "bwrap"
			return deps
		},
		"hook": func(deps []deploy.Deployment) []deploy.Deployment {
			deps[0].Hooks = nil
			return deps
		},
		"name": func(deps []deploy.Deployment) []deploy.Deployment {
			deps[1].Name = "critic"
			return deps
		},
		"order": func(deps []deploy.Deployment) []deploy.Deployment {
			return []deploy.Deployment{deps[1], deps[0]}
		},
	}
	for name, mutate := range cases {
		if got := deploy.Fingerprint(mutate(base())); got == reference {
			t.Errorf("changing the %s left the fingerprint alone", name)
		}
	}
}

// TestFingerprintSeparatesItsParts is the collision the length prefix prevents: without a
// separator, agent "ab" running program "c" and agent "a" running "bc" hash the same bytes,
// and a task could resume under the wrong deployment.
func TestFingerprintSeparatesItsParts(t *testing.T) {
	left := deploy.Fingerprint([]deploy.Deployment{deployment("ab", "c", policy.Grant{})})
	right := deploy.Fingerprint([]deploy.Deployment{deployment("a", "bc", policy.Grant{})})
	if left == right {
		t.Fatal("two different deployment sets hashed alike")
	}
}

// TestFingerprintIsWhatTheCheckpointRecords keeps the value's type honest: a checkpoint
// compares an agent.Fingerprint, so the function must produce that type rather than a string
// every caller converts — and converts wrong, once.
func TestFingerprintIsWhatTheCheckpointRecords(t *testing.T) {
	asCheckpointField := func(fingerprint agent.Fingerprint) agent.Fingerprint { return fingerprint }
	fingerprint := asCheckpointField(deploy.Fingerprint([]deploy.Deployment{
		deployment("forge", "graph", policy.Grant{}),
	}))
	if fingerprint == "" {
		t.Fatal("the fingerprint is empty")
	}
}
