package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
)

// liveEnvFile resolves the credentials file the live lanes need. The README
// tells operators to keep it at the repository root, while earlier runs put it
// next to the eval module; both are accepted so following the docs does not
// silently skip every credentialed test.
func liveEnvFile(t *testing.T) string {
	t.Helper()
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	candidates := []string{
		filepath.Join(workdir, "..", "..", ".env"),
		filepath.Join(workdir, "..", "..", "..", "..", "..", ".env"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skipf("no credentials file in %v", candidates)
	return ""
}

// requireTurnIDs fails when the store a probe reads was written before ingest
// tagged each message with its dataset turn id. Every probe here measures
// evidence coverage, and on an untagged store only the committed text can be
// matched: the probes would report a store that needs re-ingesting as a
// retrieval that missed. Re-ingesting into the same store is not enough -- the
// sessions are committed under fixed idempotency keys, so a replay returns the
// original commits and writes nothing new; the workspace has to be fresh.
func requireTurnIDs(t *testing.T, resolver host.MessageProvenance, scenarios []eval.Scenario) {
	t.Helper()
	for _, scenario := range scenarios {
		if resolver.CarriesTurnIDs(context.Background(), scenario.ConversationID, 64) {
			return
		}
	}
	t.Fatalf("the store carries no dataset turn ids: it was ingested before ingest tagged messages.\n" +
		"\tRun the harness against a fresh workspace (deploy.yaml's workspace root), then rerun this probe:\n" +
		"\ta re-ingest into this one replays the same idempotency keys and returns the original commits.")
}
