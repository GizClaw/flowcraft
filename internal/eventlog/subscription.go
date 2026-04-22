package eventlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Subscribe returns a live subscription. opts.Since selects the starting cursor:
//   - SinceLive: only events appended after this call returns
//   - SinceBeginning: replay full history then attach to live tail
//   - N>0: replay seq>N then attach to live tail
//
// History replay always reads through the SQLite log so we observe the same
// committed events that the ring does, but the cut-over to live is gap-free:
// we register the subscriber against the ring before starting the replay, and
// drop any live event whose seq has already been delivered from history.
func (l *SQLiteLog) Subscribe(ctx context.Context, opts SubscribeOptions) (Subscription, error) {
	if l.closed.Load() {
		return nil, errors.New("eventlog: log closed")
	}
	bufSize := opts.BufferSize
	if bufSize <= 0 {
		bufSize = 256
	}
	sub := &subscription{
		log:        l,
		ch:         make(chan Envelope, bufSize),
		closedCh:   make(chan struct{}),
		partitions: opts.Partitions,
		types:      opts.Types,
		onLag:      opts.OnLag,
	}
	// Attach before history replay so we don't miss live events committed in
	// between the LatestSeq call and the historical scan.
	l.addSubscriber(sub)

	switch {
	case opts.Since == SinceLive:
		max, err := l.LatestSeq(ctx)
		if err != nil {
			l.removeSubscriber(sub)
			return nil, err
		}
		atomic.StoreInt64(&sub.cursor, max)
	case opts.Since == SinceBeginning, opts.Since > 0:
		startFrom := int64(0)
		if opts.Since > 0 {
			startFrom = int64(opts.Since)
		}
		atomic.StoreInt64(&sub.cursor, startFrom)
		go sub.replay(ctx, startFrom)
	default:
		// Negative custom values fall back to live to avoid surprises.
		max, err := l.LatestSeq(ctx)
		if err != nil {
			l.removeSubscriber(sub)
			return nil, err
		}
		atomic.StoreInt64(&sub.cursor, max)
	}
	return sub, nil
}

// subscription is the live consumer attached to a SQLiteLog.
type subscription struct {
	log        *SQLiteLog
	ch         chan Envelope
	closedCh   chan struct{}
	closeOnce  sync.Once
	partitions []string // empty = all
	types      []string // empty = all
	onLag      func(int64)

	cursor     int64 // highest seq delivered to ch (or replay so-far)
	dropped    atomic.Int64
	replaying  atomic.Bool
	pending    sync.Mutex // protects pendingBuf during replay handoff
	pendingBuf []Envelope
}

func (s *subscription) C() <-chan Envelope { return s.ch }
func (s *subscription) Cursor() int64      { return atomic.LoadInt64(&s.cursor) }

func (s *subscription) Lag() int64 {
	max, err := s.log.LatestSeq(context.Background())
	if err != nil {
		return 0
	}
	cur := atomic.LoadInt64(&s.cursor)
	if cur >= max {
		return 0
	}
	return max - cur
}

func (s *subscription) Close() error {
	s.closeOnce.Do(func() {
		s.log.removeSubscriber(s)
		close(s.closedCh)
		close(s.ch)
	})
	return nil
}

// deliver is invoked by SQLiteLog.fanout for every committed envelope.
// During history replay we buffer live events into pendingBuf; replay drains
// pendingBuf when it catches up.
func (s *subscription) deliver(envs []Envelope) {
	if s.replaying.Load() {
		s.pending.Lock()
		s.pendingBuf = append(s.pendingBuf, envs...)
		s.pending.Unlock()
		return
	}
	for _, env := range envs {
		s.send(env)
	}
}

// send applies partition/type filters and pushes env to ch (drop-if-full so
// slow consumers can't back-pressure SQLite).
func (s *subscription) send(env Envelope) {
	if !s.matches(env) {
		// still advance cursor so Lag stays meaningful
		atomic.StoreInt64(&s.cursor, env.Seq)
		return
	}
	select {
	case s.ch <- env:
		atomic.StoreInt64(&s.cursor, env.Seq)
	case <-s.closedCh:
	default:
		s.dropped.Add(1)
		if s.onLag != nil {
			s.onLag(s.dropped.Load())
		}
	}
}

func (s *subscription) matches(env Envelope) bool {
	if len(s.partitions) > 0 && !contains(s.partitions, env.Partition) {
		return false
	}
	if len(s.types) > 0 && !contains(s.types, env.Type) {
		return false
	}
	return true
}

func contains(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}

// replay reads history > startFrom from SQL in pages, then drains pendingBuf
// (events that arrived live during replay) and finally clears the replaying
// flag so deliver() goes straight to send().
func (s *subscription) replay(ctx context.Context, startFrom int64) {
	s.replaying.Store(true)
	defer s.replaying.Store(false)

	const pageSize = 500
	since := Since(startFrom)
	for {
		select {
		case <-s.closedCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		res, err := s.log.ReadAll(ctx, since, pageSize)
		if err != nil {
			// Replay errors close the subscription; the consumer will see
			// channel close and reconnect at its discretion.
			_ = s.Close()
			return
		}
		for _, env := range res.Events {
			s.send(env)
		}
		if !res.HasMore {
			break
		}
		since = res.NextSince
	}

	// Drain any live events accumulated during replay. Loop until the buffer
	// is empty under the lock so deliver() either sees replaying=false or
	// finds an empty buffer plus replaying=true.
	for {
		s.pending.Lock()
		buf := s.pendingBuf
		s.pendingBuf = nil
		if len(buf) == 0 {
			s.pending.Unlock()
			break
		}
		s.pending.Unlock()
		cur := atomic.LoadInt64(&s.cursor)
		for _, env := range buf {
			if env.Seq <= cur {
				continue
			}
			s.send(env)
		}
	}
}

// String is for debug logs.
func (s *subscription) String() string {
	min, max := int64(0), int64(0)
	if s.log != nil && s.log.rng != nil {
		min, max = s.log.rng.snapshot()
	}
	return fmt.Sprintf("eventlog.subscription{cursor=%d, dropped=%d, ringWindow=[%d,%d]}",
		s.Cursor(), s.dropped.Load(), min, max)
}
