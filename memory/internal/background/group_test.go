package background

import (
	"context"
	"testing"
	"time"
)

func TestGroupCloseCancelsAndWaits(t *testing.T) {
	g := NewGroup(context.Background())
	workerDone := make(chan struct{})
	if !g.Start(func(ctx context.Context) {
		<-ctx.Done()
		close(workerDone)
	}) {
		t.Fatal("Start returned false")
	}

	closeDone := make(chan struct{})
	go func() {
		g.Close()
		close(closeDone)
	}()

	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe group cancellation")
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for worker to drain")
	}
	select {
	case <-g.Done():
	default:
		t.Fatal("Done channel is not closed after Close")
	}
}

func TestGroupRejectsStartAfterClose(t *testing.T) {
	g := NewGroup(context.Background())
	g.Close()

	if g.Start(func(context.Context) {}) {
		t.Fatal("Start succeeded after Close")
	}
	g.Close()
}

func TestGroupParentCancellationPropagates(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	g := NewGroup(parent)
	workerDone := make(chan struct{})
	if !g.Start(func(ctx context.Context) {
		<-ctx.Done()
		close(workerDone)
	}) {
		t.Fatal("Start returned false")
	}

	cancel()
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe parent cancellation")
	}
	g.Close()
}
