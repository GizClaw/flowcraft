package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/eval/internal/host"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// TestPackingRedundancy measures what the 30 packed items actually are: how
// many distinct commits they are drawn from (slots spent on a session the pack
// already reads), and how many of them carry the question's evidence.
//
// The commit is the unit that matters here because the harness commits one
// dataset session at a time and derivation reads a commit whole, so an item's
// provenance is its commit: every fact extracted from one session resolves to
// that one commit whatever turn it paraphrases. Counting distinct source turns
// instead would count the messages inside a commit, which is why it produced a
// negative duplicate share -- items times commit size exceeds items. Retrieval
// only.
//
// Attribution is by turn identity, not by text: every source message carries the
// dataset turn ingest tagged it with, so an item is attributed through the turns
// its canonical sources name. Text was only ever a proxy, and a leaky one -- an
// image turn has two renderings and the store holds one of them: the loader
// folds "[shared image: <caption>]" into the turn's text in annotation mode
// while a store written with "-images native" keeps the caption out of its text
// and attaches the picture as a part, so a loader whose flag does not match the
// store hides every such turn from a text match (the diagnostic's by-text column
// prints how many turns the tag alone found on the store it reads). An item with
// no resolvable source still falls out of the commit counts below.
//
// MEMORY_EVAL_PER_CATEGORY=25 MEMORY_EVAL_MAX_ITEMS=30
func TestPackingRedundancy(t *testing.T) {
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
		t.Skip("set MEMORY_EVAL_LIVE=1 to measure packing")
	}
	perCategory := 25
	maxItems := 30
	if raw := os.Getenv("MEMORY_EVAL_PER_CATEGORY"); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil {
			t.Fatal(convErr)
		}
		perCategory = value
	}
	if raw := os.Getenv("MEMORY_EVAL_MAX_ITEMS"); raw != "" {
		value, convErr := strconv.Atoi(raw)
		if convErr != nil {
			t.Fatal(convErr)
		}
		maxItems = value
	}
	raw, err := os.ReadFile(filepath.Join(workdir, "..", "..", "locomo10.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarios, _, err := eval.LoadLoCoMo(raw, eval.LoaderOptions{Scope: eval.Scope{RuntimeID: "memories"}})
	if err != nil {
		t.Fatal(err)
	}
	built := buildAssembly(filepath.Join(workdir, "..", "..", "deploy.yaml"), 8)
	defer built.Close()
	scope := corememory.Scope{RuntimeID: "memories"}
	resolver := host.NewMessageProvenance(built.Memory.MessageStore(), scope)
	requireTurnIDs(t, resolver, scenarios)

	type sample struct {
		scenario eval.Scenario
		question eval.Question
		// commitByTurn maps a dataset turn to the commit it was ingested in (the
		// loader commits one session per commit).
		commitByTurn map[string]string
		evidence     map[string]struct{}
		// scenarioCommits is how many commits the scenario's conversation was
		// ingested in, so a pack's commit count has a denominator: how much of
		// the conversation the 30 slots actually range over.
		scenarioCommits int
	}
	byCategory := map[int][]sample{}
	for _, scenario := range scenarios {
		commitByTurn := map[string]string{}
		commitKeys := map[string]struct{}{}
		for _, turn := range scenario.Turns {
			commitKeys[turn.IdempotencyKey] = struct{}{}
			for _, id := range turn.DatasetIDs {
				if trimmed := strings.TrimSpace(id); trimmed != "" {
					commitByTurn[trimmed] = turn.IdempotencyKey
				}
			}
		}
		for _, question := range scenario.Questions {
			if len(question.Evidence) == 0 || len(byCategory[question.Category]) >= perCategory {
				continue
			}
			evidence := map[string]struct{}{}
			for _, id := range question.Evidence {
				evidence[id] = struct{}{}
			}
			byCategory[question.Category] = append(byCategory[question.Category],
				sample{
					scenario: scenario, question: question,
					commitByTurn: commitByTurn, evidence: evidence,
					scenarioCommits: len(commitKeys),
				})
		}
	}
	var work []sample
	for _, category := range []int{1, 2, 3, 4} {
		work = append(work, byCategory[category]...)
	}

	var (
		mu                                             sync.Mutex
		items, commitTotal, unattributed               int
		untagged, spanning                             int
		unattributedKinds                              = map[corememory.ContextItemKind]int{}
		evidenceItems, evidenceQ                       int
		questions, coveredEvidenceTurns, totalEvidence int
		commitsPerPack, scenarioCommits                []int
	)
	var next atomic.Int64
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(work) {
					return
				}
				current := work[index]
				result, err := built.Memory.Context(context.Background(), corememory.ContextRequest{
					Scope: scope, ConversationID: current.scenario.ConversationID,
					Query:  current.question.Query,
					Budget: corememory.Budget{MaxItems: maxItems, MaxTokens: maxItems * 205},
				})
				if err != nil {
					continue
				}
				ctx := context.Background()
				matcher := eval.NewMatcher(resolver)
				localCommits := map[string]struct{}{}
				covered := map[string]struct{}{}
				localUnattributedKinds := map[corememory.ContextItemKind]int{}
				localItems, localUnattributed, localUntagged, localEvidenceItems, localSpanning := 0, 0, 0, 0, 0
				for _, item := range result.Items {
					localItems++
					sources := matcher.Sources(ctx, item)
					attributed := false
					itemCommits := map[string]struct{}{}
					for _, source := range sources {
						if source.TurnID == "" {
							localUntagged++
							continue
						}
						commit := current.commitByTurn[source.TurnID]
						if commit == "" {
							continue
						}
						attributed = true
						itemCommits[commit] = struct{}{}
						localCommits[commit] = struct{}{}
					}
					if !attributed {
						localUnattributed++
						localUnattributedKinds[item.Kind]++
					}
					if len(itemCommits) > 1 {
						localSpanning++
					}
					// Evidence keeps the narrower rule the other probes use: an
					// item counts once, for the first dataset turn of the
					// question it stands for.
					for _, source := range sources {
						if source.TurnID == "" {
							continue
						}
						if _, isEvidence := current.evidence[source.TurnID]; isEvidence {
							localEvidenceItems++
							covered[source.TurnID] = struct{}{}
							break
						}
					}
				}
				mu.Lock()
				items += localItems
				commitTotal += len(localCommits)
				commitsPerPack = append(commitsPerPack, len(localCommits))
				scenarioCommits = append(scenarioCommits, current.scenarioCommits)
				unattributed += localUnattributed
				for kind, count := range localUnattributedKinds {
					unattributedKinds[kind] += count
				}
				untagged += localUntagged
				spanning += localSpanning
				evidenceItems += localEvidenceItems
				questions++
				coveredEvidenceTurns += len(covered)
				totalEvidence += len(current.evidence)
				if len(covered) == len(current.evidence) {
					evidenceQ++
				}
				mu.Unlock()
			}
		}()
	}
	group.Wait()

	t.Logf("max_items=%d questions=%d", maxItems, questions)
	if questions == 0 || commitTotal == 0 {
		t.Fatalf("no pack attributed to a commit: items=%d questions=%d", items, questions)
	}
	sort.Ints(commitsPerPack)
	available := 0
	for _, count := range scenarioCommits {
		available += count
	}
	t.Logf("items packed=%d (mean per pack=%.2f); items whose sources name no dataset turn=%d (%.3f)",
		items, float64(items)/float64(questions),
		unattributed, float64(unattributed)/float64(items))
	if unattributed > 0 {
		t.Logf("unattributed item kinds (an item no turn can be traced to): %v", unattributedKinds)
	}
	t.Logf("source messages with no dataset turn id=%d (%.3f; 0 means every source ingest tagged)",
		untagged, float64(untagged)/float64(items))
	t.Logf("commits drawn on: mean per pack=%.2f p50=%d min=%d max=%d (mean %.1f commits exist per conversation)",
		float64(commitTotal)/float64(questions),
		commitsPerPack[len(commitsPerPack)/2], commitsPerPack[0],
		commitsPerPack[len(commitsPerPack)-1], float64(available)/float64(questions))
	t.Logf("items per commit=%.2f; duplicate share=%.3f (slots spent on a commit the pack already draws on)",
		float64(items)/float64(commitTotal), 1-float64(commitTotal)/float64(items))
	t.Logf("items whose sources span more than one commit=%d (%.3f; nonzero means the commit is not the item's unit)",
		spanning, float64(spanning)/float64(items))
	t.Logf("items carrying the question's evidence=%d (precision=%.3f)",
		evidenceItems, float64(evidenceItems)/float64(items))
	t.Logf("evidence turns covered=%d/%d (recall=%.3f); questions fully covered=%d (%.3f)",
		coveredEvidenceTurns, totalEvidence,
		float64(coveredEvidenceTurns)/float64(totalEvidence),
		evidenceQ, float64(evidenceQ)/float64(questions))
}
