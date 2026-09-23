package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// liveDeriveCounters is the model-in-the-loop evidence a live run produces.
type liveDeriveCounters struct {
	facts           int
	wallclock       time.Duration
	factIDs         map[string]string // fact id -> conversation that owns it
	perConversation map[string]int
}

// TestDeriveConcurrencyLiveMatchesSequential is the credentialed half of the
// derive.concurrency verification: it derives the same three conversations with
// one worker and with four workers against the real providers and checks the
// invariants that concurrency must not break -- every conversation reaches its
// last commit, every conversation publishes facts, and no fact ever appears
// under a second conversation. Byte equality of the derived text cannot be
// asserted here because the model samples; that half is covered
// deterministically by worker.TestProcessScopeParallelDerivesIdenticalFacts.
func TestDeriveConcurrencyLiveMatchesSequential(t *testing.T) {
	envPath := liveEnvFile(t)
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	loadEnvFile(envPath)
	if os.Getenv("DEEPSEEK_API_KEY") == "" || os.Getenv("ARK_API_KEY") == "" {
		t.Skip("DEEPSEEK_API_KEY and ARK_API_KEY are required")
	}
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		// Opt-in so an ordinary `go test ./...` / `make ci` neither pays for
		// model calls nor waits on the network.
		t.Skip("set MEMORY_EVAL_LIVE=1 to run the credentialed live A/B")
	}
	scenarios := liveScenarios(t, workdir)

	runs := map[int]liveDeriveCounters{}
	for _, concurrency := range []int{1, 4} {
		deployPath := writeLiveDeploy(t, workdir, concurrency)
		started := time.Now()
		counters := runLiveDerivation(t, deployPath, scenarios)
		counters.wallclock = time.Since(started)
		runs[concurrency] = counters
		t.Logf("derive.concurrency=%d: conversations=%d facts=%d per_conversation=%v wall=%s",
			concurrency, len(scenarios), counters.facts, counters.perConversation, counters.wallclock.Round(time.Second))

		if counters.facts == 0 {
			t.Fatalf("concurrency=%d derived no facts at all", concurrency)
		}
		for _, scenario := range scenarios {
			if counters.perConversation[scenario.ConversationID] == 0 {
				t.Fatalf("concurrency=%d derived nothing for %s", concurrency, scenario.ConversationID)
			}
		}
	}

	sequential, parallel := runs[1], runs[4]
	owners := map[string]string{}
	for _, counters := range []liveDeriveCounters{sequential, parallel} {
		for id, conversationID := range counters.factIDs {
			if previous, ok := owners[id]; ok && previous != conversationID {
				t.Fatalf("fact %s leaked from %s to %s", id, previous, conversationID)
			}
			owners[id] = conversationID
		}
	}
	t.Logf("parallel run derived %d facts in %s vs %d facts in %s sequentially",
		parallel.facts, parallel.wallclock.Round(time.Second),
		sequential.facts, sequential.wallclock.Round(time.Second))
}

// liveScenarios keeps the run cheap: three conversations, one session each,
// capped to a handful of messages.
func liveScenarios(t *testing.T, workdir string) []eval.Scenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{Scope: eval.Scope{RuntimeID: "memories"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) < 3 {
		t.Fatalf("dataset has %d scenarios, want at least 3", len(scenarios))
	}
	const messagesPerSession = 12
	selected := make([]eval.Scenario, 0, 3)
	for _, scenario := range scenarios[:3] {
		if len(scenario.Turns) == 0 {
			t.Fatalf("scenario %s has no turns", scenario.Name)
		}
		turn := scenario.Turns[0]
		if len(turn.Messages) > messagesPerSession {
			turn.Messages = turn.Messages[:messagesPerSession]
			if len(turn.DatasetIDs) > messagesPerSession {
				turn.DatasetIDs = turn.DatasetIDs[:messagesPerSession]
			}
		}
		trimmed := scenario
		trimmed.Turns = []eval.Turn{turn}
		if len(trimmed.Questions) > liveQuestions {
			trimmed.Questions = trimmed.Questions[:liveQuestions]
		}
		selected = append(selected, trimmed)
	}
	return selected
}

// liveQuestions caps how many questions the retrieval-invariance check probes.
const liveQuestions = 8

// TestCoIngestedConversationsDoNotChangeRetrieval is the two-phase assumption,
// measured instead of assumed: retrieval for one conversation must return the
// same items whether or not other conversations were ingested alongside it.
// It compares recalled item ids (a deterministic quantity), not answers, which
// the model samples.
func TestCoIngestedConversationsDoNotChangeRetrieval(t *testing.T) {
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	envPath := liveEnvFile(t)
	loadEnvFile(envPath)
	if os.Getenv("DEEPSEEK_API_KEY") == "" || os.Getenv("ARK_API_KEY") == "" {
		t.Skip("DEEPSEEK_API_KEY and ARK_API_KEY are required")
	}
	if os.Getenv("MEMORY_EVAL_LIVE") != "1" {
		t.Skip("set MEMORY_EVAL_LIVE=1 to run the credentialed retrieval check")
	}
	scenarios := liveScenarios(t, workdir)
	built := buildAssembly(writeLiveDeploy(t, workdir, 4), 8)
	defer built.Close()
	ctx := context.Background()

	if err := eval.Ingest(ctx, built.Memory, scenarios[0]); err != nil {
		t.Fatal(err)
	}
	deriveWithRetry(t, built.Memory)
	alone := captureRetrieval(t, built, scenarios[0])

	for _, scenario := range scenarios[1:] {
		if err := eval.Ingest(ctx, built.Memory, scenario); err != nil {
			t.Fatal(err)
		}
	}
	deriveWithRetry(t, built.Memory)
	together := captureRetrieval(t, built, scenarios[0])

	if !reflect.DeepEqual(alone, together) {
		for index := range alone {
			if !reflect.DeepEqual(alone[index], together[index]) {
				t.Fatalf("question %q recall changed after co-ingesting:\n alone:    %v\n together: %v",
					scenarios[0].Questions[index].Query, alone[index], together[index])
			}
		}
		t.Fatal("recall changed after co-ingesting")
	}
	t.Logf("recall for %s is identical alone and with %d co-ingested conversations",
		scenarios[0].ConversationID, len(scenarios)-1)
}

