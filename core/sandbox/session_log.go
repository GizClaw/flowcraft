package sandbox

import (
	"context"
	"fmt"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// watcherQueueSize bounds each event subscriber's live queue. Overflow
// surfaces as SessionEventLag followed by the watcher closing;
// consumers recover with Read(afterSeq).
const watcherQueueSize = 256

// sessionWriter is exec.Cmd's stdout/stderr sink for pipe sessions.
// cmd.Wait waits for these writers, so output ordering relative to
// process exit is exact.
type sessionWriter struct {
	out    *outputLog
	stream SessionStream
}

func (w sessionWriter) Write(p []byte) (int, error) {
	w.out.append(w.stream, p)
	return len(p), nil
}

// outputLog is the append-only, bounded, replayable output buffer. Seq
// is a byte cursor: each chunk records the sequence of its first byte
// and the log advances by len(data). When the ring budget is exceeded,
// oldest whole chunks are dropped and Read reports ErrSequenceGap for
// cursors below the retained range.
type outputLog struct {
	mu          sync.Mutex
	wake        chan struct{}
	chunks      []outputChunk
	total       int64
	nextSeq     int64
	max         int64
	eof         bool
	closed      bool
	exit        SessionExit
	subscribers []*watcher
}

type outputChunk struct {
	seq    int64
	stream SessionStream
	data   []byte
}

func newOutputLog(maxBytes int64) *outputLog {
	return &outputLog{wake: make(chan struct{}), max: maxBytes}
}

func (l *outputLog) append(stream SessionStream, data []byte) {
	if len(data) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.chunks = append(l.chunks, outputChunk{
		seq:    l.nextSeq,
		stream: stream,
		data:   append([]byte(nil), data...),
	})
	l.total += int64(len(data))
	l.nextSeq += int64(len(data))
	l.trimLocked()
	l.deliver(SessionEvent{
		Seq:    l.nextSeq - int64(len(data)),
		Stream: stream,
		Data:   l.chunks[len(l.chunks)-1].data,
		Type:   SessionEventOutput,
	})
	l.wakeReadersLocked()
}

// finish marks the stream complete and pushes Exited to every
// subscriber. The watcher channels stay open until Close, watcher
// Close, or ctx cancellation.
func (l *outputLog) finish(exit SessionExit) {
	l.mu.Lock()
	l.eof = true
	l.exit = exit
	l.deliver(SessionEvent{Seq: l.nextSeq, Type: SessionEventExited, Exit: &l.exit})
	l.wakeReadersLocked()
	l.mu.Unlock()
}

// close marks the log closed, pushes Closed to every subscriber, and
// terminates their channels and feed goroutines.
func (l *outputLog) close() {
	l.mu.Lock()
	l.closed = true
	subs := l.subscribers
	l.subscribers = nil
	for _, w := range subs {
		w.deliver(SessionEvent{Seq: l.nextSeq, Type: SessionEventClosed})
		w.once.Do(func() { close(w.ch) })
		w.stopOnce.Do(func() { close(w.stop) })
	}
	l.wakeReadersLocked()
	l.mu.Unlock()
}

// subscribe registers a new independent watcher. It must be called
// with l.mu held; Watch then replays retained chunks before releasing
// the lock, so no append can slip between replay and subscription.
func (l *outputLog) subscribe() *watcher {
	w := &watcher{
		ch:   make(chan SessionEvent, watcherQueueSize),
		stop: make(chan struct{}),
		log:  l,
	}
	l.subscribers = append(l.subscribers, w)
	return w
}

// deliver pushes one event to every active subscriber, dropping
// watchers that lagged (they close themselves after the Lag event).
// Callers hold l.mu; all sends are non-blocking.
func (l *outputLog) deliver(ev SessionEvent) {
	kept := l.subscribers[:0]
	for _, w := range l.subscribers {
		if w.deliver(ev) {
			kept = append(kept, w)
		}
	}
	l.subscribers = kept
}

func (l *outputLog) removeSubscriber(w *watcher) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, sub := range l.subscribers {
		if sub == w {
			l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
			return
		}
	}
}

// watcher is one event subscription. Events is replay-then-live; the
// channel closes on ctx cancellation, watcher.Close, or after the
// process's Closed event. On queue overflow the subscriber receives
// one SessionEventLag (Seq = first missed byte cursor) and the
// channel closes — the consumer recovers with Read(afterSeq).
type watcher struct {
	ch       chan SessionEvent
	stop     chan struct{}
	once     sync.Once
	stopOnce sync.Once
	log      *outputLog
}

func (w *watcher) Events() <-chan SessionEvent { return w.ch }

func (w *watcher) Close() error {
	w.stopOnce.Do(func() { close(w.stop) })
	return nil
}

func (w *watcher) run(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-w.stop:
	}
	w.log.removeSubscriber(w)
	w.once.Do(func() { close(w.ch) })
}

