package deploy

import (
	"testing"
)

// Kinds are a value: extending one must not mutate what BuiltinKinds handed out.
func TestKindsWithEnginesDoesNotMutate(t *testing.T) {
	base := BuiltinKinds()
	extended := base.WithEngines(engineFor("extra", nil))
	if _, ok := base.Engines["extra"]; ok {
		t.Fatal("WithEngines mutated the receiver")
	}
	if _, ok := extended.Engines["extra"]; !ok {
		t.Fatal("WithEngines did not add the kind")
	}
}
