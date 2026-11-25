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

// TestWidenComparesNetworkPostures covers the network half of containment: a widening may admit a
// destination, an inspection posture or a proxy the session did not have, and may never lose one it
// did. Every comparison here is a decision [Grant.Widen] makes on the answer an operator returned.
func TestWidenComparesNetworkPostures(t *testing.T) {
	base := func() policy.Grant {
		g := confined()
		g.Net = policy.NetPolicy{
			Mode:        policy.NetAllowList,
			AllowHosts:  []string{"api.example.com"},
			UnixSockets: []string{"/run/agent.sock"},
			MITM:        &policy.MITMPolicy{Enabled: true, Hosts: []string{"api.example.com"}},
		}
		return g
	}
	from := normalized(t, base())

	cases := map[string]struct {
		net    policy.NetPolicy
		accept bool
	}{
		"another allowed host": {
			net:    policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com", "cdn.example.com"}, UnixSockets: from.Net.UnixSockets, MITM: from.Net.MITM},
			accept: true,
		},
		"a subdomain of an allowed host": {
			net:    policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"api.example.com", "assets.cdn.example.com"}, UnixSockets: from.Net.UnixSockets, MITM: from.Net.MITM},
			accept: true,
		},
		"stopping inspection": {
			net:    policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: from.Net.AllowHosts, UnixSockets: from.Net.UnixSockets},
			accept: true,
		},
		"another socket": {
			net:    policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: from.Net.AllowHosts, UnixSockets: []string{"/run/agent.sock", "/run/proxy.sock"}, MITM: from.Net.MITM},
			accept: true,
		},
		"a narrower allow-list": {
			net:    policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: []string{"cdn.example.com"}, UnixSockets: from.Net.UnixSockets, MITM: from.Net.MITM},
			accept: false,
		},
		"a tighter mode": {
			net:    policy.NetPolicy{Mode: policy.NetDenyAll, UnixSockets: from.Net.UnixSockets, MITM: from.Net.MITM},
			accept: false,
		},
		"a different rule list": {
			net: policy.NetPolicy{
				Mode:        policy.NetAllowList,
				AllowHosts:  from.Net.AllowHosts,
				Rules:       []policy.NetRule{{Action: policy.NetAllow, Host: "cdn.example.com"}},
				UnixSockets: from.Net.UnixSockets,
				MITM:        from.Net.MITM,
			},
			accept: false,
		},
		"a dropped socket": {
			net:    policy.NetPolicy{Mode: policy.NetAllowList, AllowHosts: from.Net.AllowHosts, MITM: from.Net.MITM},
			accept: false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			to := from
			to.Net = tc.net
			_, err := from.Widen(to)
			if tc.accept && err != nil {
				t.Fatalf("Widen = %v, want the posture accepted", err)
			}
			if !tc.accept && err == nil {
				t.Fatal("Widen accepted a posture that drops a destination")
			}
		})
	}
}

// TestWidenComparesEnvironments is the third dimension: a nil allow-list means "inherit the host's",
// which is the widest statement there is, so a widening may go from a list to nil but never the
// other way.
func TestWidenComparesEnvironments(t *testing.T) {
	from := normalized(t, confined()) // HOME only

	cases := map[string]struct {
		env    policy.EnvPolicy
		accept bool
	}{
		"one more variable":  {env: policy.EnvPolicy{Allow: []string{"HOME", "PATH"}}, accept: true},
		"inherit everything": {env: policy.EnvPolicy{}, accept: true},
		"a shorter list":     {env: policy.EnvPolicy{Allow: []string{"PATH"}}, accept: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			to := from
			to.Exec.Env = tc.env
			_, err := from.Widen(to)
			if tc.accept && err != nil {
				t.Fatalf("Widen = %v, want the environment accepted", err)
			}
			if !tc.accept && err == nil {
				t.Fatal("Widen accepted an environment that hides a variable")
			}
		})
	}

	// From nil, nothing can be a widening except nil: a list is narrower than the host's whole
	// environment.
	inheriting := normalized(t, policy.Grant{})
	shorter := inheriting
	shorter.Exec.Env = policy.EnvPolicy{Allow: []string{"HOME"}}
	if _, err := inheriting.Widen(shorter); err == nil {
		t.Fatal("Widen accepted a list where the session inherited the host environment")
	}
}
