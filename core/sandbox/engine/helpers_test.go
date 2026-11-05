package engine_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// drainSession reads the session's output to EOF and returns it. The helper was
// shared across the old single-package tests; the split keeps a copy here because
// a test file cannot span packages.
func drainSession(t *testing.T, ctx context.Context, sess sandbox.Session) string {
	t.Helper()
	var out []byte
	afterSeq := int64(0)
	for {
		output, err := sess.Read(ctx, afterSeq, 64*1024)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, chunk := range output.Chunks {
			out = append(out, chunk.Data...)
		}
		afterSeq = output.NextSeq
		if output.EOF {
			return string(out)
		}
	}
}
