package eventlogtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

// NewMemoryLog creates an in-memory event log for tests.
func NewMemoryLog() *MemoryLog {
	return &MemoryLog{clock: SystemClock}
}

// MemoryLog is a deterministic in-process implementation of eventlog.Log used
// across the test suite. It mirrors SQLiteLog's contract:
//   - Append assigns a strictly monotonic seq under writeMu.
//   - Subscribe supports SinceLive / SinceBeginning / N>0 with replay+live cut-over.
//   - Read/ReadAll filter by partition.
//
// MemoryLog is not safe for production use; everything is held in slices.
type MemoryLog struct {
	mu          sync.Mutex
	seq         int64
	log         []eventlog.Envelope
	subscribers []*memSub
	hook        func(eventlog.Envelope)
	clock       Clock
}

var _ eventlog.Log = (*MemoryLog)(nil)

// WithClock substitutes the wall clock used to stamp envelopes.
func (m *MemoryLog) WithClock(c Clock) *MemoryLog {
	if c != nil {
		m.clock = c
	}
	return m
}

func (m *MemoryLog) Atomic(ctx context.Context, fn func(uow eventlog.UnitOfWork) error) ([]eventlog.Envelope, error) {
	m.mu.Lock()
	uow := &memUow{log: m, ctx: ctx}
	if err := fn(uow); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	subs := append([]*memSub(nil), m.subscribers...)
	envs := uow.appended
	m.mu.Unlock()

	if m.hook != nil {
		for _, env := range envs {
			m.hook(env)
		}
	}
	for _, s := range subs {
		s.deliver(envs)
	}
	return envs, nil
}

func (m *MemoryLog) Subscribe(ctx context.Context, opts eventlog.SubscribeOptions) (eventlog.Subscription, error) {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 256
	}
	sub := &memSub{
		log:        m,
		ch:         make(chan eventlog.Envelope, opts.BufferSize),
		closedCh:   make(chan struct{}),
		partitions: opts.Partitions,
		types:      opts.Types,
	}
	m.mu.Lock()
	m.subscribers = append(m.subscribers, sub)
	switch {
	case opts.Since == eventlog.SinceLive:
		atomic.StoreInt64(&sub.cursor, m.seq)
		m.mu.Unlock()
	case opts.Since == eventlog.SinceBeginning, opts.Since > 0:
		startFrom := int64(0)
		if opts.Since > 0 {
			startFrom = int64(opts.Since)
		}
		atomic.StoreInt64(&sub.cursor, startFrom)
		history := append([]eventlog.Envelope(nil), m.log...)
		m.mu.Unlock()
		go sub.replay(ctx, history, startFrom)
	default:
		atomic.StoreInt64(&sub.cursor, m.seq)
		m.mu.Unlock()
	}
	return sub, nil
}

func (m *MemoryLog) Read(ctx context.Context, partition string, since eventlog.Since, limit int) (eventlog.ReadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	envs := []eventlog.Envelope{}
	last := int64(since)
	for _, env := range m.log {
		if partition != "" && env.Partition != partition {
			continue
		}
		if env.Seq <= int64(since) {
			continue
		}
		envs = append(envs, env)
		last = env.Seq
		if limit > 0 && len(envs) >= limit {
			break
		}
	}
	hasMore := limit > 0 && len(envs) == limit
	return eventlog.ReadResult{Events: envs, NextSince: eventlog.Since(last), HasMore: hasMore}, nil
}

func (m *MemoryLog) ReadAll(ctx context.Context, since eventlog.Since, limit int) (eventlog.ReadResult, error) {
	return m.Read(ctx, "", since, limit)
}

func (m *MemoryLog) LatestSeq(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq, nil
}

func (m *MemoryLog) Checkpoints() eventlog.CheckpointStore { return newMemCheckpoints() }

// Append is a test helper that appends drafts to the log.
func (m *MemoryLog) Append(tb testing.TB, drafts ...eventlog.EnvelopeDraft) []eventlog.Envelope {
	tb.Helper()
	envs, err := m.Atomic(context.Background(), func(uow eventlog.UnitOfWork) error {
		return uow.Append(context.Background(), drafts...)
	})
	if err != nil {
		tb.Fatalf("MemoryLog.Append: %v", err)
	}
	return envs
}

// WithHook sets a callback invoked once per appended envelope after commit.
func (m *MemoryLog) WithHook(hook func(eventlog.Envelope)) *MemoryLog {
	m.hook = hook
	return m
}

// removeSubscriber deregisters s; called from memSub.Close.
func (m *MemoryLog) removeSubscriber(s *memSub) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, x := range m.subscribers {
		if x == s {
			m.subscribers = append(m.subscribers[:i], m.subscribers[i+1:]...)
			return
		}
	}
}

// ---- memUow ----

type memUow struct {
	log      *MemoryLog
	ctx      context.Context
	appended []eventlog.Envelope
	lastSeq  int64
}

