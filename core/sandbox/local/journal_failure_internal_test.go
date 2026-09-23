package local

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
)

// TestFactoryFailsWhenTheJournalCannotStart keeps the promise the
// settings docs make: a deployment that asked for a journal either gets
// one or gets an error — never a runner whose journal silently watches
// nothing. The engine is replaced so the path is reachable without
// exhausting the host's own watch resources.
func TestFactoryFailsWhenTheJournalCannotStart(t *testing.T) {
	if !journal.Available() {
		t.Skipf("no file-watch source on this platform")
	}
	root := t.TempDir()
	quoted, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}

	previous := newJournalEngine
	newJournalEngine = func(journal.Config) (*journal.Journal, error) {
		return nil, errors.New("watch instances exhausted")
	}
	t.Cleanup(func() { newJournalEngine = previous })

	value, err := Factory{}.New(context.Background(), resource.Input{
		Settings: json.RawMessage(`{"root": ` + string(quoted) + `, "journal": {}}`),
	})
	if err == nil {
		if runner, ok := value.(*Runner); ok {
			_ = runner.Close()
		}
		t.Fatal("the factory returned a runner although the journal could not start")
	}
	if !strings.Contains(err.Error(), "watch instances exhausted") {
		t.Fatalf("error = %v, want the engine's own reason in it", err)
	}
}
