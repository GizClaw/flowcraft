package scenarios

import "testing"

func TestScoreEMF1(t *testing.T) {
	em, f1 := scoreEMF1("She lives in San Francisco now.", []string{"San Francisco"})
	if em != 1 {
		t.Fatalf("em = %v", em)
	}
	if f1 <= 0 {
		t.Fatalf("f1 = %v", f1)
	}
	em, f1 = scoreEMF1("unknown", []string{"San Francisco"})
	if em != 0 || f1 != 0 {
		t.Fatalf("miss: em=%v f1=%v", em, f1)
	}
}
