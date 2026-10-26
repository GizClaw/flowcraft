package agenttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// JournalStore is an in-memory [agent.CheckpointStore] that also implements
// [agent.CheckpointJournal], with the bookkeeping a test needs to assert how a
// writer persisted something: how many bases were written, and what each delta
// carried.
//
// It is the test double for the store-side half of the incremental checkpoint
// design. Production stores implement the same contract; this one exists so an
// engine test can say "fifty waves cost two bases and forty-eight appends"
// instead of inferring it from allocation counts.
//
// Load replays in memory, so a replayed var keeps its Go type (an int stays an
// int, where the SQLite store would hand back a float64 after a JSON round
// trip). Assertions comparing replayed values should compare by value, not by
// type.
type JournalStore struct {
	mu      sync.Mutex
	bases   map[string]agent.Checkpoint
	deltas  map[string][]agent.CheckpointDelta
	basesN  int
	failOn  string // "base" or "append": the next call of that kind fails
	failErr error
}

// NewJournalStore returns an empty journal store.
func NewJournalStore() *JournalStore {
	return &JournalStore{
		bases:  make(map[string]agent.Checkpoint),
		deltas: make(map[string][]agent.CheckpointDelta),
	}
}

// Save implements [agent.CheckpointStore]; it is a base write, exactly as the
// journal contract requires.
func (s *JournalStore) Save(ctx context.Context, cp agent.Checkpoint) error {
	return s.WriteBase(ctx, cp)
}

// WriteBase implements [agent.CheckpointJournal].
func (s *JournalStore) WriteBase(_ context.Context, cp agent.Checkpoint) error {
	if err := cp.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOn == "base" {
		s.failOn = ""
		return s.failErrOr("base write failed")
	}
	s.bases[cp.ExecID] = cp.Clone()
	delete(s.deltas, cp.ExecID)
	s.basesN++
	return nil
}

// Append implements [agent.CheckpointJournal].
func (s *JournalStore) Append(_ context.Context, delta agent.CheckpointDelta) error {
	if err := delta.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOn == "append" {
		s.failOn = ""
		return s.failErrOr("append failed")
	}
	if _, ok := s.bases[delta.ExecID]; !ok {
		return fmt.Errorf("agenttest journal store: append %s: %w",
			delta.ExecID, agent.ErrNeedsBase)
	}
	s.deltas[delta.ExecID] = append(s.deltas[delta.ExecID], delta)
	return nil
}

// Load implements [agent.CheckpointStore]: the base plus every appended delta,
// replayed in order.
func (s *JournalStore) Load(_ context.Context, execID string) (*agent.Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, ok := s.bases[execID]
	if !ok {
		return nil, nil
	}
	cp := base.Clone()
	for _, delta := range s.deltas[execID] {
		delta.ApplyTo(&cp)
	}
	return &cp, nil
}

// Delete implements [agent.CheckpointDeleter].
func (s *JournalStore) Delete(_ context.Context, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bases, execID)
	delete(s.deltas, execID)
	return nil
}

// BaseCount returns how many base records were written.
func (s *JournalStore) BaseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.basesN
}

// Deltas returns the deltas appended for execID, in order.
func (s *JournalStore) Deltas(execID string) []agent.CheckpointDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.CheckpointDelta(nil), s.deltas[execID]...)
}

// FailNext makes the next call of kind ("base" or "append") fail, so a caller
// can assert that a persistence failure leaves the writer's position intact.
func (s *JournalStore) FailNext(kind string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failOn = kind
	s.failErr = err
}

func (s *JournalStore) failErrOr(fallback string) error {
	if s.failErr != nil {
		return s.failErr
	}
	return errdefs.Internalf("agenttest journal store: %s", fallback)
}

// StoreHost is a Host that persists checkpoints through a store and answers the
// capability question the way the runtime's host does. Use it to drive an
// engine against a [JournalStore].
type StoreHost struct {
	agent.NoopHost
	Store agent.CheckpointStore
}

// Checkpoint implements [agent.Checkpointer].
func (h StoreHost) Checkpoint(ctx context.Context, cp agent.Checkpoint) error {
	return h.Store.Save(ctx, cp)
}

// CheckpointCapabilities implements [agent.CheckpointTarget].
func (h StoreHost) CheckpointCapabilities() agent.CheckpointCapabilities {
	return agent.CheckpointCapabilitiesForStore(h.Store)
}

