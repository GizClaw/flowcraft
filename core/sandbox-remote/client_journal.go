package remote

import (
	"context"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
)

// journal is the client-side reader of the server's file journal.
//
// It holds no cursor: Read takes one, exactly like the contract's FileJournal,
// so several readers (and several clients) can each hold their own position over
// the server's single reader.
type journal struct {
	runner *Runner
}

// Read implements sandbox.FileJournal. It never waits: the server answers with
// whatever it has, and a Gap in the batch is a statement about coverage rather
// than an error.
func (j *journal) Read(ctx context.Context, afterSeq int64, max int) (sandbox.JournalBatch, error) {
	var out sandbox.JournalBatch
	err := j.runner.retry(ctx, OpJournal, wire.JournalRequest{AfterSeq: afterSeq, Max: max}, &out)
	return out, err
}

// Close implements sandbox.FileJournal. It releases only this reader: the journal
// belongs to the server's runner, and the server closes it with the server.
func (j *journal) Close() error { return nil }

var _ sandbox.FileJournal = (*journal)(nil)
