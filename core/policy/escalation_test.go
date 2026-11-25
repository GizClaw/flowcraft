package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/policy"
)

// confined is a grant that uses all three widenable dimensions, so a widening has
// something to widen: a write set inside a read set, an allow-list posture and an
// environment allow-list, with the declaration above each of them.
func confined() policy.Grant {
	out := pathtest.Clean("srv/w/out")
	tmp := pathtest.Clean("srv/w/tmp")
	return policy.Grant{
		Sandbox: "bwrap",
		Files:   policy.Files{Read: []string{pathtest.Clean("srv/w")}, Write: []string{out}},
		Net:     policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com"}},
		Exec:    policy.ExecPolicy{Env: policy.EnvPolicy{Allow: []string{"HOME"}}},
		// The sandbox is not a widening dimension; naming one keeps the fixture
		// shaped like the deployments the declaration exists for.
		Escalate: &policy.Escalation{
			Files: policy.Files{Write: []string{out, tmp}},
			Net: &policy.NetPolicy{
				Mode:       policy.NetAllowList,
				AllowHosts: []string{"api.example.com", "cdn.example.com"},
			},
			Exec:          &policy.ExecPolicy{Env: policy.EnvPolicy{Allow: []string{"HOME", "PATH"}}},
			TTL:           policy.Duration(time.Minute),
			MaxPerSession: 3,
		},
	}
}

// TestWidenOnlyGoesForward is the runtime invariant: a grant may be widened, but
// only to something that keeps everything the session had and stays inside what
// the deployment declared. Each refusal is a denial with the escalation check,
// which is what the host counts and renders.
func TestWidenOnlyGoesForward(t *testing.T) {
	bind := normalized(t, confined())
	declaration := bind.Escalate
	out, tmp := pathtest.Clean("srv/w/out"), pathtest.Clean("srv/w/tmp")
	secret := pathtest.Clean("srv/w/secret")

	cases := map[string]struct {
		from   func(*policy.Grant)
		to     func(policy.Grant) policy.Grant
		check  policy.Check
		reason string
	}{
		"the whole declaration": {
			to: func(g policy.Grant) policy.Grant { return declaration.Widest(g) },
		},
		"one dimension at a time": {
			to: func(g policy.Grant) policy.Grant {
				g.Files.Write = []string{out, tmp}
				return g
			},
		},
		"a grant between bind and declaration": {
			to: func(g policy.Grant) policy.Grant {
				widest := declaration.Widest(g)
				widest.Net = policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com"}}
				return widest
			},
		},
		"dropping a root the bind already had": {
			to: func(g policy.Grant) policy.Grant {
				g.Files.Write = []string{tmp}
				return g
			},
			check:  policy.CheckEscalation,
			reason: "does not keep every root",
		},
		"a root outside the declaration": {
			to: func(g policy.Grant) policy.Grant {
				g.Files.Write = []string{out, tmp, secret}
				return g
			},
			check:  policy.CheckEscalation,
			reason: "does not declare",
		},
		"a host outside the declaration": {
			to: func(g policy.Grant) policy.Grant {
				g.Net = policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com", "evil.example.com"}}
				return g
			},
			check:  policy.CheckEscalation,
			reason: "network authority",
		},
		"a variable outside the declaration": {
			to: func(g policy.Grant) policy.Grant {
				g.Exec.Env.Allow = []string{"HOME", "PATH", "SSH_AUTH_SOCK"}
				return g
			},
			check:  policy.CheckEscalation,
			reason: "environment",
		},
		"a dimension that is not widenable": {
			to: func(g policy.Grant) policy.Grant {
				g.Resources.MemoryBytes = 1 << 30
				return g
			},
			check:  policy.CheckEscalation,
			reason: "only files.write, net and exec.env",
		},
		"a network change nothing declared": {
			// The declaration leaves the network alone, so the bind's posture is
			// the most there is and any change to it is outside the declaration.
			from: func(g *policy.Grant) { g.Escalate.Net = nil },
			to: func(g policy.Grant) policy.Grant {
				g.Net = policy.NetPolicy{
					Mode:       policy.NetAllowList,
					AllowHosts: []string{"api.example.com", "cdn.example.com"},
				}
				return g
			},
			check:  policy.CheckEscalation,
			reason: "network posture the escalation does not declare",
		},
		"no declaration at all": {
			from:   func(g *policy.Grant) { g.Escalate = nil },
			to:     func(g policy.Grant) policy.Grant { return g },
			check:  policy.CheckEscalation,
			reason: "no escalation is declared",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			from := bind
			from.Escalate = from.Escalate.Clone()
			if tc.from != nil {
				tc.from(&from)
			}
			got, err := from.Widen(tc.to(from))
			if tc.check == "" {
				if err != nil {
					t.Fatalf("Widen: %v", err)
				}
				if got.Escalate != nil {
					t.Fatal("the widened grant carries the declaration: it is a session fact, not a ceiling")
				}
				if len(got.Files.Write) == 0 {
					t.Fatalf("widened = %+v, want the declared write set", got.Files)
				}
				return
			}
			denial, ok := policy.AsDenial(err)
			if !ok {
				t.Fatalf("Widen = %v, want a denial", err)
			}
			if denial.Check != tc.check {
				t.Fatalf("check = %q, want %q", denial.Check, tc.check)
			}
			if !strings.Contains(denial.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", denial.Reason, tc.reason)
			}
			if !errdefs.IsPolicyDenied(err) {
				t.Fatalf("err = %v, want the classification to survive", err)
			}
		})
	}
}

