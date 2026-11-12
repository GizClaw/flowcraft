package agent

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
)

// tableShards is how many independent indexes the session table is split
// into. Runs on distinct sessions only touch their own shard, so the global
// lock that used to serialize every run is gone; the number is a power of two
// only by habit, the index is a modulo.
const tableShards = 32

// table is the session index: a fixed set of shards, each with its own
// lock, its own entries, its own run counter and its own task-id sequence.
//
// It is deliberately unexported and small: the agent owns the lifecycle, this
// owns only where a session lives and how many runs a shard has in flight.
type table struct {
	shards []*shard
	// sessions is how many entries the whole table holds.
	//
	// It exists so a cap check costs one atomic load instead of a scan: the scan
	// is the work a background sweep exists to keep off the message path, and the
	// cap has to be enforced there anyway.
	sessions atomic.Int64
}

// count reports how many sessions the table holds.
func (t *table) count() int64 { return t.sessions.Load() }

type shard struct {
	index   int
	mu      sync.Mutex
	entries map[string]*session
	// running is the number of runs in flight in this shard; idle is non-nil
	// while someone is waiting for them to end.
	running int
	idle    chan struct{}
	// seq mints task ids inside this shard; the shard index is part of the id,
	// so the sequences never have to agree.
	seq uint64
	// total is the table's counter. A shard keeps it up to date under its own
	// lock, which is free: insert and remove already hold it.
	total *atomic.Int64
}

func newTable(size int) *table {
	if size <= 0 {
		size = tableShards
	}
	t := &table{shards: make([]*shard, size)}
	for i := range t.shards {
		t.shards[i] = &shard{index: i, entries: make(map[string]*session), total: &t.sessions}
	}
	return t
}

// shard returns the shard a session belongs to.
func (t *table) shard(sessionID string) *shard {
	return t.shards[shardIndex(sessionID, len(t.shards))]
}

// shardIndex is FNV-1a over the session id. It allocates nothing, which
// matters: it runs on every acquire.
func shardIndex(sessionID string, shards int) int {
	hash := uint32(2166136261)
	for i := 0; i < len(sessionID); i++ {
		hash ^= uint32(sessionID[i])
		hash *= 16777619
	}
	return int(hash % uint32(shards))
}

// lookup, insert and remove require the shard lock: they are the index
// primitives, and a caller that needs several of them keeps the lock across all
// of them.
func (s *shard) lookup(sessionID string) (*session, bool) {
	entry, found := s.entries[sessionID]
	return entry, found
}

func (s *shard) insert(entry *session) {
	if _, replaced := s.entries[entry.id]; !replaced {
		s.total.Add(1)
	}
	s.entries[entry.id] = entry
}

func (s *shard) remove(sessionID string) *session {
	entry, found := s.entries[sessionID]
	if !found {
		return nil
	}
	delete(s.entries, sessionID)
	s.total.Add(-1)
	return entry
}

// begin and end count runs in flight. Both require the shard lock, which the
// caller already holds when it takes the slot or releases it.
func (s *shard) begin() { s.running++ }

func (s *shard) end() {
	s.running--
	if s.running == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

// wait blocks until the shard has no run in flight.
func (s *shard) wait(ctx context.Context) error {
	s.mu.Lock()
	if s.running == 0 {
		s.mu.Unlock()
		return nil
	}
	if s.idle == nil {
		s.idle = make(chan struct{})
	}
	wait := s.idle
	s.mu.Unlock()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// nextID mints a task id for this shard. The shard index is part of the id, so
// two shards never have to coordinate their sequences.
func (s *shard) nextID(prefix string) string {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	buf := make([]byte, 0, len(prefix)+16)
	buf = append(buf, prefix...)
	buf = append(buf, '-')
	buf = strconv.AppendUint(buf, uint64(s.index), 36)
	buf = append(buf, '.')
	buf = strconv.AppendUint(buf, seq, 36)
	return string(buf)
}