// captureRetrieval records the recalled item ids per question.
func captureRetrieval(t *testing.T, built deployment, scenario eval.Scenario) [][]string {
	t.Helper()
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	captured := make([][]string, 0, len(scenario.Questions))
	for _, question := range scenario.Questions {
		result, err := built.Memory.Context(ctx, corememory.ContextRequest{
			Scope: scope, ConversationID: scenario.ConversationID,
			Query: question.Query, Budget: scenario.Budget,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			ids = append(ids, item.ID)
		}
		sort.Strings(ids)
		captured = append(captured, ids)
	}
	return captured
}

// deriveWithRetry runs one derivation pass, retrying from the stored
// watermarks: parallel derivation raises the transient provider error rate.
func deriveWithRetry(t *testing.T, assembly *flowmemory.Assembly) {
	t.Helper()
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		if err = eval.Derive(context.Background(), assembly); err == nil {
			return
		}
		t.Logf("derive attempt %d/3 failed, retrying from stored watermarks: %v", attempt, err)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	t.Fatal(err)
}

// writeLiveDeploy copies deploy.yaml with a private workspace root and the
// requested derive concurrency.
func writeLiveDeploy(t *testing.T, workdir string, concurrency int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document, "      root: ./workspace") {
		t.Fatal("deploy.yaml no longer contains the expected workspace root line")
	}
	document = strings.Replace(document, "      root: ./workspace", "      root: "+root, 1)
	anchor := "      retrieval: {decompose: false}"
	if !strings.Contains(document, anchor) {
		t.Fatal("deploy.yaml no longer contains the expected retrieval line")
	}
	document = strings.Replace(document, anchor,
		fmt.Sprintf("      derive: {concurrency: %d}\n%s", concurrency, anchor), 1)
	path := filepath.Join(t.TempDir(), "deploy.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runLiveDerivation(t *testing.T, deployPath string, scenarios []eval.Scenario) liveDeriveCounters {
	t.Helper()
	built := buildAssembly(deployPath, 8)
	defer built.Close()
	ctx := context.Background()
	scope := corememory.Scope{RuntimeID: "memories"}
	// Ingest every conversation before deriving: derive.concurrency fans out
	// over the conversations that have pending commits, so running the worker
	// after each conversation would leave it with a single stream to process.
	for _, scenario := range scenarios {
		for _, turn := range scenario.Turns {
			if err := built.Memory.CommitTurn(ctx, corememory.Turn{
				Scope: scope, ConversationID: scenario.ConversationID,
				IdempotencyKey: turn.IdempotencyKey, Messages: turn.Messages,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	started := time.Now()
	// A derive pass is resumable: a conversation that fails keeps its own
	// watermark, so retrying picks it up. Parallel passes raise the transient
	// provider error rate, so the retry is part of running this way, not a
	// workaround for the test.
	var deriveErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if deriveErr = built.Memory.RunOnce(ctx); deriveErr == nil {
			break
		}
		t.Logf("derive attempt %d/3 failed, retrying from the stored watermarks: %v", attempt, deriveErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if deriveErr != nil {
		t.Fatal(deriveErr)
	}
	deriveWall := time.Since(started)
	counters := liveDeriveCounters{factIDs: map[string]string{}, perConversation: map[string]int{}}
	for _, scenario := range scenarios {
		facts, err := built.Memory.Facts().List(ctx, scope, scenario.ConversationID, factview.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		counters.facts += len(facts)
		counters.perConversation[scenario.ConversationID] = len(facts)
		for _, fact := range facts {
			counters.factIDs[fact.ID] = scenario.ConversationID
		}
	}
	diagnostics, err := built.Memory.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursors := make([]string, 0, len(diagnostics.Scopes))
	for _, entry := range diagnostics.Scopes {
		for _, conversation := range entry.Conversations {
			if conversation.Behind {
				t.Fatalf("conversation %s is still behind after RunOnce", conversation.ConversationID)
			}
			cursors = append(cursors, fmt.Sprintf("%s=%d", conversation.ConversationID, conversation.Watermark))
		}
	}
	sort.Strings(cursors)
	t.Logf("derive pass took %s; watermarks: %s", deriveWall.Round(time.Millisecond), strings.Join(cursors, " "))
	return counters
}
