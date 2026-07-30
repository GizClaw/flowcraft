package namespace

import "testing"

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

// TestLegacyV1NamespaceStillDecodes pins DecodeScope's read-side
// compatibility with pre-V2 recall namespaces ("<prefix>_<rt>__u_<user>").
// The V1 constructor is gone; decoders must still parse namespaces
// written by old binaries so migration tooling can read them.
func TestLegacyV1NamespaceStillDecodes(t *testing.T) {
	p := &Prefix{name: "ltm"}
	ns := "ltm_default__u_alice"
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
