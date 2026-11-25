package policy_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/core/policy"
)

func ref(provider, name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: provider, Name: name}}
}

// ceiling is a plausible agent authority: two models, three tools, a definition
// budget, and limits that belong to the session table rather than the session.
func ceiling() policy.Grant {
	return policy.Grant{
		Sandbox: "locked",
		Files:   policy.Files{Read: []string{pathtest.Abs("srv/w")}, Write: []string{pathtest.Abs("srv/w/out")}},
		Models:  []model.ModelRef{ref("openai", "gpt-4o"), ref("openai", "gpt-4o-mini")},
		Tools: policy.ToolSet{
			Allow:    []string{"read_file", "write_file", "exec"},
			Exposure: map[string]policy.Exposure{"exec": policy.ExposureDeferred},
			Budget:   policy.Budget{MaxDefinitions: 10, MaxBytes: 4096},
		},
		Limits: policy.Limits{Idle: policy.Duration(10 * time.Minute), MaxSessions: 8},
	}
}

func TestNarrowOfNothingKeepsTheCeiling(t *testing.T) {
	base := ceiling()
	got, err := base.Narrow(policy.Request{})
	if err != nil {
		t.Fatalf("Narrow: %v", err)
	}
	if len(got.Models) != len(base.Models) {
		t.Fatalf("models = %v, want the ceiling's", got.Models)
	}
	if got.Tools.Allow == nil {
		t.Fatal("tool allow-list was widened to unrestricted")
	}
	if got.Files.Read[0] != pathtest.Clean("srv/w") {
		t.Fatalf("files = %+v, want the ceiling's", got.Files)
	}
	if !got.Limits.IsZero() {
		t.Fatalf("Limits = %+v, want cleared: a session does not limit itself", got.Limits)
	}
}

func TestNarrowRefusesWhatTheCeilingDoesNotGrant(t *testing.T) {
	base := ceiling()
	cases := map[string]struct {
		req   policy.Request
		check policy.Check
	}{
		"model outside the grant": {
			req:   policy.Request{Models: []model.ModelRef{ref("anthropic", "claude")}},
			check: policy.CheckModels,
		},
		"tool outside the allow-list": {
			req:   policy.Request{Tools: &policy.ToolSet{Allow: []string{"shell"}}},
			check: policy.CheckVisibility,
		},
		"budget above the ceiling": {
			req:   policy.Request{Budget: &policy.Budget{MaxDefinitions: 99}},
			check: policy.CheckBudget,
		},
		"exposure widened past the ceiling": {
			req: policy.Request{Tools: &policy.ToolSet{
				Exposure: map[string]policy.Exposure{"exec": policy.ExposureAlways},
			}},
			check: policy.CheckVisibility,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := base.Narrow(tc.req)
			denial, ok := policy.AsDenial(err)
			if !ok {
				t.Fatalf("Narrow returned %v, want a denial", err)
			}
			if denial.Check != tc.check {
				t.Fatalf("Check = %q, want %q", denial.Check, tc.check)
			}
			if denial.Reason == "" {
				t.Fatal("the denial has no reason to render to a model")
			}
		})
	}
}

func TestNarrowPreservesModelPriority(t *testing.T) {
	base := ceiling()
	requested := []model.ModelRef{ref("openai", "gpt-4o-mini"), ref("openai", "gpt-4o")}
	got, err := base.Narrow(policy.Request{Models: requested})
	if err != nil {
		t.Fatalf("Narrow: %v", err)
	}
	// Position is priority, so the requested order must survive; sorting would
	// silently reorder a failover chain.
	for i, want := range requested {
		if got.Models[i] != want {
			t.Fatalf("models = %v, want %v", got.Models, requested)
		}
	}
}

// TestNarrowIsIdempotent is the property that makes a narrowed grant a stable
// identity: narrowing to the same request twice must produce the same value, or
// session.grant.id would change every time a host re-applied its own request.
func TestNarrowIsIdempotent(t *testing.T) {
	base := ceiling()
	req := policy.Request{
		Models: []model.ModelRef{ref("openai", "gpt-4o-mini")},
		Tools: &policy.ToolSet{
			Allow:  []string{"read_file"},
			Budget: policy.Budget{MaxDefinitions: 5},
		},
	}
	once, err := base.Narrow(req)
	if err != nil {
		t.Fatalf("first Narrow: %v", err)
	}
	twice, err := once.Narrow(req)
	if err != nil {
		t.Fatalf("second Narrow: %v", err)
	}
	if once.Digest() != twice.Digest() {
		t.Fatalf("narrowing is not idempotent:\n%v\n%v", once, twice)
	}
}