// deliver performs a non-blocking push. On overflow it makes room for
// one Lag event (the first undelivered event's cursor), closes the
// channel, and returns false so the log detaches the watcher.
func (w *watcher) deliver(ev SessionEvent) bool {
	select {
	case w.ch <- ev:
		return true
	default:
	}
	select {
	case <-w.ch:
	default:
	}
	select {
	case w.ch <- SessionEvent{Seq: ev.Seq, Type: SessionEventLag}:
	default:
	}
	w.once.Do(func() { close(w.ch) })
	w.stopOnce.Do(func() { close(w.stop) })
	return false
}

// watchOutputLog subscribes one independent watcher to l, replaying
// retained output before delivering live events, per the
// Session.Watch contract. The caller must already have verified the
// session is not closed; the returned watcher is a SessionWatcher.
func watchOutputLog(ctx context.Context, l *outputLog) (SessionWatcher, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrSessionClosed
	}
	w := l.subscribe()
	if len(l.chunks) > watcherQueueSize {
		// Replay alone would overflow the queue; skip the partial
		// replay and deliver one Lag from the retained start instead.
		start := l.retainedSeqLocked()
		w.ch <- SessionEvent{Seq: start, Type: SessionEventLag}
		for i, sub := range l.subscribers {
			if sub == w {
				l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
				break
			}
		}
		w.once.Do(func() { close(w.ch) })
		return w, nil
	}
	for _, chunk := range l.chunks {
		w.ch <- SessionEvent{
			Seq:    chunk.seq,
			Stream: chunk.stream,
			Data:   chunk.data,
			Type:   SessionEventOutput,
		}
	}
	if l.eof {
		w.ch <- SessionEvent{Seq: l.nextSeq, Type: SessionEventExited, Exit: &l.exit}
	}
	go w.run(ctx)
	return w, nil
}

// read returns output at/after afterSeq, at most maxBytes, blocking
// until data, EOF, or ctx cancellation.
func (l *outputLog) read(ctx context.Context, afterSeq int64, maxBytes int) (SessionOutput, error) {
	if maxBytes <= 0 {
		return SessionOutput{}, errdefs.Validationf("sandbox: Read maxBytes must be positive")
	}
	l.mu.Lock()
	for {
		if l.closed {
			l.mu.Unlock()
			return SessionOutput{}, ErrSessionClosed
		}
		if retained := l.retainedSeqLocked(); retained > afterSeq {
			l.mu.Unlock()
			return SessionOutput{}, fmt.Errorf("%w: afterSeq %d, retained from %d", ErrSequenceGap, afterSeq, retained)
		}
		if l.nextSeq < afterSeq {
			l.mu.Unlock()
			return SessionOutput{}, errdefs.Validationf(
				"sandbox: afterSeq %d is beyond buffered output (next=%d)", afterSeq, l.nextSeq)
		}
		if out, ok := l.collectLocked(afterSeq, maxBytes); ok {
			l.mu.Unlock()
			return out, nil
		}
		if l.eof {
			l.mu.Unlock()
			return SessionOutput{NextSeq: afterSeq, EOF: true}, nil
		}
		wake := l.wake
		l.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return SessionOutput{}, ctx.Err()
		}
		l.mu.Lock()
	}
}

func (l *outputLog) collectLocked(afterSeq int64, maxBytes int) (SessionOutput, bool) {
	remaining := int64(maxBytes)
	next := afterSeq
	var chunks []OutputChunk
	for _, ch := range l.chunks {
		if remaining <= 0 {
			break
		}
		end := ch.seq + int64(len(ch.data))
		if next >= end {
			continue
		}
		start := next - ch.seq
		n := int64(len(ch.data)) - start
		if n > remaining {
			n = remaining
		}
		chunks = append(chunks, OutputChunk{
			Seq:    next,
			Stream: ch.stream,
			Data:   append([]byte(nil), ch.data[start:start+n]...),
		})
		next += n
		remaining -= n
	}
	if len(chunks) == 0 {
		return SessionOutput{}, false
	}
	return SessionOutput{
		NextSeq: next,
		Chunks:  chunks,
		EOF:     l.eof && next == l.nextSeq,
	}, true
}

func (l *outputLog) retainedSeqLocked() int64 {
	if len(l.chunks) == 0 {
		return l.nextSeq
	}
	return l.chunks[0].seq
}

func (l *outputLog) trimLocked() {
	if l.max <= 0 {
		return
	}
	// Never drop the only chunk: a single chunk larger than the budget
	// is bounded by sessionCopyChunk and stays replayable.
	for len(l.chunks) > 1 && l.total > l.max {
		l.total -= int64(len(l.chunks[0].data))
		l.chunks = l.chunks[1:]
	}
}

// wakeReadersLocked notifies blocked Reads that the log changed. The
// closed channel is immediately replaced so future waits get a fresh
// signal channel.
func (l *outputLog) wakeReadersLocked() {
	close(l.wake)
	l.wake = make(chan struct{})
}