func (u *memUow) Append(ctx context.Context, drafts ...eventlog.EnvelopeDraft) error {
	for _, d := range drafts {
		if d.Partition == "" || d.Type == "" || d.Category == "" || d.Version == 0 {
			return errors.New("eventlogtest: incomplete draft")
		}
		var payload json.RawMessage
		if d.Payload != nil {
			b, err := json.Marshal(d.Payload)
			if err != nil {
				return err
			}
			payload = b
		} else {
			payload = json.RawMessage("null")
		}
		u.log.seq++
		u.lastSeq = u.log.seq
		env := eventlog.Envelope{
			Seq:       u.log.seq,
			Partition: d.Partition,
			Type:      d.Type,
			Version:   d.Version,
			Category:  d.Category,
			Ts:        u.log.clock.Now(),
			Payload:   payload,
			TraceID:   d.TraceID,
			SpanID:    d.SpanID,
		}
		if d.Actor != nil {
			a := *d.Actor
			env.Actor = &a
		}
		u.log.log = append(u.log.log, env)
		u.appended = append(u.appended, env)
	}
	return nil
}

func (u *memUow) CheckpointSet(_ context.Context, _ string, _ int64) error { return nil }
func (u *memUow) Sequence() int64                                          { return u.lastSeq }

func (u *memUow) BusinessExec(context.Context, string, ...any) (sql.Result, error) {
	return nil, nil
}
func (u *memUow) BusinessQuery(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}
func (u *memUow) BusinessQueryRow(context.Context, string, ...any) *sql.Row {
	return nil
}

// ---- memSub ----

type memSub struct {
	log        *MemoryLog
	ch         chan eventlog.Envelope
	closedCh   chan struct{}
	closeOnce  sync.Once
	partitions []string
	types      []string

	cursor    int64
	dropped   atomic.Int64
	replaying atomic.Bool
	pendingMu sync.Mutex
	pending   []eventlog.Envelope
}

func (s *memSub) C() <-chan eventlog.Envelope { return s.ch }
func (s *memSub) Cursor() int64               { return atomic.LoadInt64(&s.cursor) }
func (s *memSub) Lag() int64 {
	max, _ := s.log.LatestSeq(context.Background())
	cur := atomic.LoadInt64(&s.cursor)
	if cur >= max {
		return 0
	}
	return max - cur
}

func (s *memSub) Close() error {
	s.closeOnce.Do(func() {
		s.log.removeSubscriber(s)
		close(s.closedCh)
		close(s.ch)
	})
	return nil
}

func (s *memSub) deliver(envs []eventlog.Envelope) {
	if s.replaying.Load() {
		s.pendingMu.Lock()
		s.pending = append(s.pending, envs...)
		s.pendingMu.Unlock()
		return
	}
	for _, env := range envs {
		s.send(env)
	}
}

func (s *memSub) send(env eventlog.Envelope) {
	if !s.matches(env) {
		atomic.StoreInt64(&s.cursor, env.Seq)
		return
	}
	select {
	case s.ch <- env:
		atomic.StoreInt64(&s.cursor, env.Seq)
	case <-s.closedCh:
	default:
		s.dropped.Add(1)
	}
}

func (s *memSub) matches(env eventlog.Envelope) bool {
	if len(s.partitions) > 0 && !contains(s.partitions, env.Partition) {
		return false
	}
	if len(s.types) > 0 && !contains(s.types, env.Type) {
		return false
	}
	return true
}

func contains(xs []string, t string) bool {
	for _, x := range xs {
		if x == t {
			return true
		}
	}
	return false
}

func (s *memSub) replay(ctx context.Context, history []eventlog.Envelope, startFrom int64) {
	s.replaying.Store(true)
	defer s.replaying.Store(false)
	for _, env := range history {
		if env.Seq <= startFrom {
			continue
		}
		select {
		case <-s.closedCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		s.send(env)
	}
	for {
		s.pendingMu.Lock()
		buf := s.pending
		s.pending = nil
		if len(buf) == 0 {
			s.pendingMu.Unlock()
			break
		}
		s.pendingMu.Unlock()
		cur := atomic.LoadInt64(&s.cursor)
		for _, env := range buf {
			if env.Seq <= cur {
				continue
			}
			s.send(env)
		}
	}
}

// ---- memCheckpoints ----

func newMemCheckpoints() *memCheckpoints {
	return &memCheckpoints{m: map[string]int64{}}
}

type memCheckpoints struct {
	mu sync.Mutex
	m  map[string]int64
}

func (c *memCheckpoints) Get(_ context.Context, name string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[name], nil
}

func (c *memCheckpoints) List(_ context.Context) (map[string]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int64{}
	for k, v := range c.m {
		out[k] = v
	}
	return out, nil
}

func (c *memCheckpoints) Min(_ context.Context) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) == 0 {
		return 0, false, nil
	}
	min := int64(0)
	first := true
	for _, v := range c.m {
		if first || v < min {
			min = v
			first = false
		}
	}
	return min, true, nil
}

// Set is a test helper to seed checkpoints (not part of CheckpointStore).
func (c *memCheckpoints) Set(name string, seq int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[name] = seq
}
