//go:build unix

package sandbox

import (
	"testing"
)

func TestParseProcStat(t *testing.T) {
	// pid 1234, comm "test proc", state S, ppid 1, pgrp 100,
	// utime 11 ticks, stime 22 ticks, rss 300 pages.
	line := "1234 (test proc) S 1 100 200 0 -1 4194560 100 0 0 0 11 22 0 0 20 0 1 0 100 200 300"
	got, ok := parseProcStat([]byte(line))
	if !ok {
		t.Fatal("parseProcStat rejected a valid stat line")
	}
	if got.pgrp != 100 {
		t.Errorf("pgrp = %d, want 100", got.pgrp)
	}
	if got.utimeTicks != 11 || got.stimeTicks != 22 {
		t.Errorf("ticks = (%d, %d), want (11, 22)", got.utimeTicks, got.stimeTicks)
	}
	if got.rssPages != 300 {
		t.Errorf("rssPages = %d, want 300", got.rssPages)
	}
	if got, _ := parseProcStat([]byte("1 (no fields")); got != (procStatFields{}) {
		t.Fatalf("malformed line parsed as %+v", got)
	}
}
