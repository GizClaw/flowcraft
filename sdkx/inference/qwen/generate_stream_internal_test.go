package qwen

import (
	"bufio"
	"context"
	"io"
	"testing"
	"time"
)

func TestSSEStreamNextHonorsCancelledContext(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	defer func() { _ = pr.Close() }()

	s := &sseStream{body: pr, scanner: bufio.NewScanner(pr)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := make(chan error, 1)
	go func() {
		_, err := s.Next(ctx)
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Next returned nil error after context cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next blocked after context cancellation; server sent no data")
	}
}