// CheckpointJournalSuite exercises the [agent.CheckpointJournal] contract: an
// append needs a base, a base discards the chain, a legacy Save is a base, the
// rebuilt record equals the base with every delta applied, and a store may
// refuse an append with [agent.ErrNeedsBase].
//
// newStore must return a fresh, ready-to-use store for every call, and that
// store must implement [agent.CheckpointJournal] (a store that does not is a
// caller bug, not a skip: run [CheckpointStoreSuite] for those).
func CheckpointJournalSuite(t *testing.T, newStore func() agent.CheckpointStore) {
	t.Helper()

	journalOf := func(t *testing.T, store agent.CheckpointStore) agent.CheckpointJournal {
		t.Helper()
		journal, ok := store.(agent.CheckpointJournal)
		if !ok {
			t.Fatalf("store %T does not implement agent.CheckpointJournal", store)
		}
		return journal
	}

	t.Run("AppendWithoutBase", func(t *testing.T) {
		t.Helper()
		store := newStore()
		err := journalOf(t, store).Append(context.Background(), JournalDelta("absent", 1))
		if !errors.Is(err, agent.ErrNeedsBase) {
			t.Fatalf("Append without base = %v, want ErrNeedsBase", err)
		}
	})

	t.Run("BaseThenAppendReplays", func(t *testing.T) {
		t.Helper()
		store := newStore()
		ctx := context.Background()
		base := JournalBase("run-1")
		if err := journalOf(t, store).WriteBase(ctx, base); err != nil {
			t.Fatalf("WriteBase: %v", err)
		}
		want := base.Clone()
		for i := 1; i <= 3; i++ {
			delta := JournalDelta("run-1", i)
			if err := journalOf(t, store).Append(ctx, delta); err != nil {
				t.Fatalf("Append %d: %v", i, err)
			}
			delta.ApplyTo(&want)
		}

		got, err := store.Load(ctx, "run-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got == nil {
			t.Fatal("Load after appends returned nil")
		}
		if !checkpointEqual(*got, want) {
			t.Fatalf("Load = %+v, want %+v", *got, want)
		}
		// The boundary metadata of the latest delta is the resume anchor.
		if iter := 3; got.Iteration != iter {
			t.Fatalf("Iteration = %d, want %d", got.Iteration, iter)
		}
		if msgs := got.Board.Channels[agent.MainChannel]; len(msgs) != 1+3 {
			t.Fatalf("main channel = %d messages, want base + 3 appends", len(msgs))
		}
		if v, ok := got.Board.Vars["step"]; !ok || v == nil {
			t.Fatalf("vars = %#v, want the last delta's write", got.Board.Vars)
		}
	})

	t.Run("BaseDiscardsChain", func(t *testing.T) {
		t.Helper()
		store := newStore()
		ctx := context.Background()
		journal := journalOf(t, store)
		if err := journal.WriteBase(ctx, JournalBase("run-1")); err != nil {
			t.Fatalf("WriteBase: %v", err)
		}
		if err := journal.Append(ctx, JournalDelta("run-1", 1)); err != nil {
			t.Fatalf("Append: %v", err)
		}
		replacement := JournalBase("run-1")
		replacement.Steps = []string{"wave-9"}
		replacement.Iteration = 9
		if err := journal.WriteBase(ctx, replacement); err != nil {
			t.Fatalf("WriteBase replacement: %v", err)
		}
		got, err := store.Load(ctx, "run-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !checkpointEqual(*got, replacement) {
			t.Fatalf("Load = %+v, want the replacement base (the chain must be gone)", *got)
		}
	})

	t.Run("SaveIsABase", func(t *testing.T) {
		t.Helper()
		store := newStore()
		ctx := context.Background()
		if err := journalOf(t, store).WriteBase(ctx, JournalBase("run-1")); err != nil {
			t.Fatalf("WriteBase: %v", err)
		}
		if err := journalOf(t, store).Append(ctx, JournalDelta("run-1", 1)); err != nil {
			t.Fatalf("Append: %v", err)
		}
		replacement := JournalBase("run-1")
		replacement.Iteration = 42
		if err := store.Save(ctx, replacement); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := store.Load(ctx, "run-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !checkpointEqual(*got, replacement) {
			t.Fatalf("Load = %+v, want the Save record with no chain applied", *got)
		}
	})

	t.Run("ValidateRejectsEmptyExecID", func(t *testing.T) {
		t.Helper()
		store := newStore()
		delta := JournalDelta("run-1", 1)
		delta.ExecID = ""
		if err := journalOf(t, store).Append(context.Background(), delta); err == nil {
			t.Fatal("Append with no exec id succeeded")
		}
	})

	t.Run("ConcurrentAppendsAllLand", func(t *testing.T) {
		t.Helper()
		store := newStore()
		ctx := context.Background()
		journal := journalOf(t, store)
		if err := journal.WriteBase(ctx, JournalBase("run-1")); err != nil {
			t.Fatalf("WriteBase: %v", err)
		}
		const parallel = 8
		var wg sync.WaitGroup
		for i := 0; i < parallel; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// A single-writer-per-execution store may reject a racing
				// append; the contract is that a retry with a fresh base
				// works, not that every racing append succeeds.
				_ = journal.Append(ctx, JournalDelta("run-1", 1))
			}()
		}
		wg.Wait()
		if _, err := store.Load(ctx, "run-1"); err != nil {
			t.Fatalf("Load after concurrent appends: %v", err)
		}
	})
}