// TestNarrowOnlyNarrows is the monotonicity property, asserted axis by axis
// rather than as one comparison: each field has its own notion of subset.
func TestNarrowOnlyNarrows(t *testing.T) {
	base := ceiling()
	requests := []policy.Request{
		{},
		{Models: []model.ModelRef{ref("openai", "gpt-4o")}},
		{Tools: &policy.ToolSet{Allow: []string{"read_file", "exec"}}},
		{Tools: &policy.ToolSet{Allow: []string{"read_file"}, Hide: []string{"exec"}}},
		{Tools: &policy.ToolSet{Budget: policy.Budget{MaxDefinitions: 3}}},
		{Tools: &policy.ToolSet{Allow: []string{}}}, // explicitly no tools
	}
	for i, req := range requests {
		got, err := base.Narrow(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		for _, m := range got.Models {
			if !base.AllowsModel(m) {
				t.Fatalf("request %d widened models: %v not in %v", i, m, base.Models)
			}
		}
		for name := range got.Tools.Exposure {
			if !base.Tools.Allows(name) {
				t.Fatalf("request %d exposed an ungranted tool %q", i, name)
			}
		}
		if got.Files.ReadOnly != base.Files.ReadOnly || len(got.Files.Read) != len(base.Files.Read) {
			t.Fatalf("request %d changed files, which a request cannot reach", i)
		}
		if got.Sandbox != base.Sandbox {
			t.Fatalf("request %d changed the sandbox", i)
		}
		if got.Delegation != nil {
			t.Fatalf("request %d invented delegation", i)
		}
		if got.Tools.EffectiveBudget().MaxDefinitions > base.Tools.EffectiveBudget().MaxDefinitions {
			t.Fatalf("request %d widened the definition budget", i)
		}
	}
}

func TestNarrowRejectsAnInvalidCeiling(t *testing.T) {
	base := ceiling()
	base.Files = policy.Files{Read: []string{pathtest.Abs("srv")}, Write: []string{pathtest.Abs("tmp")}}
	if _, err := base.Narrow(policy.Request{}); err == nil {
		t.Fatal("Narrow accepted a ceiling whose write set is outside its read set")
	}
}

// TestDigestIsTheSameForTheSamePermission pins the property the resume check and
// the span attributes depend on: one permission, one id.
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

func TestDigestIgnoresLimitsButNotPermissions(t *testing.T) {
	base := ceiling()

	withLimits := base
	withLimits.Limits = policy.Limits{Idle: policy.Duration(time.Second), MaxSessions: 1}
	if withLimits.Digest() != base.Digest() {
		t.Fatal("changing an idle timeout changed the permission identity")
	}

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
	if len(checks) != 7 {
		t.Fatalf("Checks() = %v, want the seven rules the package can refuse by", checks)
	}
	seen := make(map[policy.Check]bool, len(checks))
	for _, check := range checks {
		if check == "" || seen[check] {
			t.Fatalf("check %q is empty or duplicated", check)
		}
		seen[check] = true
	}
}

// FuzzNarrowOnlyNarrows is the batch-1 gate the plan asked for and the code did not have: a table
// test checks the cases someone thought of, and the property that matters — a narrowed grant is
// inside the one it came from, and narrowing twice is narrowing once — holds for every input.
func FuzzNarrowOnlyNarrows(f *testing.F) {
	f.Add([]byte(`{"sandbox":"local","files":{"read":["/srv"],"write":["/srv/out"]},"models":[{"id":{"provider":"p","name":"m"}}]}`), []byte(`{}`))
	f.Add([]byte(`{}`), []byte(`{"tools":{"allow":["a"]}}`))
	f.Fuzz(func(t *testing.T, grantJSON, requestJSON []byte) {
		var ceiling policy.Grant
		if err := json.Unmarshal(grantJSON, &ceiling); err != nil {
			return
		}
		normalized, err := ceiling.Normalized()
		if err != nil {
			return
		}
		var request policy.Request
		if err := json.Unmarshal(requestJSON, &request); err != nil {
			return
		}
		bind, err := normalized.Narrow(request)
		if err != nil {
			// Every refusal is a value a caller can inspect, never a bare string.
			var denial *policy.Denial
			if !errors.As(err, &denial) && !errdefs.IsValidation(err) && !errdefs.IsForbidden(err) {
				t.Fatalf("refusal %v is neither a Denial nor a classified error", err)
			}
			return
		}
		// Narrowing twice is narrowing once: a bind re-narrowed by the same request is itself.
		again, err := bind.Narrow(request)
		if err != nil {
			t.Fatalf("a bind cannot be narrowed by the request that produced it: %v", err)
		}
		if again.Digest() != bind.Digest() {
			t.Fatalf("narrow is not idempotent: %s vs %s", bind.Digest(), again.Digest())
		}
		// And the bind is inside the ceiling it came from: re-narrowing the ceiling by the bind's
		// own shape must not refuse what the bind already carries.
		if _, err := normalized.Narrow(requestOf(bind)); err != nil {
			t.Fatalf("the bind %s is not inside its ceiling: %v", bind.Digest(), err)
		}
	})
}

// requestOf spells a grant as the request that would produce it, which is how the fuzz asserts
// containment without a second comparison implementation.
func requestOf(g policy.Grant) policy.Request {
	tools := g.Tools
	return policy.Request{Models: g.Models, Tools: &tools, Budget: &g.Tools.Budget}
}
