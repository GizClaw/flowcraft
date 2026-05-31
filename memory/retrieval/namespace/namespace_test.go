package namespace

import "testing"

func TestRegisterDuplicateFails(t *testing.T) {
	first, err := Register("duptest")
	if err != nil {
		t.Fatalf("Register first = %v", err)
	}
	if first.String() != "duptest" {
		t.Fatalf("Register first prefix = %q", first.String())
	}
	if _, err := Register("duptest"); err == nil {
		t.Fatal("Register duplicate succeeded, want error")
	}
}

func TestMustRegisterAliasReusesRegisteredPrefix(t *testing.T) {
	first, err := Register("aliastest")
	if err != nil {
		t.Fatalf("Register first = %v", err)
	}
	alias := MustRegisterAlias("aliastest")
	if first.String() != alias.String() {
		t.Fatalf("alias prefix = %q, want %q", alias.String(), first.String())
	}
}

func TestUserScopeV2RoundTripDelimiterSafe(t *testing.T) {
	p := &Prefix{name: "ltm"}
	ns := p.UserScope("default", "bob__u_alice")
	if ns != "ltm_default__u12_bob__u_alice" {
		t.Fatalf("UserScope = %q", ns)
	}
	rt, user, isUser, ok := p.DecodeScope(ns)
	if !ok || !isUser || rt != "default" || user != "bob__u_alice" {
		t.Fatalf("DecodeScope(%q) = rt=%q user=%q isUser=%v ok=%v", ns, rt, user, isUser, ok)
	}
}

func TestLegacyUserScopeV1StillDecodes(t *testing.T) {
	p := &Prefix{name: "ltm"}
	ns := p.LegacyUserScopeV1("default", "alice")
	rt, user, isUser, ok := p.DecodeScope(ns)
	if !ok || !isUser || rt != "default" || user != "alice" {
		t.Fatalf("DecodeScope(%q) = rt=%q user=%q isUser=%v ok=%v", ns, rt, user, isUser, ok)
	}
}

func TestDatasetScope(t *testing.T) {
	p := &Prefix{name: "kb"}
	if got, want := p.DatasetScope("ds-1", "chunks"), "kb_ds_1__chunks"; got != want {
		t.Fatalf("DatasetScope = %q, want %q", got, want)
	}
}

func TestSanitize(t *testing.T) {
	if got, want := Sanitize("a-b/c"), "a_b_c"; got != want {
		t.Fatalf("Sanitize = %q, want %q", got, want)
	}
	if got := Sanitize(""); got != "anon" {
		t.Fatalf("Sanitize empty = %q", got)
	}
}
