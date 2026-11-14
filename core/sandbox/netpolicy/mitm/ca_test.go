package mitm

import (
	"fmt"
	"strings"
	"testing"
)

func TestCALeafCacheBounded(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	for i := 0; i < maxCachedLeaves+64; i++ {
		host := "h" + strings.Repeat("x", i%8) + "-" + fmt.Sprint(i) + ".example.test"
		if _, err := ca.Leaf(host); err != nil {
			t.Fatalf("Leaf(%q): %v", host, err)
		}
	}
	if got := len(ca.leaves); got != maxCachedLeaves {
		t.Fatalf("leaf cache size = %d, want %d", got, maxCachedLeaves)
	}
}
