package policy_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

func ref(provider, name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: provider, Name: name}}
}

// permission is a plausible session authority: two models, three tools and a
// definition budget, one field per dimension the digest must cover.
func permission() policy.Grant {
	return policy.Grant{
		Sandbox: "locked",
		Files:   policy.Files{Read: []string{pathtest.Abs("srv/w")}, Write: []string{pathtest.Abs("srv/w/out")}},
		Models:  []model.ModelRef{ref("openai", "gpt-4o"), ref("openai", "gpt-4o-mini")},
		Tools: policy.ToolSet{
			Allow:    []string{"read_file", "write_file", "exec"},
			Exposure: map[string]policy.Exposure{"exec": policy.ExposureDeferred},
			Budget:   policy.Budget{MaxDefinitions: 10, MaxBytes: 4096},
		},
	}
}

func TestDigestIsTheSameForTheSamePermission(t *testing.T) {
	a := policy.Grant{Files: policy.Files{Read: []string{pathtest.Abs("srv"), pathtest.Abs("srv/work")}, ReadOnly: true}}
	b := policy.Grant{Files: policy.Files{Read: []string{pathtest.Abs("srv/work"), pathtest.Abs("srv"), pathtest.Abs("srv")}, ReadOnly: true}}
	if a.Digest() != b.Digest() {
		t.Fatalf("nested and duplicated roots produced different digests: %s vs %s", a.Digest(), b.Digest())
	}
	open := policy.Grant{Files: policy.Files{Read: []string{pathtest.Roots()}}}
	if open.Digest() != (policy.Grant{}).Digest() {
		t.Fatalf("a filesystem root and an omitted list differ: %s vs %s", open.Digest(), (policy.Grant{}).Digest())
	}
}

func TestDigestCoversEveryPermission(t *testing.T) {
	base := permission()

	cases := map[string]func(*policy.Grant){
		"files":     func(g *policy.Grant) { g.Files = policy.Files{ReadOnly: true} },
		"models":    func(g *policy.Grant) { g.Models = g.Models[:1] },
		"sandbox":   func(g *policy.Grant) { g.Sandbox = "local" },
		"tools":     func(g *policy.Grant) { g.Tools.Allow = g.Tools.Allow[:1] },
		"budget":    func(g *policy.Grant) { g.Tools.Budget = policy.Budget{MaxDefinitions: 1} },
		"net":       func(g *policy.Grant) { g.Net = policy.NetPolicy{Mode: policy.NetDenyAll} },
		"exec":      func(g *policy.Grant) { g.Exec.Timeout = policy.Duration(time.Second) },
		"resources": func(g *policy.Grant) { g.Resources.MemoryBytes = 1 },
		"delegation": func(g *policy.Grant) {
			g.Delegation = &policy.Delegation{Targets: []string{"other"}, Modes: []policy.DelegationMode{policy.DelegationSync}}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := base
			change(&changed)
			if changed.Digest() == base.Digest() {
				t.Fatalf("changing %s did not change the digest", name)
			}
		})
	}
}

// TestDenialChecksAreEnumerated keeps the enumeration honest: the recording
// layer switches on these values, so a check that is added without being listed
// is counted under no name at all, and one that is listed twice collides with
// another rule in every dashboard.
func TestDenialChecksAreEnumerated(t *testing.T) {
	checks := policy.Checks()
	if len(checks) != 6 {
		t.Fatalf("Checks() = %v, want the six rules the package can refuse by", checks)
	}
	seen := make(map[policy.Check]bool, len(checks))
	for _, check := range checks {
		if check == "" || seen[check] {
			t.Fatalf("check %q is empty or duplicated", check)
		}
		seen[check] = true
	}
}

// TestAllowsModelWithoutASetIsUnrestricted pins the default: which models exist
// is a property of the process, so a grant that names none constrains nothing.
func TestAllowsModelWithoutASetIsUnrestricted(t *testing.T) {
	var grant policy.Grant
	if !grant.AllowsModel(ref("openai", "anything")) {
		t.Fatal("an empty model set refused a model")
	}
}

// TestAllowsModelProfileIsAsymmetric covers the two spellings of "this model": a
// grant without a profile names it wherever it is served, and a grant with one
// pins the deployment.
func TestAllowsModelProfileIsAsymmetric(t *testing.T) {
	loose := policy.Grant{Models: []model.ModelRef{ref("openai", "gpt-4o")}}
	if !loose.AllowsModel(model.ModelRef{ID: ref("openai", "gpt-4o").ID, Profile: "team-a"}) {
		t.Fatal("an unpinned grant refused a pinned ref")
	}
	if loose.AllowsModel(ref("openai", "gpt-4o-mini")) {
		t.Fatal("a different model id was allowed")
	}

	pinned := policy.Grant{Models: []model.ModelRef{
		{ID: ref("openai", "gpt-4o").ID, Profile: "team-a"},
	}}
	if pinned.AllowsModel(ref("openai", "gpt-4o")) {
		t.Fatal("a pinned grant allowed an unpinned ref")
	}
	if !pinned.AllowsModel(model.ModelRef{ID: ref("openai", "gpt-4o").ID, Profile: "team-a"}) {
		t.Fatal("a pinned grant refused its own ref")
	}
}
