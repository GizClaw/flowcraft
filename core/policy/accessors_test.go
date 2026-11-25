package policy_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/internal/pathtest"
	"github.com/GizClaw/flowcraft/core/policy"
)

// TestAllowsWriteHasThreeStates pins the asymmetry the file boundary is built on: an empty write set
// means anywhere, a listed one means exactly its subtrees, and readonly is the only spelling of
// "none".
func TestAllowsWriteHasThreeStates(t *testing.T) {
	root := pathtest.Clean("srv/w")
	out := pathtest.Clean("srv/w/out")

	unrestricted := policy.Grant{}
	if !unrestricted.AllowsWrite(pathtest.Clean("etc/passwd")) {
		t.Fatal("an empty write set refused a path")
	}

	listed := policy.Grant{Files: policy.Files{Read: []string{root}, Write: []string{out}}}
	if !listed.AllowsWrite(out) || !listed.AllowsWrite(pathtest.Clean("srv/w/out/deep/file")) {
		t.Fatal("a listed write set refused one of its own subtrees")
	}
	if listed.AllowsWrite(root) {
		t.Fatal("a listed write set allowed a path outside it")
	}

	readonly := policy.Grant{Files: policy.Files{Read: []string{root}, ReadOnly: true}}
	if readonly.AllowsWrite(out) {
		t.Fatal("a read-only grant allowed a write")
	}
	if !readonly.AllowsRead(out) {
		t.Fatal("a read-only grant refused a read")
	}
}

// TestAllowsReadTreatsEmptyAsEverything is the other half: reading is the recoverable direction, so
// silence means "look around".
func TestAllowsReadTreatsEmptyAsEverything(t *testing.T) {
	if !(policy.Grant{}).AllowsRead(pathtest.Clean("anywhere")) {
		t.Fatal("an empty read set refused a path")
	}
	listed := policy.Grant{Files: policy.Files{Read: []string{pathtest.Clean("srv")}}}
	if !listed.AllowsRead(pathtest.Clean("srv/deep")) || listed.AllowsRead(pathtest.Clean("etc")) {
		t.Fatal("a listed read set did not mean exactly its subtrees")
	}
}

// TestDelegationAnswersItsThreeQuestions covers the predicates the host asks of a policy: the input
// cap with its default, which targets, which modes.
func TestDelegationAnswersItsThreeQuestions(t *testing.T) {
	var absent *policy.Delegation
	if absent.AllowsTarget("a") || absent.AllowsMode(policy.DelegationSync) {
		t.Fatal("a nil delegation allowed something")
	}
	if absent.InputLimit() != policy.DefaultMaxInputBytes {
		t.Fatalf("InputLimit = %d, want the default", absent.InputLimit())
	}

	present := &policy.Delegation{
		Targets:       []string{"reviewer"},
		Modes:         []policy.DelegationMode{policy.DelegationAsync},
		MaxInputBytes: 32,
	}
	if !present.AllowsTarget("reviewer") || present.AllowsTarget("other") {
		t.Fatal("AllowsTarget did not follow the target list")
	}
	if !present.AllowsMode(policy.DelegationAsync) || present.AllowsMode(policy.DelegationSync) {
		t.Fatal("AllowsMode did not follow the mode list")
	}
	if present.InputLimit() != 32 {
		t.Fatalf("InputLimit = %d, want the stated cap", present.InputLimit())
	}
}

// TestLimitsRejectImpossibleTables keeps the session table's decisions honest: a negative horizon or
// a negative cap is a document that cannot mean anything.
func TestLimitsRejectImpossibleTables(t *testing.T) {
	if err := (policy.Limits{}).Validate(); err != nil {
		t.Fatalf("the zero limits were refused: %v", err)
	}
	if !(policy.Limits{}).IsZero() {
		t.Fatal("the zero limits are not zero")
	}
	if err := (policy.Limits{MaxSessions: -1}).Validate(); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Validate = %v, want a validation error", err)
	}
}

// TestBudgetFillsTheBuiltInDefault: a zero field means "use the default", not "unlimited", and the
// exposure of an unlisted tool is the stable baseline.
func TestBudgetFillsTheBuiltInDefault(t *testing.T) {
	effective := (policy.Budget{}).Effective()
	if effective.MaxDefinitions != policy.DefaultBudget.MaxDefinitions ||
		effective.MaxBytes != policy.DefaultBudget.MaxBytes {
		t.Fatalf("Effective = %+v, want the built-in default", effective)
	}
	stated := policy.ToolSet{Budget: policy.Budget{MaxDefinitions: 3}}
	if got := stated.EffectiveBudget(); got.MaxDefinitions != 3 || got.MaxBytes == 0 {
		t.Fatalf("EffectiveBudget = %+v, want the stated count and the default bytes", got)
	}
	if got := stated.ExposureOf("read_file"); got != policy.ExposureAlways {
		t.Fatalf("ExposureOf = %q, want always", got)
	}
	if got := (policy.ToolSet{Exposure: map[string]policy.Exposure{"exec": policy.ExposureHidden}}).ExposureOf("exec"); got != policy.ExposureHidden {
		t.Fatalf("ExposureOf = %q, want the override", got)
	}
}

// TestEveryEnumRendersAsText is the document contract: the loader decodes through JSON semantics and
// a document is written in YAML, so both tags have to render the word rather than the number.
func TestEveryEnumRendersAsText(t *testing.T) {
	mode := policy.NetAllowList
	if text, err := mode.MarshalText(); err != nil || string(text) != "allow_list" {
		t.Fatalf("NetMode text = %q (%v)", text, err)
	}
	action := policy.NetAllow
	if text, err := action.MarshalText(); err != nil || string(text) != "allow" {
		t.Fatalf("NetAction text = %q (%v)", text, err)
	}
	if got := (policy.NetRule{Action: policy.NetDeny, Host: "a.example.com", Port: 443}).String(); got != "deny a.example.com:443" {
		t.Fatalf("NetRule = %q", got)
	}
	if got := policy.IsolatingNetModes(); len(got) != 3 {
		t.Fatalf("IsolatingNetModes = %v, want the three postures a backend can enforce", got)
	}
}

// TestDiffRendersAProxyByHost: a prompt is a place credentials must not reach, so a proxy is named by
// the host it dials and never by the URL that carries them.
func TestDiffRendersAProxyByHost(t *testing.T) {
	from := policy.Grant{Net: policy.NetPolicy{Mode: policy.NetProxy, Proxy: "http://user:secret@proxy.internal:8080"}}
	to := from
	to.Net.AllowHosts = []string{"api.example.com"}

	changes := policy.Diff(from, to)
	if len(changes) != 1 || changes[0].Dimension != "net" {
		t.Fatalf("changes = %+v, want the network dimension", changes)
	}
	rendered := strings.Join(append(append([]string(nil), changes[0].Before...), changes[0].After...), " ")
	if strings.Contains(rendered, "secret") || strings.Contains(rendered, "user") {
		t.Fatalf("the difference rendered the proxy's credentials: %q", rendered)
	}
	if !strings.Contains(rendered, "proxy.internal:8080") {
		t.Fatalf("the difference = %q, want the proxy host", rendered)
	}
}
