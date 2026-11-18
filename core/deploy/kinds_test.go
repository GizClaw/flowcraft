package deploy

import (
	"testing"
)

// Kinds are a value: every With* copies the receiver, and every one of them works
// on the zero table whose maps are nil.
func TestKindsWithAreCopyOnWrite(t *testing.T) {
	cases := map[string]struct {
		add func(Kinds) Kinds
		has func(Kinds) bool
	}{
		"engines": {
			func(k Kinds) Kinds { return k.WithEngines(engineFor("extra", nil)) },
			func(k Kinds) bool { _, ok := k.Engines["extra"]; return ok },
		},
		"protocols": {
			func(k Kinds) Kinds { return k.WithProtocols(ProtocolKind{Name: "extra"}) },
			func(k Kinds) bool { _, ok := k.Protocols["extra"]; return ok },
		},
		"hooks": {
			func(k Kinds) Kinds { return k.WithHooks(HookKind{Name: "extra"}) },
			func(k Kinds) bool { _, ok := k.Hooks["extra"]; return ok },
		},
		"sources": {
			func(k Kinds) Kinds { return k.WithSources(fileSource()) },
			func(k Kinds) bool { _, ok := k.Sources["file"]; return ok },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base := Kinds{}
			extended := tc.add(base)
			if tc.has(base) {
				t.Fatal("With* mutated the receiver")
			}
			if !tc.has(extended) {
				t.Fatal("With* did not add the kind")
			}
		})
	}
}
