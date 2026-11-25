package policy_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/policy"
)

// confined is the authority one session holds: a write set inside a read set, an allow-list
// network posture and an environment allow-list — the three dimensions an approval may widen.
func confined() policy.Grant {
	return policy.Grant{
		Sandbox: "bwrap",
		Files:   policy.Files{Read: []string{pathtest.Clean("srv/w")}, Write: []string{pathtest.Clean("srv/w/out")}},
		Net:     policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com"}},
		Exec:    policy.ExecPolicy{Env: policy.EnvPolicy{Allow: []string{"HOME"}}},
	}
}

// widened is what an operator approves: one more root, one more host, one more variable.
func widened(bind policy.Grant) policy.Grant {
	out := bind
	out.Files.Write = append(append([]string(nil), bind.Files.Write...), pathtest.Clean("srv/w/tmp"))
	out.Net = policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com", "cdn.example.com"}}
	out.Exec.Env = policy.EnvPolicy{Allow: []string{"HOME", "PATH"}}
	return out
}

// TestWidenKeepsEveryPermitTheSessionHad is the runtime invariant: an approval widens a boundary,
// it does not replace the authority the session already held. Each accepted case is one dimension,
// so a regression names the dimension it broke.
func TestWidenKeepsEveryPermitTheSessionHad(t *testing.T) {
	bind := normalized(t, confined())
	approved := widened(bind)

	for name, to := range map[string]policy.Grant{
		"one dimension": func() policy.Grant {
			one := bind
			one.Files.Write = approved.Files.Write
			return one
		}(),
		"the whole approval": approved,
	} {
		got, err := bind.Widen(to)
		if err != nil {
			t.Fatalf("%s: Widen: %v", name, err)
		}
		if !hasRoot(got.Files.Write, bind.Files.Write[0]) {
			t.Fatalf("%s: widened = %+v, want the session's own root kept", name, got.Files)
		}
		for _, host := range bind.Net.AllowHosts {
			if !contains(got.Net.AllowHosts, host) {
				t.Fatalf("%s: widened = %v, want %q kept", name, got.Net.AllowHosts, host)
			}
		}
		for _, name := range bind.Exec.Env.Allow {
			if !contains(got.Exec.Env.Allow, name) {
				t.Fatalf("%s: widened = %v, want %q kept", name, got.Exec.Env.Allow, name)
			}
		}
	}
}

// TestWidenRefusesWhatIsNotAWidening covers every way an answer can stop being one: a permit the
// session had that it would lose, and a dimension the widener may not touch at all.
func TestWidenRefusesWhatIsNotAWidening(t *testing.T) {
	bind := normalized(t, confined())
	tmp := pathtest.Clean("srv/w/tmp")

	cases := map[string]struct {
		to     func(policy.Grant) policy.Grant
		reason string
	}{
		"dropping a root the session already had": {
			to: func(g policy.Grant) policy.Grant {
				g.Files.Write = []string{tmp}
				return g
			},
			reason: "does not keep every root",
		},
		"dropping a destination the session already had": {
			to: func(g policy.Grant) policy.Grant {
				g.Net = policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"cdn.example.com"}}
				return g
			},
			reason: "does not keep every destination",
		},
		"dropping a variable the session already saw": {
			to: func(g policy.Grant) policy.Grant {
				g.Exec.Env = policy.EnvPolicy{Allow: []string{"PATH"}}
				return g
			},
			reason: "does not keep every variable",
		},
		"a dimension that is not widenable": {
			to: func(g policy.Grant) policy.Grant {
				g.Resources.MemoryBytes = 1 << 30
				return g
			},
			reason: "only files.write, net and exec.env",
		},
		"a different tool set": {
			to: func(g policy.Grant) policy.Grant {
				g.Tools.Allow = []string{"read_file"}
				return g
			},
			reason: "only files.write, net and exec.env",
		},
		"a different sandbox": {
			to: func(g policy.Grant) policy.Grant {
				g.Sandbox = "local"
				return g
			},
			reason: "only files.write, net and exec.env",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := bind.Widen(tc.to(bind))
			denial, ok := policy.AsDenial(err)
			if !ok {
				t.Fatalf("Widen = %v, want a denial", err)
			}
			if denial.Check != policy.CheckEscalation {
				t.Fatalf("check = %q, want %q", denial.Check, policy.CheckEscalation)
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

// TestWidenRefusesAnAnswerThatDoesNotValidate is the other class: a grant this package would refuse
// on its own is a caller bug, so it is reported as one rather than as an approval that was declined.
func TestWidenRefusesAnAnswerThatDoesNotValidate(t *testing.T) {
	bind := normalized(t, confined())
	_, err := bind.Widen(policy.Grant{Files: policy.Files{Read: []string{pathtest.Clean("srv")}}})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Widen = %v, want a validation error", err)
	}
}

// TestWidenSplitsTheThreeDimensions is what an approval prompt is shown: the dimensions that
// changed, with their values, and never a command.
func TestWidenSplitsTheThreeDimensions(t *testing.T) {
	bind := normalized(t, confined())
	changes := policy.Diff(bind, widened(bind))

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
	only.Files.Write = widened(bind).Files.Write
	if changes := policy.Diff(bind, only); len(changes) != 1 || changes[0].Dimension != "files.write" {
		t.Fatalf("changes = %+v, want only files.write", changes)
	}
	// And nothing changed is nothing to show.
	if changes := policy.Diff(bind, bind); len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
}

// normalized is the value the host works with: a validated grant.
func normalized(t *testing.T, grant policy.Grant) policy.Grant {
	t.Helper()
	out, err := grant.Normalized()
	if err != nil {
		t.Fatalf("Normalized: %v", err)
	}
	return out
}

// hasRoot reports whether root is one of the listed subtrees.
func hasRoot(roots []string, root string) bool {
	return contains(roots, root)
}

// contains reports whether value is in values.
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
