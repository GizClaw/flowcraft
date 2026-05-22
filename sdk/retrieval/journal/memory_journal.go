package journal

import (
	"context"
	"iter"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// MemoryJournal is an in-process Journal implementation.
type MemoryJournal struct {
	mu     sync.Mutex
	events []Event
	next   uint64
}

// NewMemoryJournal returns an empty MemoryJournal.
func NewMemoryJournal() *MemoryJournal {
	return &MemoryJournal{}
}

// Record implements Journal. SeqID is assigned when ev.SeqID == 0.
func (m *MemoryJournal) Record(ctx context.Context, ev Event) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	if ev.SeqID == 0 {
		m.next++
		ev.SeqID = m.next
	} else if ev.SeqID > m.next {
		m.next = ev.SeqID
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	ev.Before = cloneDocPtr(ev.Before)
	ev.After = cloneDocPtr(ev.After)
	m.events = append(m.events, ev)
	return nil
}

// History implements Journal.
func (m *MemoryJournal) History(ctx context.Context, namespace, docID string) ([]Event, error) {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events {
		if e.Namespace == namespace && e.DocID == docID {
			out = append(out, e)
		}
	}
	return out, nil
}

// Replay implements Journal.
func (m *MemoryJournal) Replay(ctx context.Context, namespace string, sinceSeq uint64) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, e := range m.events {
			if e.Namespace != namespace || e.SeqID <= sinceSeq {
				continue
			}
			select {
			case <-ctx.Done():
				if !yield(Event{}, ctx.Err()) {
					return
				}
				return
			default:
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}

// Compact implements Journal by dropping events strictly before cutoff.
func (m *MemoryJournal) Compact(ctx context.Context, before time.Time) error {
	_ = ctx
	m.mu.Lock()
	defer m.mu.Unlock()
	dst := m.events[:0]
	for _, e := range m.events {
		if !e.Timestamp.Before(before) {
			dst = append(dst, e)
		}
	}
	m.events = dst
	return nil
}

// Close implements Journal.
func (m *MemoryJournal) Close() error { return nil }

func cloneDocPtr(d *retrieval.Doc) *retrieval.Doc {
	if d == nil {
		return nil
	}
	cp := *d
	if d.Metadata != nil {
		cp.Metadata = make(map[string]any, len(d.Metadata))
		for k, v := range d.Metadata {
			cp.Metadata[k] = v
		}
	}
	if len(d.Vector) > 0 {
		cp.Vector = append([]float32(nil), d.Vector...)
	}
	if len(d.SparseVector) > 0 {
		cp.SparseVector = make(map[string]float32, len(d.SparseVector))
		for k, v := range d.SparseVector {
			cp.SparseVector[k] = v
		}
	}
	return &cp
}
