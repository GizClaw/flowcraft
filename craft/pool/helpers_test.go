package pool

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// fakeMember is one pooled value with real lifecycle state and no work
// behind it: units of work and teardown are what a test drives, and the
// pool's own bookkeeping is what the tests assert on.
type fakeMember struct {
	key Key

	mu     sync.Mutex
	units  int
	closed bool
	done   chan struct{}
}

func newFakeMember(k Key) *fakeMember {
	return &fakeMember{key: k, done: make(chan struct{})}
}

// work adds one unit of work in flight.
func (m *fakeMember) work() {
	m.mu.Lock()
	m.units++
	m.mu.Unlock()
}

// busy reports whether the member still has work in flight.
func (m *fakeMember) busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.units > 0
}

// beginClose starts teardown: immediately when gate is nil, once the
// gate is released otherwise. Closing twice is a no-op, the way a real
// member's teardown is.
func (m *fakeMember) beginClose(gate chan struct{}) {
	finish := func() {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		m.closed = true
		m.mu.Unlock()
		close(m.done)
	}
	if gate == nil {
		finish()
		return
	}
	go func() {
		<-gate
		finish()
	}()
}

// waitClosed blocks until teardown finished, within ctx.
func (m *fakeMember) waitClosed(ctx context.Context) error {
	m.mu.Lock()
	done, closed := m.done, m.closed
	m.mu.Unlock()
	if closed {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *fakeMember) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// errGuard is the fake application's transient lifecycle guard, the
// error its Retryable classifier recognizes.
var errGuard = errors.New("member is closing")

// harness wires a pool to a Spec whose hooks are the test's, so a test
// can gate assemblies, gate teardowns and record what the pool did.
type harness struct {
	t    *testing.T
	pool *Pool[fakeMember]

	mu        sync.Mutex
	builds    int
	opened    []*fakeMember
	installed []Key
	replaced  []Key
	wanted    func(Key) bool
	openGate  chan struct{}
	closeGate chan struct{}
	openErr   error
	// openIgnoresCtx models an assembly that finishes after the caller's
	// attempt deadline: it waits for the gate and never looks at ctx.
	openIgnoresCtx bool

	closedCh chan *fakeMember
}

// newHarness builds a pool over the fake member. tune may adjust the
// spec before the pool is created (the retry window, mostly).
func newHarness(t *testing.T, tune ...func(*Spec[fakeMember])) *harness {
	t.Helper()
	h := &harness{t: t, closedCh: make(chan *fakeMember, 64)}
	spec := Spec[fakeMember]{
		Open: func(ctx context.Context, k Key) (*fakeMember, error) {
			h.mu.Lock()
			h.builds++
			gate, openErr := h.openGate, h.openErr
			ignoreCtx := h.openIgnoresCtx
			h.mu.Unlock()
			switch {
			case gate == nil:
			case ignoreCtx:
				<-gate
			default:
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if openErr != nil {
				return nil, openErr
			}
			m := newFakeMember(k)
			h.mu.Lock()
			h.opened = append(h.opened, m)
			h.mu.Unlock()
			return m, nil
		},
		Close: func(m *fakeMember) {
			h.mu.Lock()
			gate := h.closeGate
			h.mu.Unlock()
			h.closedCh <- m
			m.beginClose(gate)
		},
		Busy: func(m *fakeMember) bool { return m.busy() },
		WaitClosed: func(ctx context.Context, m *fakeMember) error {
			return m.waitClosed(ctx)
		},
		Installed: func(k Key, _ *fakeMember) {
			h.mu.Lock()
			h.installed = append(h.installed, k)
			h.mu.Unlock()
		},
		Replaced: func(k Key, _ *fakeMember) {
			h.mu.Lock()
			h.replaced = append(h.replaced, k)
			h.mu.Unlock()
		},
		Wanted: func(k Key) bool {
			h.mu.Lock()
			fn := h.wanted
			h.mu.Unlock()
			return fn == nil || fn(k)
		},
		Retryable: func(err error) bool { return errors.Is(err, errGuard) },
	}
	for _, fn := range tune {
		fn(&spec)
	}
	p, err := New(spec)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	h.pool = p
	return h
}

// pooled installs a member for k the way a finished assembly would, so
// a test can start from a populated pool without running Open. units is
// how much work the member has in flight.
func (h *harness) pooled(k Key, units int) *fakeMember {
	m := newFakeMember(k)
	m.units = units
	h.pool.mu.Lock()
	h.pool.pooled[k] = &entry[fakeMember]{value: m, refs: 1}
	h.pool.mu.Unlock()
	return m
}

// finish ends one unit of work on the member and settles it when that
// was its last, the way a member announces that it became idle.
func (h *harness) finish(m *fakeMember) {
	m.mu.Lock()
	m.units--
	idle := m.units == 0
	m.mu.Unlock()
	if idle {
		h.pool.Settle(m.key, m)
	}
}

func (h *harness) mustAcquire(k Key) *fakeMember {
	h.t.Helper()
	m, err := h.pool.Acquire(context.Background(), k)
	if err != nil {
		h.t.Fatalf("acquire %s: %v", k, err)
	}
	return m
}

func (h *harness) mustEnsure(k Key) *fakeMember {
	h.t.Helper()
	return h.mustEnsureWith(context.Background(), k)
}

func (h *harness) mustEnsureWith(ctx context.Context, k Key) *fakeMember {
	h.t.Helper()
	m, err := h.pool.Ensure(ctx, k)
	if err != nil {
		h.t.Fatalf("ensure %s: %v", k, err)
	}
	return m
}

func (h *harness) buildCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.builds
}

func (h *harness) pendingAssemblies() int {
	h.pool.mu.Lock()
	defer h.pool.mu.Unlock()
	return len(h.pool.assembling)
}

func (h *harness) openErrSet(err error) {
	h.mu.Lock()
	h.openErr = err
	h.mu.Unlock()
}

// waitClosed waits for one member to be reported to Close and fails
// when it is not.
func (h *harness) waitClosed(m *fakeMember) {
	h.t.Helper()
	select {
	case got := <-h.closedCh:
		if got != m {
			h.t.Fatalf("closed member = %p, want %p", got, m)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("member %p was never closed", m)
	}
}

// assertNothingClosed fails when any member was closed.
func (h *harness) assertNothingClosed() {
	h.t.Helper()
	select {
	case got := <-h.closedCh:
		h.t.Fatalf("unexpected close of %p", got)
	default:
	}
}

// eventually polls cond until it holds, so the tests can wait for the
// pool's own goroutines (a close watcher, a replacement watcher)
// without sleeping a fixed amount.
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) retired(k Key) bool {
	h.pool.mu.Lock()
	defer h.pool.mu.Unlock()
	return h.pool.pooled[k] == nil && h.pool.retiring[k] == nil
}

// settleRetirement waits until the pool has accounted for a member's
// teardown: Close was called, teardown finished, and the retiring entry
// is gone. The pool drops entries from a watcher, so a test that wants
// to read Current afterwards waits for it here rather than assuming a
// scheduling hop already happened.
func (h *harness) settleRetirement(k Key, m *fakeMember) {
	h.t.Helper()
	h.waitClosed(m)
	h.eventually("the retired member to be accounted for", func() bool {
		return h.retired(k)
	})
}

// waitRetired waits until a member is accounted for after a retirement
// the test did not drive through waitClosed (a pool-wide invalidation,
// mostly): teardown finished, and the pool dropped it.
func (h *harness) waitRetired(m *fakeMember) {
	h.t.Helper()
	h.eventually("the member to finish tearing down", m.isClosed)
	h.eventually(m.key.String()+" to leave the pool", func() bool {
		return h.retired(m.key)
	})
}

// pooledEntry returns the member the pool holds for k, or nil.
func (h *harness) pooledEntry(k Key) *fakeMember {
	h.pool.mu.Lock()
	defer h.pool.mu.Unlock()
	if e := h.pool.pooled[k]; e != nil {
		return e.value
	}
	return nil
}

// assertPooled pins "these members are still the pool's answer for
// their keys": each is still held and current, none was marked stale,
// and none was sent to teardown.
func (h *harness) assertPooled(members ...*fakeMember) {
	h.t.Helper()
	for _, m := range members {
		if got := h.pooledEntry(m.key); got != m {
			h.t.Fatalf("%s is no longer the pooled member", m.key)
		}
		if h.pool.Stale(m.key) {
			h.t.Fatalf("%s was marked stale", m.key)
		}
		if got := h.pool.Current(m.key); got != m {
			h.t.Fatalf("Current(%s) = %p, want %p", m.key, got, m)
		}
	}
}

// drainClosed collects the members reported to Close so far.
func (h *harness) drainClosed() []*fakeMember {
	var out []*fakeMember
	for {
		select {
		case m := <-h.closedCh:
			out = append(out, m)
		default:
			return out
		}
	}
}

// assertClosedExactly pins which members were closed, as a set: in
// which order the pool walked a kind is its business.
func (h *harness) assertClosedExactly(members ...*fakeMember) {
	h.t.Helper()
	want := make(map[*fakeMember]bool, len(members))
	for _, m := range members {
		want[m] = true
	}
	got := h.drainClosed()
	if len(got) != len(want) {
		h.t.Fatalf("closed %d members, want %d", len(got), len(want))
	}
	for _, m := range got {
		if !want[m] {
			h.t.Fatalf("unexpected close of %s", m.key)
		}
	}
}

// logSink records the telemetry the pool emits, so a test can pin the
// attribution of a log line (which key, which reason, deferred or not)
// rather than only that something was logged.
type logSink struct {
	mu      sync.Mutex
	records []logRecord
}

type logRecord struct {
	body  string
	attrs map[string]string
}

func (s *logSink) Enabled(context.Context, sdklog.EnabledParameters) bool {
	return true
}

func (s *logSink) OnEmit(_ context.Context, record *sdklog.Record) error {
	clone := record.Clone()
	attrs := make(map[string]string, clone.AttributesLen())
	clone.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = valueString(kv.Value)
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, logRecord{
		body:  clone.Body().AsString(),
		attrs: attrs,
	})
	s.mu.Unlock()
	return nil
}

func (s *logSink) Shutdown(context.Context) error   { return nil }
func (s *logSink) ForceFlush(context.Context) error { return nil }

// find returns every recorded line with the given body.
func (s *logSink) find(body string) []logRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []logRecord
	for _, r := range s.records {
		if r.body == body {
			out = append(out, r)
		}
	}
	return out
}

// installLogSink points the process-wide logger at a recording sink.
func installLogSink(t *testing.T) *logSink {
	t.Helper()
	sink := &logSink{}
	shutdown, err := telemetry.InitLog(context.Background(),
		telemetry.WithLogProcessor(sink))
	if err != nil {
		t.Fatalf("init log: %v", err)
	}
	telemetry.Enable()
	t.Cleanup(func() {
		_ = shutdown(context.Background())
	})
	return sink
}

// valueString renders one attribute value the way the log line's
// reader sees it.
func valueString(v attribute.Value) string {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString()
	case attribute.BOOL:
		return strconv.FormatBool(v.AsBool())
	case attribute.INT64:
		return strconv.FormatInt(v.AsInt64(), 10)
	default:
		return v.String()
	}
}
