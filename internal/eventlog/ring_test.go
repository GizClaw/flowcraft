package eventlog

import (
	"testing"
)

func mkEnv(seq int64) Envelope {
	return Envelope{Seq: seq, Partition: "runtime:r1", Type: "task.submitted", Version: 1, Category: "business"}
}

func TestRing_AppendAndReadFrom_NoOverflow(t *testing.T) {
	r := newRing(8)
	for i := int64(1); i <= 5; i++ {
		r.append(mkEnv(i))
	}
	envs, ok := r.readFrom(0)
	if !ok || len(envs) != 5 {
		t.Fatalf("want 5 envs, got %d ok=%v", len(envs), ok)
	}
	envs, ok = r.readFrom(3)
	if !ok || len(envs) != 2 || envs[0].Seq != 4 {
		t.Fatalf("want envs starting 4, got %d ok=%v", len(envs), ok)
	}
}

func TestRing_ReadFromEmpty(t *testing.T) {
	r := newRing(4)
	envs, ok := r.readFrom(0)
	if !ok || len(envs) != 0 {
		t.Fatalf("expected empty authoritative, got %d ok=%v", len(envs), ok)
	}
}

func TestRing_Wraparound_LosesOldest(t *testing.T) {
	r := newRing(4)
	for i := int64(1); i <= 6; i++ {
		r.append(mkEnv(i))
	}
	// minSeq should now be 3 (we kept seqs 3..6)
	min, max := r.snapshot()
	if min != 3 || max != 6 {
		t.Fatalf("want window [3,6], got [%d,%d]", min, max)
	}

	// requesting from sinceSeq=1 is non-authoritative (we'd start at 2 but
	// the ring only has seqs >= 3).
	envs, ok := r.readFrom(1)
	if ok {
		t.Fatalf("expected non-authoritative for seq before window, got %d envs", len(envs))
	}

	envs, ok = r.readFrom(3)
	if !ok || len(envs) != 3 {
		t.Fatalf("want 3 envs (4,5,6), got %d ok=%v", len(envs), ok)
	}
	if envs[0].Seq != 4 || envs[2].Seq != 6 {
		t.Fatalf("unexpected seqs: %v", envs)
	}
}

func TestRing_ReadFromAfterMax(t *testing.T) {
	r := newRing(4)
	r.append(mkEnv(10))
	envs, ok := r.readFrom(10)
	if !ok || len(envs) != 0 {
		t.Fatalf("want empty authoritative for cursor==max, got %d ok=%v", len(envs), ok)
	}
}
