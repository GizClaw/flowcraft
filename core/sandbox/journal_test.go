package sandbox_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// stubJournal is a do-nothing FileJournal: enough to prove that the
// journal a runner hands out survives decoration.
type stubJournal struct {
	closed bool
}

func (j *stubJournal) Read(context.Context, int64, int) (sandbox.JournalBatch, error) {
	return sandbox.JournalBatch{}, nil
}

func (j *stubJournal) Close() error {
	j.closed = true
	return nil
}

// journalRunner is a Runner that provides a journal.
type journalRunner struct {
	closingRunner
	journal sandbox.FileJournal
}

func (r *journalRunner) OpenJournal(context.Context) (sandbox.FileJournal, error) {
	if r.journal == nil {
		return nil, errdefs.NotAvailablef("test runner has no journal")
	}
	return r.journal, nil
}

// TestOpenJournalThroughTheDecoratorChain proves the composition rule
// the contract states: a decorator forwards the journal instead of
// hiding it, so the journal a caller gets through the recommended chain
// is the backend's own.
func TestOpenJournalThroughTheDecoratorChain(t *testing.T) {
	inner := &journalRunner{journal: &stubJournal{}}
	chain := sandbox.WithDefaults(
		sandbox.WithApproval(
			sandbox.AllowCommands(inner, nil),
			nil, nil,
		),
		sandbox.ExecOptions{},
	)

	reader, err := sandbox.OpenJournal(context.Background(), chain)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if reader != inner.journal {
		t.Fatal("the decorator chain handed out a journal that is not the backend's")
	}
	// The decorators are themselves providers, so the assertion happens
	// once, inside the package, instead of at every call site.
	if _, ok := chain.(sandbox.JournalProvider); !ok {
		t.Fatal("the decorated runner does not implement JournalProvider")
	}
}

// TestOpenJournalWithoutAProviderIsNotAvailable covers both failure
// shapes: a runner that never had the capability, and one whose journal
// is not attached.
func TestOpenJournalWithoutAProviderIsNotAvailable(t *testing.T) {
	plain := &closingRunner{}
	if _, err := sandbox.OpenJournal(context.Background(), plain); !errdefs.IsNotAvailable(err) {
		t.Fatalf("plain runner: %v, want NotAvailable", err)
	}

	empty := sandbox.WithDefaults(&journalRunner{}, sandbox.ExecOptions{})
	if _, err := sandbox.OpenJournal(context.Background(), empty); !errdefs.IsNotAvailable(err) {
		t.Fatalf("runner without an attached journal: %v, want NotAvailable", err)
	}
	if _, err := sandbox.OpenJournal(context.Background(), nil); !errdefs.IsValidation(err) {
		t.Fatalf("nil runner: %v, want Validation", err)
	}
}

// TestDecoratorsForwardJournalCapabilities keeps the two halves of the
// contract together: the declaration travels through the decorators, and
// so does the journal the declaration is about.
func TestDecoratorsForwardJournalCapabilities(t *testing.T) {
	inner := &journalRunner{journal: &stubJournal{}}
	chain := sandbox.AllowCommands(sandbox.WithDefaults(inner, sandbox.ExecOptions{}), nil)

	if got := chain.Capabilities().Journal; got.Enabled {
		t.Fatal("the stub declares no journal capability, but the chain added one")
	}
	// The declaration is the inner one, whatever it says.
	if got, want := chain.Capabilities().Journal, inner.Capabilities().Journal; got != want {
		t.Fatalf("Journal capabilities = %+v, want the inner declaration %+v", got, want)
	}
}

func TestFileOpSpellingsRoundTrip(t *testing.T) {
	for _, want := range []sandbox.FileOp{
		sandbox.FileOpCreate,
		sandbox.FileOpWrite,
		sandbox.FileOpRename,
		sandbox.FileOpRemove,
	} {
		got, ok := sandbox.ParseFileOp(want.String())
		if !ok || got != want {
			t.Fatalf("ParseFileOp(%q) = %v, %v; want %v, true", want.String(), got, ok, want)
		}
	}
	if _, ok := sandbox.ParseFileOp("chmod"); ok {
		t.Fatal("ParseFileOp accepted an operation the journal never reports")
	}
	if got := sandbox.FileOp(200).String(); got != "unknown" {
		t.Fatalf("unknown FileOp renders as %q", got)
	}
	if got := sandbox.JournalGapReason(200).String(); got != "unknown" {
		t.Fatalf("unknown gap reason renders as %q", got)
	}
	for _, reason := range []sandbox.JournalGapReason{
		sandbox.JournalGapOverflow,
		sandbox.JournalGapRetention,
		sandbox.JournalGapCapacity,
		sandbox.JournalGapWatchLost,
		sandbox.JournalGapBackend,
	} {
		if reason.String() == "unknown" {
			t.Fatalf("gap reason %d has no spelling", reason)
		}
	}
}

// TestZeroJournalCapabilities means "no journal": everything else in the
// contract is written against that assumption.
func TestZeroJournalCapabilitiesMeansNoJournal(t *testing.T) {
	var caps sandbox.Capabilities
	if caps.Journal.Enabled || caps.Journal.WriterID || caps.Journal.RenamePairing ||
		caps.Journal.FileIdentity || caps.Journal.WatchBudget != 0 {
		t.Fatalf("zero Journal capabilities = %+v, want the absent surface", caps.Journal)
	}
}

func TestJournalGapIsNotAnError(t *testing.T) {
	// The gap is a value, not an error: it travels with a successful
	// read so a consumer can distinguish "nothing happened" from "I
	// cannot tell you what happened". Nothing in the contract should
	// ever turn it into a failure.
	var batch sandbox.JournalBatch
	if batch.Gap != nil {
		t.Fatal("a zero batch must not invent a gap")
	}
	batch.Gap = &sandbox.JournalGap{FirstMissing: 7, Reason: sandbox.JournalGapOverflow}
	if batch.Gap.Reason.String() != "overflow" {
		t.Fatalf("reason = %q", batch.Gap.Reason)
	}
	if batch.Gap.FirstMissing != 7 {
		t.Fatalf("FirstMissing = %d", batch.Gap.FirstMissing)
	}
}
