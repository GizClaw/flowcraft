package knowledge

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeNotifier feeds ChangeEvents into the reloader on demand.
type fakeNotifier struct {
	out    chan ChangeEvent
	closed bool
}

func newFakeNotifier() *fakeNotifier { return &fakeNotifier{out: make(chan ChangeEvent, 16)} }

func (n *fakeNotifier) Events() <-chan ChangeEvent { return n.out }

func (n *fakeNotifier) Close() error {
	if n.closed {
		return nil
	}
	n.closed = true
	close(n.out)
	return nil
}

// recordingRebuilder counts Rebuild invocations and remembers each scope.
type recordingRebuilder struct {
	mu     sync.Mutex
	scopes []RebuildScope
	done   chan struct{}
	want   int
}

func newRecording(want int) *recordingRebuilder {
	return &recordingRebuilder{done: make(chan struct{}), want: want}
}

func (r *recordingRebuilder) Rebuild(_ context.Context, scope RebuildScope) error {
	r.mu.Lock()
	r.scopes = append(r.scopes, scope)
	if len(r.scopes) >= r.want {
		select {
		case <-r.done:
		default:
			close(r.done)
		}
	}
	r.mu.Unlock()
	return nil
}

func (r *recordingRebuilder) wait(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(timeout):
		t.Fatalf("rebuild did not fire within %s (got %d, want %d)", timeout, len(r.scopes), r.want)
	}
}

func TestEventReloader_DebouncesAndCollapsesSingleDoc(t *testing.T) {
	rb := newRecording(1)
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{Debounce: 20 * time.Millisecond})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	rb.wait(t, time.Second)
	_ = r.Close()
	if len(rb.scopes) != 1 {
		t.Fatalf("expected 1 rebuild, got %d", len(rb.scopes))
	}
	if rb.scopes[0] != (RebuildScope{DatasetID: "ds", DocName: "a.md"}) {
		t.Fatalf("scope = %+v, want {ds,a.md}", rb.scopes[0])
	}
}

func TestEventReloader_MultipleDocsCollapseToDataset(t *testing.T) {
	rb := newRecording(1)
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{Debounce: 20 * time.Millisecond})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "b.md", Kind: EventPut}
	rb.wait(t, time.Second)
	_ = r.Close()
	if rb.scopes[0] != (RebuildScope{DatasetID: "ds"}) {
		t.Fatalf("scope = %+v, want dataset-wide", rb.scopes[0])
	}
}

func TestEventReloader_MultipleDatasetsCollapseToGlobal(t *testing.T) {
	rb := newRecording(1)
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{Debounce: 20 * time.Millisecond})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds1", DocName: "a.md", Kind: EventPut}
	notif.out <- ChangeEvent{DatasetID: "ds2", DocName: "b.md", Kind: EventPut}
	rb.wait(t, time.Second)
	_ = r.Close()
	if rb.scopes[0] != (RebuildScope{}) {
		t.Fatalf("scope = %+v, want global", rb.scopes[0])
	}
}

func TestEventReloader_BulkEventScopesDataset(t *testing.T) {
	rb := newRecording(1)
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{Debounce: 20 * time.Millisecond})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds", Kind: EventBulk}
	rb.wait(t, time.Second)
	_ = r.Close()
	if rb.scopes[0] != (RebuildScope{DatasetID: "ds"}) {
		t.Fatalf("scope = %+v, want dataset-wide", rb.scopes[0])
	}
}

func TestEventReloader_NilNotifierIsNoop(t *testing.T) {
	r := NewEventReloader(newRecording(0), nil, ReloaderOptions{})
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v", err)
	}
}

func TestEventReloader_CloseStopsRun(t *testing.T) {
	rb := newRecording(0)
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{Debounce: 50 * time.Millisecond})
	done := make(chan struct{})
	go func() {
		_ = r.Run(context.Background())
		close(done)
	}()
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Run did not return after Close")
	}
}

func TestEventReloader_SerialisesRebuilds(t *testing.T) {
	rb := newRecording(2)
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{Debounce: 10 * time.Millisecond})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	time.Sleep(40 * time.Millisecond)
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "b.md", Kind: EventPut}
	rb.wait(t, time.Second)
	_ = r.Close()
	if len(rb.scopes) < 2 {
		t.Fatalf("expected >= 2 rebuilds, got %d", len(rb.scopes))
	}
}