// TestWidenSplitsTheThreeDimensions is what an approval is asked about: the
// dimensions that changed, with their values. It never renders a command, because
// it is never given one.
func TestWidenSplitsTheThreeDimensions(t *testing.T) {
	bind := normalized(t, confined())
	widest := bind.Escalate.Widest(bind)

	changes := policy.Diff(bind, widest)
	got := make([]string, 0, len(changes))
	for _, change := range changes {
		got = append(got, change.Dimension)
		if len(change.Before) == len(change.After) {
			t.Fatalf("%s: before and after are the same size, so nothing widened: %+v", change.Dimension, change)
		}
		for _, line := range append(append([]string(nil), change.Before...), change.After...) {
			if strings.Contains(line, "run ") || strings.Contains(line, " -rf") {
				t.Fatalf("%s rendered a command line: %q", change.Dimension, line)
			}
		}
	}
	if strings.Join(got, ",") != "files.write,net,exec.env" {
		t.Fatalf("dimensions = %v, want the three widenable ones in order", got)
	}

	// A widening that only touches files reports only files.
	only := bind
	only.Files.Write = widest.Files.Write
	if changes := policy.Diff(bind, only); len(changes) != 1 || changes[0].Dimension != "files.write" {
		t.Fatalf("changes = %+v, want only files.write", changes)
	}
	// And nothing changed is nothing to show.
	if changes := policy.Diff(bind, bind); len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// TestEscalationLoadTimeRules are the two rules a document can break on its own:
// a declaration may only carry the widenable dimensions, and it may never be
// narrower than the grant it sits on. Both are validation errors, which is what
// makes them load-time rather than first-command failures.
func TestEscalationLoadTimeRules(t *testing.T) {
	out := pathtest.Clean("srv/w/out")

	cases := map[string]struct {
		change func(*policy.Escalation)
		reason string
	}{
		"declaration narrower than the write set": {
			change: func(e *policy.Escalation) { e.Files.Write = nil },
			reason: "does not cover the grant's own write set",
		},
		"writable root outside the read set": {
			change: func(e *policy.Escalation) {
				e.Files.Write = []string{out, pathtest.Clean("etc")}
			},
			reason: "outside the grant's read set",
		},
		"a declaration read set": {
			change: func(e *policy.Escalation) { e.Files.Read = []string{pathtest.Clean("srv")} },
			reason: "files.read is not escalatable",
		},
		"a read-only declaration": {
			change: func(e *policy.Escalation) { e.Files.ReadOnly = true },
			reason: "files.readonly is not escalatable",
		},
		"an execution timeout": {
			change: func(e *policy.Escalation) { e.Exec.Timeout = policy.Duration(time.Minute) },
			reason: "may only carry env",
		},
		"an injected value": {
			change: func(e *policy.Escalation) {
				e.Exec.Env.Inject = map[string]string{"TOKEN": "secret"}
			},
			reason: "inject is not escalatable",
		},
		"an exec block that names nothing": {
			change: func(e *policy.Escalation) { e.Exec = &policy.ExecPolicy{} },
			reason: "exec.env.allow must name the variables",
		},
		"an environment narrower than the grant": {
			change: func(e *policy.Escalation) {
				e.Exec = &policy.ExecPolicy{Env: policy.EnvPolicy{Allow: []string{}}}
			},
			reason: "does not cover the grant's own environment",
		},
		"a network posture narrower than the grant": {
			change: func(e *policy.Escalation) {
				e.Net = &policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"cdn.example.com"}}
			},
			reason: "does not cover the grant's own posture",
		},
		"a network posture that denies": {
			change: func(e *policy.Escalation) { e.Net = &policy.NetPolicy{Mode: policy.NetDenyAll} },
			reason: "does not cover the grant's own posture",
		},
		"no lifetime": {
			change: func(e *policy.Escalation) { e.TTL = 0 },
			reason: "ttl must be a positive duration",
		},
		"no budget": {
			change: func(e *policy.Escalation) { e.MaxPerSession = 0 },
			reason: "max_per_session must be positive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			grant := confined()
			tc.change(grant.Escalate)
			_, err := grant.Normalized()
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Normalized = %v, want a validation error", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.reason)
			}
		})
	}

	// The declaration itself is valid, so the fixture is the control.
	if _, err := confined().Normalized(); err != nil {
		t.Fatalf("the fixture is not loadable: %v", err)
	}
	// A declaration on a read-only grant is the same contradiction as a write set
	// that lists nothing: there is no write to widen.
	readonly := confined()
	readonly.Files = policy.Files{Read: []string{pathtest.Clean("srv/w")}, ReadOnly: true}
	if _, err := readonly.Normalized(); err == nil || !strings.Contains(err.Error(), "read-only grant") {
		t.Fatalf("Normalized = %v, want a read-only refusal", err)
	}
	// A grant whose writes are already unrestricted has nothing to escalate, and a
	// declaration that listed roots would shrink it.
	unrestricted := confined()
	unrestricted.Files = policy.Files{}
	if _, err := unrestricted.Normalized(); err == nil ||
		!strings.Contains(err.Error(), "already unrestricted") {
		t.Fatalf("Normalized = %v, want an unrestricted-write refusal", err)
	}
}

// normalized is the value the host works with: a validated ceiling.
func normalized(t *testing.T, grant policy.Grant) policy.Grant {
	t.Helper()
	out, err := grant.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	return out
}
