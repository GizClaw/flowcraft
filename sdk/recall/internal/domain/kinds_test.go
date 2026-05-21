package domain

import "testing"

func TestFactKindProcedureIsValid(t *testing.T) {
	if !KindProcedure.IsValid() {
		t.Fatal("KindProcedure must be part of the canonical FactKind enum")
	}
}
