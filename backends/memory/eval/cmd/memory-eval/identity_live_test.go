package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	msgsource "github.com/GizClaw/flowcraft/backends/memory/sources/message"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestIngestTagsDatasetTurns checks the channel evidence recall is matched by
// when the store carries it: ingest records the dataset turn each message was
// loaded from on the committed message, so grading can name the turn instead of
// recognizing it by its text (the two disagree for an image turn, whose stored
// text may not hold the caption the loader rendered).
//
// It ingests a conversation of its own instead of reading the runner's
// workspace, because ids only reach a store written after ingest tagged
// messages: the sessions commit under fixed idempotency keys, so re-ingesting an
// older workspace replays the original commits and writes nothing. It runs no
// derive pass and asks no model anything, so it is cheap next to the probes that
// measure a derivation.
func TestIngestTagsDatasetTurns(t *testing.T) {
	envPath := liveEnvFile(t)
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	loadEnvFile(envPath)
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to check how ingest tags dataset turns")
	}
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{
		Scope:  eval.Scope{RuntimeID: "memories"},
		Budget: corememory.Budget{MaxItems: 30, MaxTokens: 6144},
		// Samples keeps the image downloader to the one conversation that runs.
		Samples: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 {
		t.Fatalf("loaded %d scenarios, want 1", len(scenarios))
	}
	scenario := scenarios[0]

	// A fresh workspace: writeLiveDeploy points a copy of deploy.yaml at a temp
	// root, which is what makes the commits below new writes rather than
	// replays of what the runner's store already holds.
	built := buildAssembly(writeLiveDeploy(t, workdir, 1), 8)
	defer built.Close()
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	store := built.Memory.MessageStore()

	want := map[string]int{}
	for _, turn := range scenario.Turns {
		for _, id := range turn.DatasetIDs {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				want[trimmed]++
			}
		}
	}
	if len(want) == 0 {
		t.Fatal("the loader named no dataset turns, so this probe could not tell an untagged store from a tagged one")
	}
	if err := eval.Ingest(ctx, built.Memory, scenario); err != nil {
		t.Fatal(err)
	}
	records := latestRecords(t, store, scope, scenario.ConversationID)
	got := map[string]int{}
	for _, record := range records {
		id := strings.TrimSpace(record.Metadata[eval.DatasetTurnMetadataKey])
		if id == "" {
			t.Fatalf("record %s carries no dataset turn id: %#v", record.ID, record.Metadata)
		}
		got[id]++
	}
	// Alignment, not just presence: a tag that landed on the wrong message
	// would still make every record look tagged.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("committed turn ids = %v, want %v", got, want)
	}
	// The runner's own guard decides whether it warns about an untagged store
	// and which rule it stamps in the fingerprint, so it has to agree with the
	// records it just read.
	resolver := host.NewMessageProvenance(store, scope)
	if !resolver.CarriesTurnIDs(ctx, scenario.ConversationID, 64) {
		t.Fatal("CarriesTurnIDs reports no turn ids after every record was tagged")
	}
	// Re-ingesting is a replay, not a repair: the ids are written by ingest, so
	// the only way to reach a workspace that predates them is to ingest into a
	// fresh one.
	if err := eval.Ingest(ctx, built.Memory, scenario); err != nil {
		t.Fatal(err)
	}
	if replayed := latestRecords(t, store, scope, scenario.ConversationID); len(replayed) != len(records) {
		t.Fatalf("re-ingest holds %d records over %d: an idempotent replay writes nothing, so re-ingesting cannot bring an untagged store up to date",
			len(replayed), len(records))
	}
	t.Logf("ingest committed %d records, all tagged with one of %d dataset turns; the re-ingest replayed them",
		len(records), len(want))
}

func latestRecords(t *testing.T, store *msgsource.MessageStore, scope corememory.Scope, conversationID string) []msgsource.Record {
	t.Helper()
	records, err := store.Latest(context.Background(), scope, conversationID, msgsource.LatestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return records
}
