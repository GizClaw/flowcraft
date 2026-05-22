package knowledge

import (
	"context"
	"errors"
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

type retryRebuilder struct {
	mu        sync.Mutex
	calls     int
	scopes    []RebuildScope
	succeeded chan struct{}
}

func (r *retryRebuilder) Rebuild(_ context.Context, scope RebuildScope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.scopes = append(r.scopes, scope)
	if r.calls == 1 {
		return errors.New("transient")
	}
	select {
	case <-r.succeeded:
	default:
		close(r.succeeded)
	}
	return nil
}

func TestEventReloader_RetriesAndKeepsPendingAfterError(t *testing.T) {
	rb := &retryRebuilder{succeeded: make(chan struct{})}
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{
		Debounce:        5 * time.Millisecond,
		RetryBackoff:    5 * time.Millisecond,
		MaxRetryBackoff: 10 * time.Millisecond,
	})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	select {
	case <-rb.succeeded:
	case <-time.After(time.Second):
		t.Fatalf("rebuild did not retry after transient error")
	}
	_ = r.Close()
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.calls != 2 {
		t.Fatalf("calls = %d, want 2", rb.calls)
	}
	want := RebuildScope{DatasetID: "ds", DocName: "a.md"}
	if rb.scopes[0] != want || rb.scopes[1] != want {
		t.Fatalf("pending scope was not preserved across retry: %+v", rb.scopes)
	}
}

type cancelAwareRebuilder struct {
	started  chan struct{}
	canceled chan struct{}
}

func (r *cancelAwareRebuilder) Rebuild(ctx context.Context, scope RebuildScope) error {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	return ctx.Err()
}

func TestEventReloader_CloseCancelsInFlightRebuild(t *testing.T) {
	rb := &cancelAwareRebuilder{started: make(chan struct{}), canceled: make(chan struct{})}
	notif := newFakeNotifier()
	r := NewEventReloader(rb, notif, ReloaderOptions{
		Debounce:       5 * time.Millisecond,
		RebuildTimeout: time.Minute,
	})
	go func() { _ = r.Run(context.Background()) }()
	notif.out <- ChangeEvent{DatasetID: "ds", DocName: "a.md", Kind: EventPut}
	select {
	case <-rb.started:
	case <-time.After(time.Second):
		t.Fatalf("rebuild did not start")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-rb.canceled:
	case <-time.After(time.Second):
		t.Fatalf("Close did not cancel in-flight rebuild")
	}
}
